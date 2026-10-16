package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/restore"
	"github.com/unifers/feather/internal/store"
)

// ErrImportBusy is returned when an import is already running in this process.
//
// One at a time is a deliberate limit, not a resource one. An import writes
// into a production cluster on an operator's say-so; two of them racing into
// the same collection would make the counts on both meaningless, and nobody
// has asked for parallel imports.
var ErrImportBusy = errors.New("an import is already running")

// ImportRequest is everything StartImport needs. The web layer validates the
// target and checks the password for a replace before building one.
type ImportRequest struct {
	Actor   string
	ActorIP string

	// SourceKind is store.ImportSourceArchive, ImportSourceRun or
	// ImportSourceUpload. SourceRef is the archive or run id for the first
	// two.
	SourceKind  string
	SourceRef   string
	PreferAlias string

	// The upload, for ImportSourceUpload. StartImport owns UploadPath from the
	// moment it is called: the file is removed when the import finishes, and
	// also when it never starts.
	UploadPath  string
	UploadName  string
	UploadCodec string

	TargetAlias      string
	TargetDatabase   string
	TargetCollection string

	Mode   string // store.ImportModeInsert or store.ImportModeReplace
	DryRun bool
}

// StartImport records an import and runs it in the background, returning its
// id as soon as it has started.
//
// Everything that can be wrong with the request is found before the row is
// written — an unknown archive, an empty run, a MongoDB account that will not
// connect — so the operator gets an error on the form rather than a row that
// failed in the first millisecond.
func (a *App) StartImport(ctx context.Context, req ImportRequest) (id uuid.UUID, err error) {
	if req.SourceKind == store.ImportSourceUpload && req.UploadPath != "" {
		// Ownership of the file passes to the import only once it has
		// started; any earlier return cleans up here.
		defer func() {
			if err != nil {
				_ = os.Remove(req.UploadPath)
			}
		}()
	}

	if req.Mode != store.ImportModeInsert && req.Mode != store.ImportModeReplace {
		return uuid.Nil, fmt.Errorf("unknown import mode %q", req.Mode)
	}

	a.mu.Lock()
	if a.importing {
		a.mu.Unlock()
		return uuid.Nil, ErrImportBusy
	}
	a.importing = true
	a.mu.Unlock()
	release := func() {
		a.mu.Lock()
		a.importing = false
		a.mu.Unlock()
	}
	defer func() {
		if err != nil {
			release()
		}
	}()

	imp := &store.Import{
		Actor: req.Actor, ActorIP: req.ActorIP,
		SourceKind: req.SourceKind, SourceRef: req.SourceRef, PreferAlias: req.PreferAlias,
		TargetAlias: req.TargetAlias, TargetDatabase: req.TargetDatabase, TargetCollection: req.TargetCollection,
		Mode: req.Mode, DryRun: req.DryRun,
	}

	var artifacts []store.ArtifactWithCopies
	switch req.SourceKind {
	case store.ImportSourceArchive, store.ImportSourceRun:
		artifacts, imp.SourceLabel, err = a.importArtifacts(ctx, req.SourceKind, req.SourceRef)
		if err != nil {
			return uuid.Nil, err
		}
		// An import writes into MongoDB. PostgreSQL rows would go in as
		// documents with fresh ids — a copy nobody asked for, in a database
		// they never came from — so they are sent to the restore that puts
		// them back in their table.
		for _, item := range artifacts {
			if item.IsPostgres() {
				return uuid.Nil, fmt.Errorf("%s holds PostgreSQL rows, and imports write into MongoDB — "+
					"put them back in their table with: feather restore %s", item.FileName, req.SourceRef)
			}
		}
		imp.FilesTotal = len(artifacts)
		for _, item := range artifacts {
			imp.DocsExpected += item.DocCount
			imp.BytesExpected += item.BytesCompressed
		}
	case store.ImportSourceUpload:
		info, statErr := os.Stat(req.UploadPath)
		if statErr != nil {
			return uuid.Nil, fmt.Errorf("the uploaded file is gone: %w", statErr)
		}
		imp.SourceLabel = req.UploadName
		imp.FilesTotal = 1
		imp.BytesExpected = info.Size()
	default:
		return uuid.Nil, fmt.Errorf("unknown import source %q", req.SourceKind)
	}

	// The target is resolved now rather than in the background so that an
	// account that cannot connect is a message on the form. A dry run writes
	// nothing, so it does not need one.
	var into *mongo.Collection
	if !req.DryRun {
		connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		client, connectErr := a.Mongo.Get(connectCtx, req.TargetAlias)
		if connectErr != nil {
			return uuid.Nil, connectErr
		}
		into, err = client.Collection(req.TargetDatabase, req.TargetCollection, "")
		if err != nil {
			return uuid.Nil, err
		}
	}

	if err := a.Store.StartImport(ctx, imp); err != nil {
		return uuid.Nil, err
	}

	a.inFlight.Add(1)
	go func() {
		defer a.inFlight.Done()
		defer release()
		if req.SourceKind == store.ImportSourceUpload {
			defer func() { _ = os.Remove(req.UploadPath) }()
		}
		a.runImport(imp, req, artifacts, into)
	}()
	return imp.ID, nil
}

