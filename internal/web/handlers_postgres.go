package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/pgsrc"
	"github.com/unifers/feather/internal/store"
)

// --- PostgreSQL accounts ---------------------------------------------------
//
// The same shape as the MongoDB account pages, deliberately: an operator who
// has added one kind of source should find nothing new about adding the other.

func (s *Server) handlePostgresList(w http.ResponseWriter, r *http.Request) {
	sources, err := s.app.Store.ListPostgresSources(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	jobs, err := s.app.Store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	usedBy := map[string][]string{}
	for _, job := range jobs {
		if job.Config.Source.IsPostgres() {
			alias := job.Config.Source.PostgresSource
			usedBy[alias] = append(usedBy[alias], job.Config.Name)
		}
	}

	s.render(w, r, "postgres_list", pageData{
		Title: "PostgreSQL accounts",
		Nav:   "postgres",
		Data:  map[string]any{"Sources": sources, "UsedBy": usedBy},
	})
}

func (s *Server) handlePostgresForm(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Source": store.PostgresSourceRecord{Enabled: true, AppName: "feather"}}
	title := "Add a PostgreSQL account"

	if id := r.PathValue("id"); id != "" {
		record, err := s.app.Store.PostgresSourceByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		data["Source"] = record
		title = "PostgreSQL account: " + record.Alias
	}

	s.render(w, r, "postgres_form", pageData{Title: title, Nav: "postgres", Data: data})
}

func (s *Server) handlePostgresSave(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	record := store.PostgresSourceRecord{
		ID:      r.PathValue("id"),
		Alias:   f.Require("alias"),
		DSN:     f.String("dsn"),
		AppName: f.String("appName"),
		Enabled: f.Bool("enabled"),
		Note:    f.String("note"),
	}
	if record.DSN != "" && !looksLikePostgresDSN(record.DSN) {
		f.fail("The connection string must be a postgres:// URL or libpq keywords such as host=… dbname=…")
	}
	if !f.OK() {
		s.setFlash(w, "error", "%s", f.Error())
		s.redirectBack(w, r, postgresFormPath(record.ID))
		return
	}

	if err := s.app.Store.SavePostgresSource(r.Context(), &record); err != nil {
		s.setFlash(w, "error", "%s", config.RedactSecretsFromError(err))
		s.redirectBack(w, r, postgresFormPath(record.ID))
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("saved, but could not reload configuration", "error", err)
	}

	s.audit(r, store.AuditPostgresSaved, "postgres_source", record.Alias, "")
	s.setFlash(w, "ok", "Saved the PostgreSQL account %q. Test it to confirm Feather can reach it.", record.Alias)
	http.Redirect(w, r, "/postgres", http.StatusSeeOther)
}

func (s *Server) handlePostgresTest(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Store.PostgresSourceByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}

	checkErr := pgsrc.Ping(r.Context(), record.Source())
	if recordErr := s.app.Store.RecordPostgresCheck(r.Context(), record.ID, checkErr); recordErr != nil {
		s.log.Warn("could not record connection test", "error", recordErr)
	}
	if checkErr != nil {
		s.setFlash(w, "error", "%s could not be reached: %s", record.Alias, config.RedactSecretsFromError(checkErr))
	} else {
		s.setFlash(w, "ok", "Connected to %s.", record.Alias)
	}
	s.redirectBack(w, r, "/postgres")
}

func (s *Server) handlePostgresDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Store.DeletePostgresSource(r.Context(), r.PathValue("id")); err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		http.Redirect(w, r, "/postgres", http.StatusSeeOther)
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("deleted, but could not reload configuration", "error", err)
	}

	s.audit(r, store.AuditPostgresDeleted, "postgres_source", r.PathValue("id"), "")
	s.setFlash(w, "ok", "Removed the PostgreSQL account.")
	http.Redirect(w, r, "/postgres", http.StatusSeeOther)
}

func postgresFormPath(id string) string {
	if id == "" {
		return "/postgres/new"
	}
	return "/postgres/" + id
}

// looksLikePostgresDSN catches a string that is obviously not a connection
// string — a MongoDB URI pasted into the wrong form, say — before it is
// stored, rather than at the first scheduled run.
func looksLikePostgresDSN(dsn string) bool {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	return strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") ||
		strings.Contains(lower, "host=") || strings.Contains(lower, "dbname=")
}
