package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// One broken row must not stop the others from running, nor hide itself.
func TestABrokenConfigIsParkedRatherThanFatal(t *testing.T) {
	st, ctx := testStore(t)

	if records, err := st.ListJobs(ctx); err != nil || len(records) != 0 {
		t.Fatalf("an empty install lists %d configs (%v)", len(records), err)
	}

	seedAccounts(t, st, ctx)
	seedJob(t, st, ctx, "orders", false)
	badZone := seedJob(t, st, ctx, "events", false)
	noRunID := seedJob(t, st, ctx, "audit", false)

	// Rows that went bad after they were saved: a timezone this host cannot
	// resolve, and a key template from before {runId} was required.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE job_configs SET schedule_timezone = 'Mars/Olympus' WHERE id = ?`, badZone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE job_configs SET key_template = 'archive/{collection}/{seq}{ext}' WHERE id = ?`, noRunID.ID); err != nil {
		t.Fatal(err)
	}

	records, err := st.ListJobs(ctx)
	if err != nil {
		t.Fatalf("one bad row failed the whole listing: %v", err)
	}
	var names []string
	for _, record := range records {
		names = append(names, record.Config.Name)
		if len(record.Config.Destinations) != 1 || record.Config.Destinations[0] != "cold" {
			t.Errorf("%s lost its destinations: %v", record.Config.Name, record.Config.Destinations)
		}
	}
	if strings.Join(names, ",") != "audit,events,orders" {
		t.Fatalf("configs listed as %v, want sorted by name", names)
	}

	broken, err := st.BrokenJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) != 2 {
		t.Fatalf("broken configs are %v, want audit and events", broken)
	}
	if !strings.Contains(broken["events"], "Mars/Olympus") {
		t.Errorf("events is parked for %q", broken["events"])
	}
	if !strings.Contains(broken["audit"], "{runId}") {
		t.Errorf("audit is parked for %q", broken["audit"])
	}

	runnable, err := st.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runnable) != 1 || runnable[0].Name != "orders" {
		t.Errorf("the scheduler would see %d configs, want only orders", len(runnable))
	}
}

func TestSetJobEnabledParksAndResumesAConfig(t *testing.T) {
	st, ctx := testStore(t)
	seedAccounts(t, st, ctx)
	record := seedJob(t, st, ctx, "orders", true)

	before, err := st.JobByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.SetJobEnabled(ctx, record.ID, false); err != nil {
		t.Fatal(err)
	}
	disabled, err := st.JobByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Config.IsEnabled() {
		t.Fatal("the config is still enabled")
	}
	// Runs record the fingerprint of the config they ran, so it must move.
	if disabled.Config.SHA256 == before.Config.SHA256 {
		t.Error("disabling did not change the config's fingerprint")
	}
	// Nothing else may change.
	if !disabled.Config.Retention.DeleteAfterArchive || disabled.Config.Source.Collection != "orders" {
		t.Errorf("disabling touched other settings: %+v", disabled.Config)
	}

	if err := st.SetJobEnabled(ctx, record.ID, true); err != nil {
		t.Fatal(err)
	}
	enabled, err := st.JobByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Config.IsEnabled() {
		t.Error("the config was not re-enabled")
	}

	if err := st.SetJobEnabled(ctx, "no-such-id", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown config gave %v", err)
	}
}

// Deleting a config forgets the config, not the history: the archives are
// still in the buckets, so the record of them has to survive.
func TestDeleteJobKeepsItsRunsAndArchives(t *testing.T) {
	st, ctx := testStore(t)
	source, destination := seedAccounts(t, st, ctx)
	record := seedJob(t, st, ctx, "orders", false)

	run := recordRun(t, st, ctx, "orders", RunSuccess, false, time.Now().UTC())
	insertArtifact(t, st, ctx, run, 1, 10)

	if err := st.DeleteJob(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.JobByID(ctx, record.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the deleted config still loads: %v", err)
	}
	var orphans int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_destinations WHERE job_id = ?`, record.ID).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d destination links outlived their config", orphans)
	}

	archives, err := st.ArtifactsForConfig(ctx, "orders")
	if err != nil || len(archives) != 1 {
		t.Errorf("the config's archives were lost with it: %d (%v)", len(archives), err)
	}
	if _, err := st.Run(ctx, run.ID); err != nil {
		t.Errorf("the config's run was lost with it: %v", err)
	}

	// With nothing pointing at them any more, the accounts can now go.
	if err := st.DeleteMongoSource(ctx, source.ID); err != nil {
		t.Errorf("the source is still considered in use: %v", err)
	}
	if err := st.DeleteDestination(ctx, destination.ID); err != nil {
		t.Errorf("the destination is still considered in use: %v", err)
	}

	if err := st.DeleteJob(ctx, record.ID); err != nil {
		t.Errorf("deleting an already-deleted config should be a no-op: %v", err)
	}
}

func TestJobRecordIsBrokenOnlyWithAProblem(t *testing.T) {
	if (JobRecord{}).Broken() {
		t.Error("a record with no problem reports itself broken")
	}
	if !(JobRecord{Problem: "timezone"}).Broken() {
		t.Error("a record with a problem reports itself fine")
	}
}
