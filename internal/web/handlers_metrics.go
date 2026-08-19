package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/unifers/feather/internal/buildinfo"
)

// Prometheus exposition, written by hand.
//
// feather is a single binary an operator drops on a VPS, and the client library
// would pull in a dependency tree larger than feather itself for what amounts
// to printing numbers in a documented text format. The format is stable and
// small; this is about sixty lines.
//
// The metric that matters most is feather_config_last_success_age_seconds. A
// config that has been failing every night for a week looks fine in every view
// except the runs page, and only if somebody opens it. That gauge is what an
// alert rule watches.

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	totals, err := s.app.Store.Totals(ctx)
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		s.log.Error("metrics query failed", "error", err)
		return
	}
	health, err := s.app.Store.ConfigHealthAll(ctx)
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		s.log.Error("metrics query failed", "error", err)
		return
	}

	var out strings.Builder
	now := time.Now()

	build := buildinfo.Get()
	metric(&out, "feather_build_info", "gauge", "Build information for the running binary.",
		fmt.Sprintf(`feather_build_info{version=%q,commit=%q,go=%q} 1`, build.Version, build.Commit, build.Go))

	simple(&out, "feather_configs_total", "gauge", "Retention configs known to feather.", float64(totals.Configs))
	simple(&out, "feather_configs_enabled", "gauge", "Retention configs that are enabled.", float64(totals.EnabledJobs))
	simple(&out, "feather_destinations_total", "gauge", "Enabled destinations.", float64(totals.Destinations))
	simple(&out, "feather_mongo_sources_total", "gauge", "Enabled MongoDB accounts.", float64(totals.MongoSources))
	simple(&out, "feather_archives_total", "counter", "Archive files ever written.", float64(totals.Artifacts))
	simple(&out, "feather_documents_archived_total", "counter", "Documents ever archived.", float64(totals.DocsArchived))
	simple(&out, "feather_documents_deleted_total", "counter", "Documents ever deleted from MongoDB after archiving.", float64(totals.DocsDeleted))
	simple(&out, "feather_bytes_stored_total", "counter", "Compressed bytes ever written to destinations.", float64(totals.BytesStored))
	simple(&out, "feather_runs_failed_total", "counter", "Runs that ended in failure.", float64(totals.FailedRuns))
	simple(&out, "feather_runs_last_24h", "gauge", "Runs started in the last 24 hours.", float64(totals.RunsLast24h))

	sort.Slice(health, func(i, j int) bool { return health[i].ConfigName < health[j].ConfigName })

	header(&out, "feather_config_enabled", "gauge", "Whether a retention config is enabled.")
	for _, config := range health {
		fmt.Fprintf(&out, "feather_config_enabled{config=%q} %d\n", config.ConfigName, boolValue(config.Enabled))
	}

	header(&out, "feather_config_deletes_source", "gauge", "Whether a config deletes documents after archiving them.")
	for _, config := range health {
		fmt.Fprintf(&out, "feather_config_deletes_source{config=%q} %d\n", config.ConfigName, boolValue(config.DeleteEnabled))
	}

	header(&out, "feather_config_parked", "gauge", "Whether a config is parked because it no longer parses.")
	for _, config := range health {
		fmt.Fprintf(&out, "feather_config_parked{config=%q} %d\n", config.ConfigName, boolValue(config.Parked))
	}

	// Age rather than a timestamp: an alert reads "has not succeeded in 36
	// hours" directly, without arithmetic against the scrape time. A config
	// that has never succeeded is deliberately absent rather than reported as
	// age zero, which would read as "just succeeded".
	header(&out, "feather_config_last_success_age_seconds", "gauge",
		"Seconds since this config last completed a run. Absent if it has never completed one.")
	for _, config := range health {
		if config.LastSuccessAt != nil {
			fmt.Fprintf(&out, "feather_config_last_success_age_seconds{config=%q} %.0f\n",
				config.ConfigName, now.Sub(*config.LastSuccessAt).Seconds())
		}
	}

	header(&out, "feather_config_consecutive_failures", "gauge",
		"Runs that have failed since this config last completed one.")
	for _, config := range health {
		fmt.Fprintf(&out, "feather_config_consecutive_failures{config=%q} %d\n",
			config.ConfigName, config.FailuresSince)
	}

	header(&out, "feather_config_running", "gauge", "Whether a run of this config is in flight on this host.")
	running := s.app.Running()
	for _, config := range health {
		_, inFlight := running[config.ConfigName]
		fmt.Fprintf(&out, "feather_config_running{config=%q} %d\n", config.ConfigName, boolValue(inFlight))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out.String()))
}

func header(out *strings.Builder, name, kind, help string) {
	fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func metric(out *strings.Builder, name, kind, help, line string) {
	header(out, name, kind, help)
	fmt.Fprintln(out, line)
}

func simple(out *strings.Builder, name, kind, help string, value float64) {
	header(out, name, kind, help)
	fmt.Fprintf(out, "%s %g\n", name, value)
}

func boolValue(b bool) int {
	if b {
		return 1
	}
	return 0
}
