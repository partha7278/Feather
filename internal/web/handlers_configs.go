package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/pgsrc"
	"github.com/unifers/feather/internal/store"
)

func (s *Server) handleConfigList(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.app.Store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	latest, err := s.app.Store.LatestRuns(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	leases, err := s.app.Store.RunningConfigs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "config_list", pageData{
		Title: "Retention configs",
		Nav:   "configs",
		Data: map[string]any{
			"Jobs":     jobs,
			"Latest":   latest,
			"Running":  leases,
			"NextRuns": s.app.Scheduler.NextRuns(),
		},
	})
}

func (s *Server) handleConfigForm(w http.ResponseWriter, r *http.Request) {
	sources, err := s.app.Store.ListMongoSources(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pgSources, err := s.app.Store.ListPostgresSources(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	destinations, err := s.app.Store.ListDestinations(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	record := store.JobRecord{Config: newJobTemplate()}
	title := "New retention config"

	if id := r.PathValue("id"); id != "" {
		found, err := s.app.Store.JobByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		record = found
		title = "Config: " + record.Config.Name
	}

	s.renderConfigForm(w, r, configFormState{
		record:       record,
		title:        title,
		sources:      sources,
		pgSources:    pgSources,
		destinations: destinations,
		filter:       filterText(record.Config),
		projection: func() string {
			if len(record.Config.Source.Projection) == 0 {
				return ""
			}
			return prettyJSON(string(record.Config.Source.Projection))
		}(),
		scheduleKind: scheduleKind(record.Config),
		maxBytes:     record.Config.Batch.MaxBytes.String(),
		maxRuntime:   record.Config.Limits.MaxRuntime.Std().String(),
	})
}

// configFormState is everything the config form shows. The free-text fields
// are carried as strings rather than derived from the config, so a rejected
// submission can be shown back exactly as it was typed — an invalid filter
// cannot be parsed into the config, and it is precisely the field the
// operator needs to see again to fix.
type configFormState struct {
	record       store.JobRecord
	title        string
	sources      []store.MongoSourceRecord
	pgSources    []store.PostgresSourceRecord
	destinations []store.DestinationRecord
	filter       string
	projection   string
	scheduleKind string
	maxBytes     string
	maxRuntime   string
	flash        *flash
}

func (s *Server) renderConfigForm(w http.ResponseWriter, r *http.Request, state configFormState) {
	job := state.record.Config
	s.render(w, r, "config_form", pageData{
		Title: state.title,
		Nav:   "configs",
		Flash: state.flash,
		Data: map[string]any{
			"Record":          state.record,
			"Job":             job,
			"Sources":         state.sources,
			"PostgresSources": state.pgSources,
			"Destinations":    state.destinations,
			"KeyPreview":      job.PreviewKey(time.Now()),
			"FilterText":      state.filter,
			"ProjectionJSON":  state.projection,
			"ScheduleKind":    state.scheduleKind,
			"MaxBytes":        state.maxBytes,
			"MaxRuntime":      state.maxRuntime,
		},
	})
}

// rerenderConfigForm shows a rejected submission back with its error, keeping
// every field as the operator typed it. Redirecting instead reloads the form
// from the database — or the blank template, for a new config — and throws
// away everything they had entered.
func (s *Server) rerenderConfigForm(w http.ResponseWriter, r *http.Request, f *form, id string, job *config.JobConfig, message string) {
	sources, err := s.app.Store.ListMongoSources(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pgSources, err := s.app.Store.ListPostgresSources(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	destinations, err := s.app.Store.ListDestinations(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	record := store.JobRecord{ID: id, Config: job}
	title := "New retention config"
	if id != "" {
		existing, err := s.app.Store.JobByID(r.Context(), id)
		if err != nil {
			s.notFoundOrError(w, r, err)
			return
		}
		existing.Config = job
		record = existing
		title = "Config: " + existing.Config.Name
	}

	kind := f.String("scheduleKind")
	if kind != "cron" {
		kind = "every"
	}
	s.renderConfigForm(w, r, configFormState{
		record:       record,
		title:        title,
		sources:      sources,
		pgSources:    pgSources,
		destinations: destinations,
		filter:       f.Raw("filter"),
		projection:   f.Raw("projection"),
		scheduleKind: kind,
		maxBytes:     f.String("batchMaxBytes"),
		maxRuntime:   f.String("maxRuntime"),
		flash:        &flash{Kind: "error", Message: message},
	})
}

// newJobTemplate is what a blank form starts from: safe defaults, deletion off.
// Turning archiving into deletion has to be a deliberate act.
func newJobTemplate() *config.JobConfig {
	enabled := true
	job := &config.JobConfig{
		Enabled: &enabled,
		Source: config.Source{
			Filter: json.RawMessage(`{
  "createdAt": { "$lt": { "$date": "{{now-90d}}" } }
}`),
			ReadPreference: "primary",
		},
		Schedule:  config.Schedule{Every: "1d", At: "02:30", Timezone: "UTC"},
		Retention: config.Retention{DeleteAfterArchive: false},
	}
	// Fill in every other default so the form shows real numbers rather than
	// empty boxes the operator has to guess at.
	_ = job.Prepare()
	return job
}

func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	id := r.PathValue("id")
	job, parseErr := jobFromForm(f)
	if parseErr != nil {
		s.rerenderConfigForm(w, r, f, id, job, parseErr.Error())
		return
	}

	record := store.JobRecord{ID: id, Config: job}
	if err := s.app.Store.SaveJob(r.Context(), &record); err != nil {
		s.rerenderConfigForm(w, r, f, id, job, err.Error())
		return
	}
	if err := s.reload(r); err != nil {
		s.setFlash(w, "error", "Saved, but the schedule could not be reloaded: %s", err)
		http.Redirect(w, r, "/configs", http.StatusSeeOther)
		return
	}

	s.audit(r, store.AuditConfigSaved, "config", job.Name, auditConfigDetail(job))
	if job.Retention.DeleteAfterArchive {
		s.setFlash(w, "ok", "Saved %q. Matching records will be deleted from %s once every copy is stored and verified.", job.Name, engineLabel(job))
	} else {
		s.setFlash(w, "ok", "Saved %q. Matching records will be copied and left in place.", job.Name)
	}
	http.Redirect(w, r, "/configs", http.StatusSeeOther)
}

// jobFromForm builds a retention config out of the posted fields. Validation
// against the accounts that exist happens in the store; this is only about
// turning strings into the right shapes. The config is returned even with an
// error, so the form can be shown back with what was submitted.
func jobFromForm(f *form) (*config.JobConfig, error) {
	enabled := f.Bool("enabled")
	verify := f.Bool("verifyBeforeDelete")

	engine, alias := sourceAccountFromForm(f)
	job := &config.JobConfig{
		Name:    f.Require("name"),
		Enabled: &enabled,
		Source: config.Source{
			Collection:     f.Require("collection"),
			ReadPreference: f.String("readPreference"),
			TimestampField: f.String("timestampField"),
		},
		Batch: config.Batch{
			MaxDocs:   f.Int("batchMaxDocs", config.DefaultBatchMaxDocs),
			FetchSize: int32(f.Int("fetchSize", 1000)),
		},
		Compression: config.Compression{
			Codec: f.String("codec"),
			Level: f.Int("codecLevel", 6),
		},
		Output: config.Output{
			Format:      config.FormatNDJSON,
			KeyTemplate: f.Require("keyTemplate"),
		},
		Destinations: f.Strings("destinations"),
		Retention: config.Retention{
			DeleteAfterArchive: f.Bool("deleteAfterArchive"),
			VerifyBeforeDelete: &verify,
			DeleteBatchSize:    f.Int("deleteBatchSize", 5000),
		},
		Schedule: config.Schedule{
			Timezone: f.String("timezone"),
		},
		Limits: config.Limits{
			MaxDocsPerRun: f.Int("maxDocsPerRun", 0),
		},
		DryRun: f.Bool("dryRun"),
	}

	if engine == config.EnginePostgres {
		job.Source.Engine = config.EnginePostgres
		job.Source.PostgresSource = alias
		// The schema may be left blank; Prepare makes that "public".
		job.Source.Database = f.String("database")
		// A WHERE clause, kept as text. Whether it is safe to embed is
		// Prepare's call, with a message naming what is wrong.
		job.Source.Filter = config.SQLFilterJSON(f.Raw("filter"))
		// The projection box is hidden for PostgreSQL; a value left in it
		// from switching engines is not something the operator meant.
	} else {
		job.Source.MongoSource = alias
		job.Source.Database = f.Require("database")

		// Filters are typed by hand, so a JSON error is the most likely
		// mistake on this page and deserves a clear message rather than a
		// generic one.
		filter := f.Raw("filter")
		if filter == "" {
			filter = "{}"
		}
		if !json.Valid([]byte(filter)) {
			f.fail("The filter is not valid JSON.")
		}
		job.Source.Filter = json.RawMessage(filter)

		if projection := f.Raw("projection"); projection != "" {
			if !json.Valid([]byte(projection)) {
				f.fail("The projection is not valid JSON.")
			}
			job.Source.Projection = json.RawMessage(projection)
		}
	}

	if size, err := config.ParseByteSize(f.String("batchMaxBytes")); err != nil {
		f.fail("Batch size: %s", err)
	} else {
		job.Batch.MaxBytes = size
	}

	if runtime, err := config.ParseDuration(f.String("maxRuntime")); err != nil {
		f.fail("Maximum runtime: %s", err)
	} else {
		job.Limits.MaxRuntime = config.Duration(runtime)
	}

	switch f.String("scheduleKind") {
	case "cron":
		job.Schedule.Cron = f.Require("cron")
	default:
		job.Schedule.Every = f.Require("every")
		job.Schedule.At = f.String("at")
	}

	if !f.OK() {
		return job, errors.New(f.Error())
	}
	return job, nil
}

// sourceAccountFromForm reads the account picker, whose value is
// "engine:alias". A bare alias — the form as it was before PostgreSQL, or a
// script posting mongoSource — is a MongoDB account.
func sourceAccountFromForm(f *form) (engine, alias string) {
	value := f.String("sourceAccount")
	if value == "" {
		return config.EngineMongo, f.Require("mongoSource")
	}
	if rest, ok := strings.CutPrefix(value, config.EnginePostgres+":"); ok {
		engine, alias = config.EnginePostgres, rest
	} else {
		engine, alias = config.EngineMongo, strings.TrimPrefix(value, config.EngineMongo+":")
	}
	if alias == "" {
		f.fail("Choose the account to read from.")
	}
	return engine, alias
}

// filterText is a config's filter as the form shows it: indented JSON for
// MongoDB, the bare condition for PostgreSQL.
func filterText(job *config.JobConfig) string {
	if job.Source.IsPostgres() {
		condition, err := job.Source.SQLFilter()
		if err != nil {
			return string(job.Source.Filter)
		}
		return condition
	}
	return prettyJSON(string(job.Source.Filter))
}

// engineLabel is the source's engine as people write it.
func engineLabel(job *config.JobConfig) string {
	if job.Source.IsPostgres() {
		return "PostgreSQL"
	}
	return "MongoDB"
}

// auditConfigDetail summarises the parts of a saved config that decide whether
// it destroys data, so the trail is readable without opening the config.
func auditConfigDetail(job *config.JobConfig) string {
	retention := "archive only"
	if job.Retention.DeleteAfterArchive {
		retention = "DELETES from " + engineLabel(job) + " after archiving"
	}
	return fmt.Sprintf("%s %s.%s -> %v; %s", engineLabel(job), job.Source.Database, job.Source.Collection,
		job.Destinations, retention)
}

func (s *Server) handleConfigEnabled(w http.ResponseWriter, r *http.Request) {
	enabled := r.PostFormValue("enabled") == "on" || r.PostFormValue("enabled") == "true"
	if err := s.app.Store.SetJobEnabled(r.Context(), r.PathValue("id"), enabled); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("could not reload after enable/disable", "error", err)
	}

	action := store.AuditConfigDisabled
	if enabled {
		action = store.AuditConfigEnabled
	}
	s.audit(r, action, "config", r.PathValue("id"), "")

	if enabled {
		s.setFlash(w, "ok", "Config resumed — it will run on its schedule again.")
	} else {
		s.setFlash(w, "info", "Config paused. It stays configured but will not run.")
	}
	s.redirectBack(w, r, "/configs")
}

// handleConfigDeletion flips the switch that decides whether an archived batch
// is also removed from MongoDB. It is its own action, separate from editing,
// because it is the one setting that destroys data.
func (s *Server) handleConfigDeletion(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Store.JobByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}

	deleteAfter := r.PostFormValue("deleteAfterArchive") == "on" || r.PostFormValue("deleteAfterArchive") == "true"
	if err := s.app.Store.SetJobDeleteAfterArchive(r.Context(), record.ID, deleteAfter); err != nil {
		// The store validates the resulting config rather than writing it
		// blind. A config with an empty filter and deletion off is perfectly
		// legal to save; turning deletion on makes it "archive the collection,
		// then empty it", which Prepare refuses — and before the store checked,
		// this one click both armed that and wrote a row that Prepare rejects,
		// which parked the config.
		s.setFlash(w, "error", "%s", err.Error())
		s.redirectBack(w, r, "/configs")
		return
	}
	if err := s.reload(r); err != nil {
		s.setFlash(w, "error", "Saved, but the running scheduler could not be reloaded: %s", err.Error())
		s.redirectBack(w, r, "/configs")
		return
	}

	label := engineLabel(record.Config)
	action, detail := store.AuditDeletionDisabled, "archive only; nothing is deleted from "+label
	if deleteAfter {
		action, detail = store.AuditDeletionEnabled, "records will be deleted from "+label+" after every copy is stored and verified"
	}
	s.log.Warn("deletion setting changed",
		"config", record.Config.Name, "delete_after_archive", deleteAfter,
		"user", currentUserEmail(r), "ip", s.clientIP(r))
	s.audit(r, action, "config", record.Config.Name, detail)

	if deleteAfter {
		s.setFlash(w, "info", "%s will now DELETE matching records from %s after every copy is stored and verified.", record.Config.Name, label)
	} else {
		s.setFlash(w, "ok", "%s will now archive only. Nothing will be deleted from %s.", record.Config.Name, label)
	}
	s.redirectBack(w, r, "/configs")
}

func (s *Server) handleConfigRun(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Store.JobByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}

	dryRun := r.PostFormValue("dryRun") != ""
	started, err := s.app.StartRun(record.Config.Name, uiTrigger, dryRun)
	if err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		s.redirectBack(w, r, "/configs")
		return
	}
	if !started {
		s.setFlash(w, "info", "%s is already running.", record.Config.Name)
		s.redirectBack(w, r, "/configs")
		return
	}

	s.log.Info("run started from the control panel",
		"config", record.Config.Name, "dry_run", dryRun, "user", currentUserEmail(r))
	detail := "live run"
	if dryRun {
		detail = "dry run — reads and compresses only"
	}
	s.audit(r, store.AuditRunStarted, "config", record.Config.Name, detail)
	if dryRun {
		s.setFlash(w, "ok", "Dry run of %q started. It reads and compresses everything but uploads and deletes nothing.", record.Config.Name)
	} else {
		s.setFlash(w, "ok", "Run of %q started. Watch its progress under Runs.", record.Config.Name)
	}
	http.Redirect(w, r, "/runs", http.StatusSeeOther)
}

// handleConfigPreview counts what the filter matches right now, which is the
// question worth answering before turning deletion on.
func (s *Server) handleConfigPreview(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Store.JobByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}
	job := record.Config

	// Counting an unindexed filter over a large collection is unbounded work,
	// and this handler had no deadline — so one click could hold a request slot
	// for the full two-minute write timeout. handleAPICollections already caps
	// itself at 15s; this is the same bound, a little more generous because a
	// count legitimately takes longer than a listing.
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	matching, resolved, err := s.app.CountMatching(ctx, job, time.Now())
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.setFlash(w, "error", "Counting matching records took longer than 30 seconds. "+
				"That usually means the filter has no index behind it — the run itself will still work, but it will be slow.")
			s.redirectBack(w, r, configFormPath(record.ID))
			return
		}
		s.setFlash(w, "error", "Counting matching records: %s", config.RedactSecretsFromError(err))
		s.redirectBack(w, r, configFormPath(record.ID))
		return
	}

	if job.Retention.DeleteAfterArchive {
		s.setFlash(w, "info", "%s records match right now. With deletion on, a run would archive and then delete them. Resolved filter: %s",
			humanNumber(matching), resolved)
	} else {
		s.setFlash(w, "ok", "%s records match right now. Deletion is off, so a run would copy them and leave them in place. Resolved filter: %s",
			humanNumber(matching), resolved)
	}
	s.redirectBack(w, r, configFormPath(record.ID))
}

