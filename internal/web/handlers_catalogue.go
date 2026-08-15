package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

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
