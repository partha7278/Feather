package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

// maxImportUploadBytes bounds a file uploaded for import. Large enough for a
// month of a busy collection compressed, small enough that one upload cannot
// fill the spool disk the archives also need. Anything bigger belongs in a
// destination, where it can be imported from the catalogue.
const maxImportUploadBytes = 256 << 20

// importUploadTimeout replaces the server's two-minute read timeout for an
// upload: 256 MiB in two minutes needs a faster link than an operator at home
// can count on.
const importUploadTimeout = 30 * time.Minute

// importFormState is what the import form renders from, both fresh and when a
// rejected submission is shown back.
type importFormState struct {
	SourceKind  string
	SourceRef   string
	SourceLabel string
	Artifacts   []store.ArtifactWithCopies
	Copies      []importCopy
	PreferAlias string

	// Where the documents were archived from, for the summary.
	ConfigName    string
	Origin        string
	DocsExpected  int64
	BytesExpected int64

	TargetAlias      string
	TargetDatabase   string
	TargetCollection string
	Mode             string

	// LoopConfig names the enabled config that archives and deletes from the
	// target, once the form has been checked. The form shows the tick box
	// that acknowledges it only when it is set.
	LoopConfig  string
	Acknowledge bool
}

// importCopy is one destination a catalogue import can read from.
type importCopy struct {
	Alias    string
	Readable bool
}

func (f importFormState) Upload() bool { return f.SourceKind == store.ImportSourceUpload }

func (s *Server) handleImportList(w http.ResponseWriter, r *http.Request) {
	imports, err := s.app.Store.Imports(r.Context(), 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Only while something is moving, as on a run's page.
	refresh := 0
	for _, imp := range imports {
		if imp.FinishedAt == nil {
			refresh = 5
			break
		}
	}

	s.render(w, r, "imports", pageData{
		Title:   "Imports",
		Nav:     "imports",
		Refresh: refresh,
		Data: map[string]any{
			"Imports": imports,
			"Busy":    s.app.ImportRunning(),
		},
	})
}

// handleImportForm shows a new import, prefilled from the archive or run it was
// started from. With neither it is an upload.
//
// The defaults are chosen so that accepting them is safe: the same account and
// database the archive came from, but a new collection named for today, and a
// mode that never overwrites. Restoring over the original collection is
// possible, and is a decision the operator has to type.
func (s *Server) handleImportForm(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state := importFormState{SourceKind: store.ImportSourceUpload, Mode: store.ImportModeInsert}
	switch {
	case query.Get("archive") != "":
		state.SourceKind, state.SourceRef = store.ImportSourceArchive, query.Get("archive")
	case query.Get("run") != "":
		state.SourceKind, state.SourceRef = store.ImportSourceRun, query.Get("run")
	}

	if !state.Upload() {
		if err := s.loadImportSource(r.Context(), &state); err != nil {
			s.notFoundOrError(w, r, err)
			return
		}
		first := state.Artifacts[0]
		state.TargetDatabase = first.Database
		state.TargetCollection = first.Collection + "_restored_" + time.Now().Format("20060102")
		// The account the config reads through is the cluster these
		// documents came from. If the config is gone, nothing is guessed.
		if record, err := s.app.Store.JobByName(r.Context(), first.ConfigName); err == nil {
			state.TargetAlias = record.Config.Source.MongoSource
		}
	}

	s.renderImportForm(w, r, state, nil)
}

// loadImportSource fills in the catalogue half of the form: which archives,
// how many documents, and which destinations hold a copy.
func (s *Server) loadImportSource(ctx context.Context, state *importFormState) error {
	id, err := uuid.Parse(state.SourceRef)
	if err != nil {
		return store.ErrNotFound
	}
	if state.SourceKind == store.ImportSourceArchive {
		item, err := s.app.Store.ArtifactByID(ctx, id)
		if err != nil {
			return err
		}
		state.Artifacts = []store.ArtifactWithCopies{item}
		state.SourceLabel = item.FileName
	} else {
		found, err := s.app.Store.ArtifactsForRun(ctx, id)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return store.ErrNotFound
		}
		state.Artifacts = found
		state.SourceLabel = fmt.Sprintf("%d archive(s) from one run", len(found))
	}

	first := state.Artifacts[0]
	state.ConfigName = first.ConfigName
	state.Origin = first.Database + "." + first.Collection
	state.DocsExpected, state.BytesExpected = 0, 0

	seen := map[string]bool{}
	for _, item := range state.Artifacts {
		state.DocsExpected += item.DocCount
		state.BytesExpected += item.BytesCompressed
		for _, copy := range item.Copies {
			if seen[copy.Alias] {
				continue
			}
			seen[copy.Alias] = true
			_, err := s.app.Registry.Get(copy.Alias)
			state.Copies = append(state.Copies, importCopy{Alias: copy.Alias, Readable: err == nil})
		}
	}
	sort.SliceStable(state.Copies, func(i, j int) bool {
		return state.Copies[i].Readable && !state.Copies[j].Readable
	})
	if state.PreferAlias == "" {
		for _, copy := range state.Copies {
			if copy.Readable {
				state.PreferAlias = copy.Alias
				break
			}
		}
	}
	return nil
}