// importArtifacts resolves a catalogue source to the archives it names, oldest
// batch first for a run — the order they were written in.
func (a *App) importArtifacts(ctx context.Context, kind, ref string) ([]store.ArtifactWithCopies, string, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return nil, "", fmt.Errorf("%q is not an archive or run id", ref)
	}
	if kind == store.ImportSourceArchive {
		item, err := a.Store.ArtifactByID(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return []store.ArtifactWithCopies{item}, item.FileName, nil
	}

	found, err := a.Store.ArtifactsForRun(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if len(found) == 0 {
		return nil, "", fmt.Errorf("run %s wrote no archives, so there is nothing to import", ref)
	}
	label := fmt.Sprintf("%d archive(s) from run %s of %s", len(found), shortID(ref), found[0].ConfigName)
	return found, label, nil
}

// importProgressEvery bounds how often a running import writes its counters.
// Every batch would be a write per thousand documents into the one SQLite
// connection the scheduler also needs; the detail page refreshes every few
// seconds anyway.
const importProgressEvery = 2 * time.Second

// runImport does the work and closes the row. It runs on the base context, so a
// browser navigating away does not stop it, and Close waits for it.
func (a *App) runImport(imp *store.Import, req ImportRequest, artifacts []store.ArtifactWithCopies, into *mongo.Collection) {
	ctx := a.baseCtx
	log := a.Log.With("import", imp.ID.String())

	mode := restore.ModeInsert
	if req.Mode == store.ImportModeReplace {
		mode = restore.ModeReplace
	}

	// done holds the totals of every finished file; the progress callback adds
	// the file in flight on top.
	done := *imp
	lastWrite := time.Now()
	progress := func(current restore.Result) {
		if time.Since(lastWrite) < importProgressEvery {
			return
		}
		lastWrite = time.Now()
		snapshot := done
		addResult(&snapshot, current)
		if err := a.Store.UpdateImportProgress(ctx, &snapshot); err != nil {
			log.Warn("could not record import progress", "error", err)
		}
	}

	log.Info("import started", "source", imp.SourceLabel, "target", imp.Target(),
		"account", imp.TargetAlias, "mode", imp.Mode, "dry_run", imp.DryRun, "by", imp.Actor)

	var runErr error
	switch req.SourceKind {
	case store.ImportSourceUpload:
		runErr = a.importUpload(ctx, log, &done, req, into, mode, progress)
	default:
		for _, item := range artifacts {
			// A preference, not a requirement: in a run, an archive written
			// before that destination was added has no copy there.
			prefer := ""
			for _, copy := range item.Copies {
				if copy.Alias == req.PreferAlias {
					prefer = req.PreferAlias
				}
			}
			result, err := restore.Artifact(ctx, log, a.Registry, into, item.Artifact, item.Copies, restore.Options{
				DryRun: req.DryRun, PreferAlias: prefer, Mode: mode, Progress: progress,
			})
			addResult(&done, result)
			if err != nil {
				runErr = fmt.Errorf("%s: %w", item.FileName, err)
				break
			}
			done.FilesDone++
			if len(artifacts) == 1 {
				done.SHA256 = result.SHA256
			}
		}
	}

	switch {
	case runErr != nil:
		done.Status = store.ImportFailed
		done.Error = config.RedactSecrets(friendlyImportError(runErr, imp).Error())
	case imp.DryRun:
		done.Status = store.ImportDryRun
	default:
		done.Status = store.ImportSuccess
	}

	// The row and the audit entry are written on a context of their own: the
	// import is over, and recording that it is over must not depend on
	// anything that might have been cancelled while it ran.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := a.Store.FinishImport(finishCtx, &done); err != nil {
		log.Error("could not close the import row", "error", err)
	}
	if err := a.Store.RecordAudit(finishCtx, store.AuditEvent{
		Actor: imp.Actor, ActorIP: imp.ActorIP, Action: store.AuditImportFinished,
		TargetKind: "import", Target: imp.ID.String(), Detail: importOutcome(&done),
	}); err != nil {
		log.Error("could not record an audit event", "action", store.AuditImportFinished, "error", err)
	}

	if runErr != nil {
		log.Error("import failed", "error", done.Error, "read", done.DocsRead, "inserted", done.DocsInserted)
		return
	}
	log.Info("import finished", "read", done.DocsRead, "inserted", done.DocsInserted,
		"already_present", done.DocsPresent, "replaced", done.DocsReplaced, "dry_run", imp.DryRun)
}