func (s *Server) handleConfigDelete(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Store.JobByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}
	if err := s.app.Store.DeleteJob(r.Context(), record.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("could not reload after config removal", "error", err)
	}

	s.audit(r, store.AuditConfigDeleted, "config", record.Config.Name,
		"archives and run history kept")
	s.setFlash(w, "ok", "Removed the config %q. Its archives and run history are kept.", record.Config.Name)
	http.Redirect(w, r, "/configs", http.StatusSeeOther)
}

// handleAPICollections backs the database and collection pickers on the config
// form, so nobody has to type a collection name from memory.
func (s *Server) handleAPICollections(w http.ResponseWriter, r *http.Request) {
	account := r.URL.Query().Get("source")
	if account == "" {
		http.Error(w, `{"error":"a source alias is required"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	// The picker sends "engine:alias"; a bare alias is MongoDB, as it always
	// was. For PostgreSQL the "databases" are schemas and the "collections"
	// tables, which is what the relabelled fields on the form ask for.
	if alias, ok := strings.CutPrefix(account, config.EnginePostgres+":"); ok {
		db, err := s.app.Postgres.Get(ctx, alias)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": config.RedactSecretsFromError(err)})
			return
		}
		if schema := r.URL.Query().Get("database"); schema != "" {
			tables, err := pgsrc.Tables(ctx, db, schema)
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"error": config.RedactSecretsFromError(err)})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"collections": tables})
			return
		}
		schemas, err := pgsrc.Schemas(ctx, db)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": config.RedactSecretsFromError(err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"databases": schemas})
		return
	}
	alias := strings.TrimPrefix(account, config.EngineMongo+":")

	client, err := s.app.Mongo.Get(ctx, alias)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": config.RedactSecretsFromError(err)})
		return
	}

	if database := r.URL.Query().Get("database"); database != "" {
		collections, err := client.Collections(ctx, database)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": config.RedactSecretsFromError(err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"collections": collections})
		return
	}

	databases, err := client.Databases(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": config.RedactSecretsFromError(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": databases})
}

func configFormPath(id string) string {
	if id == "" {
		return "/configs/new"
	}
	return "/configs/" + id
}

func scheduleKind(job *config.JobConfig) string {
	if job.Schedule.Cron != "" {
		return "cron"
	}
	return "every"
}

func currentUserEmail(r *http.Request) string {
	if _, user, ok := sessionFrom(r); ok {
		return user.Email
	}
	return "unknown"
}