func (s *Server) renderImportForm(w http.ResponseWriter, r *http.Request, state importFormState, problem *flash) {
	sources, err := s.app.Store.ListMongoSources(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	title := "New import"
	if state.Upload() {
		title = "Import a file"
	}
	s.render(w, r, "import_form", pageData{
		Title: title,
		Nav:   "imports",
		Flash: problem,
		Data: map[string]any{
			"Form":      state,
			"Sources":   sources,
			"MaxUpload": int64(maxImportUploadBytes),
			"Busy":      s.app.ImportRunning(),
		},
	})
}

// readImportTarget takes the target and mode half of a posted form.
func readImportTarget(f *form, state *importFormState) {
	state.TargetAlias = f.String("targetAlias")
	state.TargetDatabase = f.String("targetDatabase")
	state.TargetCollection = f.String("targetCollection")
	state.Mode = store.ImportModeInsert
	if f.String("mode") == store.ImportModeReplace {
		state.Mode = store.ImportModeReplace
	}
	state.Acknowledge = f.Bool("acknowledge")
}

// handleImportStart starts an import from the catalogue.
func (s *Server) handleImportStart(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	state := importFormState{
		SourceKind:  f.String("sourceKind"),
		SourceRef:   f.String("sourceRef"),
		PreferAlias: f.String("preferAlias"),
	}
	if state.SourceKind != store.ImportSourceArchive && state.SourceKind != store.ImportSourceRun {
		http.Error(w, "Unknown import source.", http.StatusBadRequest)
		return
	}
	if err := s.loadImportSource(r.Context(), &state); err != nil {
		s.notFoundOrError(w, r, err)
		return
	}
	readImportTarget(f, &state)

	s.startImport(w, r, f, state, app.ImportRequest{
		SourceKind: state.SourceKind, SourceRef: state.SourceRef, PreferAlias: state.PreferAlias,
	})
}

// handleImportUpload starts an import from a file posted with the form.
//
// The body is capped by limitBody before the CSRF check parses it, and the file
// is copied into the spool directory — not left wherever the multipart parser
// put it — so the import goroutine can read it after this request has ended,
// and so a crash leaves it where CleanSpoolDir looks.
func (s *Server) handleImportUpload(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}

	state := importFormState{SourceKind: store.ImportSourceUpload}
	readImportTarget(f, &state)

	file, header, err := r.FormFile("file")
	if err != nil {
		s.renderImportForm(w, r, state, &flash{Kind: "error", Message: "Choose a file to import."})
		return
	}
	defer func() { _ = file.Close() }()

	name := filepath.Base(header.Filename)
	codec, err := archive.CodecForUpload(name)
	if err != nil {
		s.renderImportForm(w, r, state, &flash{Kind: "error", Message: err.Error()})
		return
	}
	state.SourceLabel = name

	// Everything that can reject the form is checked before the file is
	// copied anywhere.
	if !s.checkImportForm(w, r, f, &state) {
		return
	}

	path, err := s.spoolUpload(file)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.launchImport(w, r, state, app.ImportRequest{
		SourceKind: store.ImportSourceUpload,
		UploadPath: path, UploadName: name, UploadCodec: codec,
	}, f.String("action") != "import")
}