// importUpload streams an uploaded file into the target.
func (a *App) importUpload(ctx context.Context, log *slog.Logger, done *store.Import, req ImportRequest, into *mongo.Collection, mode restore.Mode, progress func(restore.Result)) error {
	file, err := os.Open(req.UploadPath)
	if err != nil {
		return fmt.Errorf("opening the uploaded file: %w", err)
	}
	defer func() { _ = file.Close() }()

	result, err := restore.Stream(ctx, log, into, file, req.UploadCodec, restore.Options{
		DryRun: req.DryRun, Mode: mode, Progress: progress,
	})
	addResult(done, result)
	done.SHA256 = result.SHA256
	if err != nil {
		return err
	}
	done.FilesDone = 1
	return nil
}

// addResult folds one file's counts into an import's totals.
func addResult(imp *store.Import, result restore.Result) {
	imp.DocsRead += result.DocumentsRead
	imp.DocsInserted += result.Inserted
	imp.DocsPresent += result.AlreadyPresent
	imp.DocsReplaced += result.Replaced
	imp.BytesRead += result.BytesRead
}

// friendlyImportError rewrites the failure an operator is most likely to hit —
// an account with read-only access — into what to do about it. The driver's
// own wording ("not authorized on shop to execute command { insert: … }") is
// kept after it, since it names the exact privilege.
func friendlyImportError(err error, imp *store.Import) error {
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "not authorized") || strings.Contains(text, "unauthorized") {
		return fmt.Errorf("the MongoDB account %q is not allowed to write to %s — give it the readWrite role on %s, or import through another account (%w)",
			imp.TargetAlias, imp.Target(), imp.TargetDatabase, err)
	}
	return err
}

// importOutcome is the one-line audit detail for a finished import.
func importOutcome(imp *store.Import) string {
	outcome := fmt.Sprintf("%s: %d read, %d inserted, %d already present", imp.Status,
		imp.DocsRead, imp.DocsInserted, imp.DocsPresent)
	if imp.Mode == store.ImportModeReplace {
		outcome += fmt.Sprintf(", %d replaced", imp.DocsReplaced)
	}
	outcome += " into " + imp.Target() + " on " + imp.TargetAlias
	if imp.Error != "" {
		outcome += " — " + imp.Error
	}
	return outcome
}

// ImportRunning reports whether this process has an import in flight.
func (a *App) ImportRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.importing
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
