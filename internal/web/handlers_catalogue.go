package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	totals, err := s.app.Store.Totals(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	recent, err := s.app.Store.RecentRuns(r.Context(), "", 8)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	usage, err := s.app.Store.StorageByDestination(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	jobs, err := s.app.Store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Leases rather than this process's own map, so a run started by another
	// feather against the same database still reads as running here.
	running, err := s.app.Store.RunningConfigs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// The dashboard leads with the two things an operator most needs to know:
	// what is due next, and which configs are allowed to delete data.
	nextRuns := s.app.Scheduler.NextRuns()
	var deleting []string
	for _, job := range jobs {
		if job.Config.Retention.DeleteAfterArchive && job.Config.IsEnabled() {
			deleting = append(deleting, job.Config.Name)
		}
	}

	s.render(w, r, "dashboard", pageData{
		Title: "Overview",
		Nav:   "dashboard",
		Data: map[string]any{
			"Totals":   totals,
			"Recent":   recent,
			"Usage":    usage,
			"Jobs":     jobs,
			"NextRuns": nextRuns,
			"Deleting": deleting,
			"Running":  running,
			"NeedsSetupSteps": map[string]bool{
				"mongo":        totals.MongoSources == 0,
				"destinations": totals.Destinations == 0,
				"configs":      totals.Configs == 0,
			},
		},
	})
}

// handleBackups is the archive catalogue: every file feather has written, with
// its date, document count, size, and the exact location of every copy.
func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page := max(1, atoiOr(query.Get("page"), 1))
	const perPage = 50

	filter := store.ArtifactQuery{
		ConfigName:  query.Get("config"),
		RunID:       query.Get("run"),
		Search:      query.Get("q"),
		DeletedOnly: query.Get("deleted") == "1",
		Limit:       perPage,
		Offset:      (page - 1) * perPage,
	}

	artifacts, total, err := s.app.Store.Artifacts(r.Context(), filter)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	jobs, err := s.app.Store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	pages := int((total + perPage - 1) / perPage)
	s.render(w, r, "backups", pageData{
		Title: "Backups",
		Nav:   "backups",
		Data: map[string]any{
			"Artifacts": artifacts,
			"Total":     total,
			"Page":      page,
			"Pages":     pages,
			"HasPrev":   page > 1,
			"HasNext":   page < pages,
			"Filter":    filter,
			"Jobs":      jobs,
		},
	})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	configName := r.URL.Query().Get("config")
	runs, err := s.app.Store.RecentRuns(r.Context(), configName, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	jobs, err := s.app.Store.ListJobs(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "runs", pageData{
		Title: "Runs",
		Nav:   "runs",
		Data: map[string]any{
			"Runs":       runs,
			"Jobs":       jobs,
			"ConfigName": configName,
		},
	})
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}

	run, err := s.app.Store.Run(r.Context(), id)
	if err != nil {
		s.notFound(w, r)
		return
	}
	filter, err := s.app.Store.RunFilter(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	artifacts, _, err := s.app.Store.Artifacts(r.Context(), store.ArtifactQuery{RunID: id.String(), Limit: 500})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "run_detail", pageData{
		Title: "Run " + run.ConfigName,
		Nav:   "runs",
		Data: map[string]any{
			"Run":       run,
			"Filter":    filter,
			"Artifacts": artifacts,
		},
	})
}

// --- small helpers ---------------------------------------------------------

func atoiOr(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}

// handleArtifactDownload streams one archive back to the browser.
//
// The catalogue could always say where a copy was; it could not hand it to you.
// For an operator answering "what was in the January orders?", downloading the
// file is the shortest path there is — and it is the same read path `feather
// verify` uses, so a download that succeeds is also a check that the copy is
// still intact.
func (s *Server) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}

	item, err := s.app.Store.ArtifactByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}

	body, alias, err := s.openArchive(r.Context(), item)
	if err != nil {
		s.setFlash(w, "error", "%s could not be downloaded: %s", item.FileName, config.RedactSecrets(err.Error()))
		http.Redirect(w, r, "/backups", http.StatusSeeOther)
		return
	}
	defer func() { _ = body.Close() }()

	s.audit(r, store.AuditArchiveDownloaded, "archive", item.ID.String(),
		fmt.Sprintf("%s from %s", item.FileName, alias))

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(item.FileName)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if item.BytesCompressed > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(item.BytesCompressed))
	}

	if _, err := io.Copy(w, body); err != nil {
		// The status line is long gone, so there is nowhere to report this to
		// the browser. The log is the only honest record that the download was
		// truncated.
		s.log.Warn("archive download did not complete",
			"archive", item.FileName, "destination", alias, "error", err)
	}
}

// openArchive finds a readable copy of an archive, trying each destination in
// turn so one broken bucket does not make the file unreachable.
func (s *Server) openArchive(ctx context.Context, item store.ArtifactWithCopies) (io.ReadCloser, string, error) {
	if len(item.Copies) == 0 {
		return nil, "", fmt.Errorf("no copies of %s are recorded", item.FileName)
	}
	var failures []string
	for _, copy := range item.Copies {
		target, err := s.app.Registry.Get(copy.Alias)
		if err != nil {
			failures = append(failures, copy.Alias+": no longer configured")
			continue
		}
		body, err := target.Open(ctx, copy.ObjectKey, copy.ETag)
		if err != nil {
			failures = append(failures, copy.Alias+": "+err.Error())
			continue
		}
		return body, copy.Alias, nil
	}
	return nil, "", errors.New(strings.Join(failures, "; "))
}