// spoolUpload copies an uploaded file into SpoolDir/imports. The name ends in
// .tmp so CleanSpoolDir removes it if feather dies before the import does.
func (s *Server) spoolUpload(file io.Reader) (string, error) {
	dir := filepath.Join(s.app.Env.SpoolDir, "imports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	out, err := os.CreateTemp(dir, "upload-*.tmp")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, file); err != nil {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return "", fmt.Errorf("saving the upload: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// startImport finishes a catalogue import's submission: checks, then launch.
func (s *Server) startImport(w http.ResponseWriter, r *http.Request, f *form, state importFormState, req app.ImportRequest) {
	if !s.checkImportForm(w, r, f, &state) {
		return
	}
	s.launchImport(w, r, state, req, f.String("action") != "import")
}

// checkImportForm validates the target, the retention-loop acknowledgement and,
// for a real replace, the password. It renders the form back and reports false
// when anything is wrong, so nothing has been recorded or written yet.
func (s *Server) checkImportForm(w http.ResponseWriter, r *http.Request, f *form, state *importFormState) bool {
	if problems := validateImportTarget(state.TargetAlias, state.TargetDatabase, state.TargetCollection); len(problems) > 0 {
		s.renderImportForm(w, r, *state, &flash{Kind: "error", Message: strings.Join(problems, " ")})
		return false
	}
	known, err := s.app.Store.MongoSourceMap(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return false
	}
	if _, ok := known[state.TargetAlias]; !ok {
		s.renderImportForm(w, r, *state, &flash{Kind: "error",
			Message: fmt.Sprintf("There is no MongoDB account called %q.", state.TargetAlias)})
		return false
	}

	if !s.checkRetentionLoop(w, r, state) {
		return false
	}

	dryRun := f.String("action") != "import"
	if state.Mode == store.ImportModeReplace && !dryRun {
		_, user, _ := sessionFrom(r)
		if err := s.app.Store.CheckPassword(r.Context(), user.ID, f.Raw("password")); err != nil {
			s.log.Warn("replace import refused: wrong password", "email", user.Email, "ip", s.clientIP(r),
				"target", state.TargetDatabase+"."+state.TargetCollection)
			s.renderImportForm(w, r, *state, &flash{Kind: "error", Message: "Password incorrect, nothing was imported."})
			return false
		}
	}
	return true
}

// checkRetentionLoop refuses, until acknowledged, an import into a collection
// that an enabled config archives and deletes from. The documents are going
// back into the place they were deleted from, and unless the filter has moved
// on they will be archived and deleted again on the next run.
func (s *Server) checkRetentionLoop(w http.ResponseWriter, r *http.Request, state *importFormState) bool {
	loop, err := s.retentionLoop(r.Context(), state.TargetAlias, state.TargetDatabase, state.TargetCollection)
	if err != nil {
		s.serverError(w, r, err)
		return false
	}
	state.LoopConfig = loop
	if loop == "" || state.Acknowledge {
		return true
	}
	s.renderImportForm(w, r, *state, &flash{Kind: "error", Message: fmt.Sprintf(
		"The config %q archives and then deletes documents from %s.%s. Imported documents that still match its filter "+
			"will be deleted again on its next run. Tick the box to confirm this is what you want.",
		loop, state.TargetDatabase, state.TargetCollection)})
	return false
}

// retentionLoop names the enabled, deleting config that reads from a target,
// if there is one.
func (s *Server) retentionLoop(ctx context.Context, alias, database, collection string) (string, error) {
	jobs, err := s.app.Store.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	for _, job := range jobs {
		cfg := job.Config
		if cfg == nil || !cfg.IsEnabled() || !cfg.Retention.DeleteAfterArchive {
			continue
		}
		if cfg.Source.MongoSource == alias && cfg.Source.Database == database && cfg.Source.Collection == collection {
			return cfg.Name, nil
		}
	}
	return "", nil
}

// launchImport hands a checked request to the app, records it, and sends the
// operator to its page.
func (s *Server) launchImport(w http.ResponseWriter, r *http.Request, state importFormState, req app.ImportRequest, dryRun bool) {
	req.Actor = currentUserEmail(r)
	req.ActorIP = s.clientIP(r)
	req.TargetAlias = state.TargetAlias
	req.TargetDatabase = state.TargetDatabase
	req.TargetCollection = state.TargetCollection
	req.Mode = state.Mode
	req.DryRun = dryRun

	// Not the request's context: StartImport connects to MongoDB and writes the
	// row, and neither should be abandoned half way because the browser gave
	// up waiting.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()

	id, err := s.app.StartImport(ctx, req)
	if err != nil {
		if errors.Is(err, app.ErrImportBusy) {
			s.setFlash(w, "error", "An import is already running. Wait for it to finish.")
			http.Redirect(w, r, "/imports", http.StatusSeeOther)
			return
		}
		s.log.Warn("import could not start", "error", config.RedactSecrets(err.Error()))
		message := config.RedactSecrets(err.Error())
		if errors.Is(err, store.ErrNotFound) {
			message = "That archive or run is no longer in the catalogue."
		}
		s.renderImportForm(w, r, state, &flash{Kind: "error", Message: "The import did not start: " + message})
		return
	}

	action := store.AuditImportStarted
	if req.Mode == store.ImportModeReplace && !dryRun {
		action = store.AuditImportReplaceStarted
	}
	s.audit(r, action, "import", id.String(), importAuditDetail(state, req))

	if dryRun {
		s.setFlash(w, "ok", "Dry run started. Nothing will be written to MongoDB.")
	} else {
		s.setFlash(w, "ok", "Import started into %s.%s on %s.", req.TargetDatabase, req.TargetCollection, req.TargetAlias)
	}
	http.Redirect(w, r, "/imports/"+id.String(), http.StatusSeeOther)
}

// importAuditDetail is the who-what-where line for an import's start.
func importAuditDetail(state importFormState, req app.ImportRequest) string {
	source := state.SourceLabel
	if req.SourceKind == store.ImportSourceUpload {
		source = "uploaded file " + req.UploadName
	}
	detail := fmt.Sprintf("%s into %s.%s on %s, mode %s", source,
		req.TargetDatabase, req.TargetCollection, req.TargetAlias, req.Mode)
	if req.DryRun {
		detail += ", dry run"
	}
	if state.LoopConfig != "" {
		detail += fmt.Sprintf(", acknowledged that config %s deletes from this collection", state.LoopConfig)
	}
	return detail
}

func (s *Server) handleImportDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	imp, err := s.app.Store.ImportByID(r.Context(), id)
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}

	refresh := 0
	if imp.FinishedAt == nil {
		refresh = 5
	}

	// "Import for real" is offered for a dry run of the catalogue that read
	// everything cleanly. It carries the retention-loop tick when the target
	// needs one, so the warning is not skipped by going this way round.
	canRerun := imp.DryRun && imp.Status == store.ImportDryRun && imp.SourceKind != store.ImportSourceUpload
	loop := ""
	if canRerun {
		if loop, err = s.retentionLoop(r.Context(), imp.TargetAlias, imp.TargetDatabase, imp.TargetCollection); err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	s.render(w, r, "import_detail", pageData{
		Title:   "Import " + imp.SourceLabel,
		Nav:     "imports",
		Refresh: refresh,
		Data: map[string]any{
			"Import":     imp,
			"CanRerun":   canRerun,
			"LoopConfig": loop,
		},
	})
}

// handleImportRerun runs a finished catalogue dry run for real, with the same
// source, target and mode.
func (s *Server) handleImportRerun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	imp, err := s.app.Store.ImportByID(r.Context(), id)
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	back := "/imports/" + imp.ID.String()
	if !imp.DryRun || imp.Status != store.ImportDryRun || imp.SourceKind == store.ImportSourceUpload {
		s.setFlash(w, "error", "Only a finished dry run of a catalogued archive can be run for real.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	state := importFormState{
		SourceKind: imp.SourceKind, SourceRef: imp.SourceRef, PreferAlias: imp.PreferAlias,
		TargetAlias: imp.TargetAlias, TargetDatabase: imp.TargetDatabase, TargetCollection: imp.TargetCollection,
		Mode: imp.Mode, Acknowledge: f.Bool("acknowledge"),
	}
	if err := s.loadImportSource(r.Context(), &state); err != nil {
		s.setFlash(w, "error", "That archive or run is no longer in the catalogue.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	// The same checks as the form, answered on the dry run's own page.
	loop, err := s.retentionLoop(r.Context(), state.TargetAlias, state.TargetDatabase, state.TargetCollection)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	state.LoopConfig = loop
	if loop != "" && !state.Acknowledge {
		s.setFlash(w, "error", "The config %q deletes from %s.%s. Tick the box to confirm the import anyway.",
			loop, state.TargetDatabase, state.TargetCollection)
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if state.Mode == store.ImportModeReplace {
		_, user, _ := sessionFrom(r)
		if err := s.app.Store.CheckPassword(r.Context(), user.ID, f.Raw("password")); err != nil {
			s.log.Warn("replace import refused: wrong password", "email", user.Email, "ip", s.clientIP(r),
				"target", imp.Target())
			s.setFlash(w, "error", "Password incorrect, nothing was imported.")
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
	}

	s.launchImport(w, r, state, app.ImportRequest{
		SourceKind: state.SourceKind, SourceRef: state.SourceRef, PreferAlias: state.PreferAlias,
	}, false)
}

// validateImportTarget applies MongoDB's naming rules, plus feather's own: the
// server's internal databases and system collections are never a target.
func validateImportTarget(alias, database, collection string) []string {
	var problems []string
	if alias == "" {
		problems = append(problems, "Choose a MongoDB account.")
	}

	switch {
	case database == "":
		problems = append(problems, "Database is required.")
	case strings.ContainsAny(database, "/\\. \"$\x00"):
		problems = append(problems, `A database name cannot contain / \ . " $, spaces or NUL.`)
	case len(database) > 64:
		problems = append(problems, "A database name is at most 64 bytes.")
	default:
		switch strings.ToLower(database) {
		case "admin", "local", "config":
			problems = append(problems, fmt.Sprintf("%q is one of MongoDB's own databases and cannot be imported into.", database))
		}
	}

	switch {
	case collection == "":
		problems = append(problems, "Collection is required.")
	case strings.ContainsAny(collection, "$\x00"):
		problems = append(problems, "A collection name cannot contain $ or NUL.")
	case strings.HasPrefix(collection, "system."):
		problems = append(problems, "Collections starting with system. belong to MongoDB and cannot be imported into.")
	case len(database)+1+len(collection) > 255:
		problems = append(problems, "The database and collection names together are too long for MongoDB.")
	}
	return problems
}

// limitUpload caps an import upload and gives it a read deadline long enough
// to arrive.
//
// A body declared too large is answered before anything reads it, with a page
// that says why. Without this, the cap would surface as the CSRF check failing
// on a truncated form — an "expired form" message for a file that was simply
// too big.
func (s *Server) limitUpload(limit int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			s.setFlash(w, "error", "That file is larger than %s, the most that can be uploaded. "+
				"Copy it to a destination and import it from Backups instead.", config.HumanBytes(limit))
			http.Redirect(w, r, "/imports/new", http.StatusSeeOther)
			return
		}
		// Not supported by every ResponseWriter — a test recorder, for one —
		// and then the server's own timeout stands.
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now().Add(importUploadTimeout))
		_ = controller.SetWriteDeadline(time.Now().Add(importUploadTimeout + time.Minute))
		limitBody(limit, next)(w, r)
	}
}
