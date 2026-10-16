package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recordRun opens and closes a run, then pins its timestamps so ordering tests
// do not depend on how fast the machine is.
func recordRun(t *testing.T, st *Store, ctx context.Context, configName string,
	status RunStatus, dryRun bool, startedAt time.Time) *Run {
	t.Helper()
	run := &Run{ConfigName: configName, Database: "shop", Collection: "orders",
		FilterJSON: `{"status":"completed"}`, Trigger: TriggerCron, Host: "test", DryRun: dryRun}
	if err := st.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.Status = status
	if err := st.FinishRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	finished := startedAt.Add(30 * time.Second)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE feather_runs SET started_at = ?, finished_at = ? WHERE id = ?`,
		timeText(startedAt), timeText(finished), run.ID.String()); err != nil {
		t.Fatal(err)
	}
	run.StartedAt, run.FinishedAt = startedAt, &finished
	return run
}

func insertArtifact(t *testing.T, st *Store, ctx context.Context, run *Run, seq int, docs int64) *Artifact {
	t.Helper()
	artifact := &Artifact{RunID: run.ID, ConfigName: run.ConfigName, Database: "shop", Collection: "orders",
		BatchSeq: seq, FileName: run.ConfigName + "-" + uuid.NewString()[:8] + ".ndjson.gz",
		Format: "ndjson", Codec: "gzip", DocCount: docs, BytesRaw: docs * 10, BytesCompressed: docs, SHA256: "x"}
	if err := st.InsertArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	return artifact
}

func insertCopy(t *testing.T, st *Store, ctx context.Context, artifactID uuid.UUID, alias, bucket string) *ArtifactDestination {
	t.Helper()
	copy := &ArtifactDestination{ArtifactID: artifactID, Alias: alias, Provider: "s3", Bucket: bucket,
		ObjectKey: "archive/" + artifactID.String()}
	if err := st.InsertArtifactDestination(ctx, copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

// A long run has to be observable while it is going, and a hard kill has to
// leave its numbers behind.
func TestUpdateRunProgressMakesCountersVisibleMidRun(t *testing.T) {
	st, ctx := testStore(t)

	run := &Run{ConfigName: "orders", Database: "shop", Collection: "orders", FilterJSON: "{}",
		Trigger: TriggerUI, Host: "test"}
	if err := st.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.DocsMatched, run.DocsScanned, run.DocsArchived, run.DocsDeleted = 1000, 600, 500, 400
	run.BytesRaw, run.BytesCompressed, run.BatchCount = 9000, 1200, 5
	if err := st.UpdateRunProgress(ctx, run); err != nil {
		t.Fatal(err)
	}

	got, err := st.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunRunning || got.FinishedAt != nil {
		t.Errorf("a progress update changed the run's state: %s, finished %v", got.Status, got.FinishedAt)
	}
	if got.DocsMatched != 1000 || got.DocsScanned != 600 || got.DocsArchived != 500 ||
		got.DocsDeleted != 400 || got.BytesStored != 1200 || got.Batches != 5 {
		t.Errorf("counters are %+v", got)
	}
	if got.Progress() != 50 {
		t.Errorf("progress reads %d%%, want 50%%", got.Progress())
	}
}

func TestMarkDestinationFailedRecordsTheCauseUntilARetrySucceeds(t *testing.T) {
	st, ctx := testStore(t)
	run := recordRun(t, st, ctx, "orders", RunPartial, false, time.Now().UTC())
	artifact := insertArtifact(t, st, ctx, run, 1, 10)
	copy := insertCopy(t, st, ctx, artifact.ID, "cold", "archive")

	cause := "AccessDenied: " + strings.Repeat("x", 5000)
	if err := st.MarkDestinationFailed(ctx, copy.ID, cause); err != nil {
		t.Fatal(err)
	}
	found, err := st.ArtifactByID(ctx, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := found.Copies[0]
	if failed.Status != DestFailed {
		t.Fatalf("copy is %s, want failed", failed.Status)
	}
	if !strings.HasPrefix(failed.Error, "AccessDenied") || len(failed.Error) > 4000+len("…") {
		t.Errorf("the cause was not stored, or not capped: %d bytes", len(failed.Error))
	}

	if err := st.MarkDestinationUploaded(ctx, copy.ID, "etag-1", 10); err != nil {
		t.Fatal(err)
	}
	found, err = st.ArtifactByID(ctx, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found.Copies[0].Status != DestUploaded || found.Copies[0].Error != "" {
		t.Errorf("a successful retry left %s with error %q", found.Copies[0].Status, found.Copies[0].Error)
	}
}

// Interval schedules anchor on the last success; a later failure must not move
// the anchor, and another config's runs must not count.
func TestLastSuccessAtAnchorsOnTheLatestCompletedRun(t *testing.T) {
	st, ctx := testStore(t)

	if _, ok, err := st.LastSuccessAt(ctx, "orders"); err != nil || ok {
		t.Fatalf("a config that never ran has a last success (%v, %v)", ok, err)
	}

	// On the second exactly, then sub-second: the case the old timestamp
	// layout sorted backwards.
	base := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)
	recordRun(t, st, ctx, "orders", RunSuccess, false, base)
	partial := recordRun(t, st, ctx, "orders", RunPartial, false, base.Add(5*time.Millisecond))
	recordRun(t, st, ctx, "orders", RunFailed, false, base.Add(time.Hour))
	recordRun(t, st, ctx, "orders", RunRunning, false, base.Add(2*time.Hour))
	recordRun(t, st, ctx, "events", RunSuccess, false, base.Add(3*time.Hour))

	at, ok, err := st.LastSuccessAt(ctx, "orders")
	if err != nil || !ok {
		t.Fatalf("no last success found (%v, %v)", ok, err)
	}
	// The anchor is when the run finished, not when it started.
	if !at.Equal(*partial.FinishedAt) {
		t.Fatalf("last success is %s, want the partial run's finish %s", at, *partial.FinishedAt)
	}
}

func TestRunFilterReturnsWhatTheRunWasAllowedToTouch(t *testing.T) {
	st, ctx := testStore(t)
	run := recordRun(t, st, ctx, "orders", RunSuccess, false, time.Now().UTC())

	filter, err := st.RunFilter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if filter != `{"status":"completed"}` {
		t.Errorf("filter is %q", filter)
	}
	if _, err := st.RunFilter(ctx, uuid.New()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("an unknown run gave %v", err)
	}
}

func TestTotalsOnAnEmptyInstallAreZero(t *testing.T) {
	st, ctx := testStore(t)
	totals, err := st.Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if totals != (Totals{}) {
		t.Errorf("an empty install reports %+v", totals)
	}
}

func TestTotalsGatherTheDashboardNumbers(t *testing.T) {
	st, ctx := testStore(t)
	seedAccounts(t, st, ctx)
	seedJob(t, st, ctx, "orders", false)
	parked := seedJob(t, st, ctx, "events", false)
	if err := st.SetJobEnabled(ctx, parked.ID, false); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	old := recordRun(t, st, ctx, "orders", RunFailed, false, now.Add(-48*time.Hour))
	recent := recordRun(t, st, ctx, "orders", RunSuccess, false, now.Add(-time.Hour))
	recordRun(t, st, ctx, "events", RunPartial, false, now.Add(-30*time.Minute))

	first := insertArtifact(t, st, ctx, old, 1, 100)
	insertArtifact(t, st, ctx, recent, 1, 250)
	if err := st.MarkArtifactDeleted(ctx, first.ID, 90); err != nil {
		t.Fatal(err)
	}
	oldest := now.Add(-47 * time.Hour)
	if _, err := st.db.ExecContext(ctx, `UPDATE feather_artifacts SET created_at = ? WHERE id = ?`,
		timeText(oldest), first.ID.String()); err != nil {
		t.Fatal(err)
	}

	totals, err := st.Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Totals{Configs: 2, EnabledJobs: 1, Destinations: 1, MongoSources: 1, Artifacts: 2,
		DocsArchived: 350, DocsDeleted: 90, BytesStored: 350, FailedRuns: 2, RunsLast24h: 2}
	got := totals
	got.LastRunAt, got.OldestArchive = nil, nil
	if got != want {
		t.Errorf("totals are\n %+v\nwant\n %+v", got, want)
	}
	if totals.LastRunAt == nil || !totals.LastRunAt.Equal(now.Add(-30*time.Minute).Truncate(time.Microsecond)) {
		t.Errorf("last run at %v", totals.LastRunAt)
	}
	if totals.OldestArchive == nil || !totals.OldestArchive.Equal(oldest.Truncate(time.Microsecond)) {
		t.Errorf("oldest archive at %v, want %s", totals.OldestArchive, oldest)
	}
}

// Only copies that actually landed count towards what a destination holds.
func TestStorageByDestinationCountsOnlyCopiesThatLanded(t *testing.T) {
	st, ctx := testStore(t)

	usage, err := st.StorageByDestination(ctx)
	if err != nil || len(usage) != 0 {
		t.Fatalf("an empty catalogue reports usage %v (%v)", usage, err)
	}

	run := recordRun(t, st, ctx, "orders", RunPartial, false, time.Now().UTC())
	var copies []*ArtifactDestination
	for seq := 1; seq <= 4; seq++ {
		artifact := insertArtifact(t, st, ctx, run, seq, 10)
		copies = append(copies, insertCopy(t, st, ctx, artifact.ID, "cold", "archive"))
		if seq == 1 {
			insertCopy(t, st, ctx, artifact.ID, "attic", "spare")
		}
	}
	if err := st.MarkDestinationUploaded(ctx, copies[0].ID, "e", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDestinationUploaded(ctx, copies[1].ID, "e", 200); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDestinationVerified(ctx, copies[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDestinationFailed(ctx, copies[2].ID, "boom"); err != nil {
		t.Fatal(err)
	}
	// copies[3] stays pending.

	usage, err = st.StorageByDestination(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 || usage[0].Alias != "attic" || usage[1].Alias != "cold" {
		t.Fatalf("usage is %+v", usage)
	}
	attic, cold := usage[0], usage[1]
	if attic.Objects != 1 || attic.Bytes != 0 || attic.LastWrite != nil {
		t.Errorf("a destination with nothing uploaded reports %+v", attic)
	}
	if cold.Objects != 4 || cold.Bytes != 300 || cold.Failed != 1 || cold.Bucket != "archive" || cold.Provider != "s3" {
		t.Errorf("cold reports %+v", cold)
	}
	if cold.LastWrite == nil || time.Since(*cold.LastWrite) > time.Minute {
		t.Errorf("cold's last write is %v", cold.LastWrite)
	}
}

func TestArtifactByIDReturnsTheFileWithEveryCopy(t *testing.T) {
	st, ctx := testStore(t)
	run := recordRun(t, st, ctx, "orders", RunSuccess, false, time.Now().UTC())
	artifact := insertArtifact(t, st, ctx, run, 1, 42)
	insertArtifact(t, st, ctx, run, 2, 7) // a neighbour that must not leak in
	insertCopy(t, st, ctx, artifact.ID, "cold", "archive")
	insertCopy(t, st, ctx, artifact.ID, "attic", "spare")

	found, err := st.ArtifactByID(ctx, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != artifact.ID || found.DocCount != 42 || found.RunID != run.ID {
		t.Errorf("found %+v", found.Artifact)
	}
	if len(found.Copies) != 2 || found.Copies[0].Alias != "attic" || found.Copies[1].Alias != "cold" {
		t.Errorf("copies are %+v", found.Copies)
	}
	for _, copy := range found.Copies {
		if copy.ArtifactID != artifact.ID || copy.Status != DestPending {
			t.Errorf("copy %+v belongs elsewhere or has the wrong status", copy)
		}
	}

	if _, err := st.ArtifactByID(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown artifact gave %v", err)
	}
}

// A restore replays batches in the order they were written. When two land in
// the same microsecond, the batch number decides.
func TestArtifactsForRunListsOldestBatchFirst(t *testing.T) {
	st, ctx := testStore(t)
	run := recordRun(t, st, ctx, "orders", RunSuccess, false, time.Now().UTC())
	other := recordRun(t, st, ctx, "orders", RunSuccess, false, time.Now().UTC())

	// Inserted out of order, then given one shared timestamp.
	for _, seq := range []int{3, 1, 2} {
		insertArtifact(t, st, ctx, run, seq, int64(seq))
	}
	insertArtifact(t, st, ctx, other, 1, 99)
	if _, err := st.db.ExecContext(ctx, `UPDATE feather_artifacts SET created_at = ? WHERE run_id = ?`,
		timeText(time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)), run.ID.String()); err != nil {
		t.Fatal(err)
	}

	found, err := st.ArtifactsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("listed %d artifacts for the run, want 3", len(found))
	}
	for i, artifact := range found {
		if artifact.BatchSeq != i+1 || artifact.RunID != run.ID {
			t.Fatalf("position %d is batch %d of run %s", i, artifact.BatchSeq, artifact.RunID)
		}
	}

	none, err := st.ArtifactsForRun(ctx, uuid.New())
	if err != nil || len(none) != 0 {
		t.Errorf("an unknown run listed %d artifacts (%v)", len(none), err)
	}
}

func TestArtifactsForConfigSpansRunsInTheOrderTheyWereWritten(t *testing.T) {
	st, ctx := testStore(t)
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	first := recordRun(t, st, ctx, "orders", RunSuccess, false, base)
	second := recordRun(t, st, ctx, "orders", RunSuccess, false, base.Add(time.Hour))
	elsewhere := recordRun(t, st, ctx, "events", RunSuccess, false, base)

	// Pin creation times on and around a whole second, newest inserted first.
	later := insertArtifact(t, st, ctx, second, 1, 2)
	earlier := insertArtifact(t, st, ctx, first, 1, 1)
	insertArtifact(t, st, ctx, elsewhere, 1, 3)
	for id, at := range map[uuid.UUID]time.Time{earlier.ID: base, later.ID: base.Add(5 * time.Millisecond)} {
		if _, err := st.db.ExecContext(ctx, `UPDATE feather_artifacts SET created_at = ? WHERE id = ?`,
			timeText(at), id.String()); err != nil {
			t.Fatal(err)
		}
	}

	found, err := st.ArtifactsForConfig(ctx, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].ID != earlier.ID || found[1].ID != later.ID {
		var ids []string
		for _, a := range found {
			ids = append(ids, a.ConfigName+"/"+a.CreatedAt.Format(time.RFC3339Nano))
		}
		t.Fatalf("listed %v, want the first run's archive then the second's", ids)
	}
}

// The age of the last success is what an alert watches; dry runs prove
// nothing about whether archiving works, so they must not count.
func TestConfigHealthReportsLastSuccessAndConsecutiveFailures(t *testing.T) {
	st, ctx := testStore(t)
	seedAccounts(t, st, ctx)
	seedJob(t, st, ctx, "failing", true)
	seedJob(t, st, ctx, "healthy", false)
	seedJob(t, st, ctx, "never-ran", false)
	parked := seedJob(t, st, ctx, "parked", false)
	if _, err := st.db.ExecContext(ctx, `UPDATE job_configs SET schedule_timezone = 'Mars/Olympus' WHERE id = ?`,
		parked.ID); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)
	// failing: an old failure, a success, then three failures — one of them
	// only 5ms after the success, where the old layout mis-sorted.
	recordRun(t, st, ctx, "failing", RunFailed, false, base.Add(-time.Hour))
	success := recordRun(t, st, ctx, "failing", RunSuccess, false, base)
	recordRun(t, st, ctx, "failing", RunFailed, false, base.Add(5*time.Millisecond))
	recordRun(t, st, ctx, "failing", RunFailed, false, base.Add(24*time.Hour))
	lastFailure := recordRun(t, st, ctx, "failing", RunFailed, false, base.Add(48*time.Hour))
	// A later dry run neither resets the count nor becomes the last run.
	recordRun(t, st, ctx, "failing", RunSuccess, true, base.Add(72*time.Hour))

	recordRun(t, st, ctx, "healthy", RunFailed, false, base)
	healthySuccess := recordRun(t, st, ctx, "healthy", RunPartial, false, base.Add(time.Hour))

	health, err := st.ConfigHealthAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ConfigHealth{}
	var names []string
	for _, h := range health {
		byName[h.ConfigName] = h
		names = append(names, h.ConfigName)
	}
	if strings.Join(names, ",") != "failing,healthy,never-ran,parked" {
		t.Fatalf("configs listed as %v", names)
	}

	failing := byName["failing"]
	if failing.FailuresSince != 3 {
		t.Errorf("failing has %d consecutive failures, want 3", failing.FailuresSince)
	}
	if failing.LastSuccessAt == nil || !failing.LastSuccessAt.Equal(*success.FinishedAt) {
		t.Errorf("failing last succeeded at %v, want %s", failing.LastSuccessAt, *success.FinishedAt)
	}
	if failing.LastRunAt == nil || !failing.LastRunAt.Equal(lastFailure.StartedAt) {
		t.Errorf("failing last ran at %v, want %s (the dry run must not count)", failing.LastRunAt, lastFailure.StartedAt)
	}
	if failing.LastStatus != RunFailed || !failing.DeleteEnabled || !failing.Enabled || failing.Parked {
		t.Errorf("failing reports %+v", failing)
	}

	healthy := byName["healthy"]
	if healthy.FailuresSince != 0 || healthy.LastStatus != RunPartial ||
		healthy.LastSuccessAt == nil || !healthy.LastSuccessAt.Equal(*healthySuccess.FinishedAt) {
		t.Errorf("healthy reports %+v", healthy)
	}

	never := byName["never-ran"]
	if never.LastSuccessAt != nil || never.LastRunAt != nil || never.LastStatus != "" || never.FailuresSince != 0 {
		t.Errorf("a config that never ran reports %+v", never)
	}

	if !byName["parked"].Parked {
		t.Error("a config that no longer prepares is not reported as parked")
	}
}

func TestRunSummaryProgressIsAClampedPercentage(t *testing.T) {
	cases := []struct {
		matched, archived int64
		want              int
	}{
		{0, 0, 0},
		{0, 500, 0}, // unknown total: no bar rather than a nonsense one
		{-1, 5, 0},
		{200, 50, 25},
		{3, 1, 33},
		{100, 100, 100},
		{100, 250, 100},
	}
	for _, tc := range cases {
		got := RunSummary{DocsMatched: tc.matched, DocsArchived: tc.archived}.Progress()
		if got != tc.want {
			t.Errorf("%d of %d is %d%%, want %d%%", tc.archived, tc.matched, got, tc.want)
		}
	}
}

func TestRunSummaryDurationIsElapsedOrOngoing(t *testing.T) {
	started := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)
	finished := started.Add(90 * time.Second)
	if got := (RunSummary{StartedAt: started, FinishedAt: &finished}).Duration(); got != 90*time.Second {
		t.Errorf("a finished run lasted %s, want 1m30s", got)
	}

	running := RunSummary{StartedAt: time.Now().Add(-time.Minute)}
	if got := running.Duration(); got < time.Minute || got > 2*time.Minute {
		t.Errorf("a run started a minute ago reports %s", got)
	}
}
