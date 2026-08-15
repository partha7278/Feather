package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/secrets"
)

func TestMigrateIsRepeatable(t *testing.T) {
	st, ctx := testStore(t)

	// Startup runs migrations every time; a second pass must be a no-op rather
	// than an error.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestMongoSourceSecretsAreEncryptedAtRest(t *testing.T) {
	st, ctx := testStore(t)
	const uri = "mongodb://user:hunter2@127.0.0.1:27017"

	record := MongoSourceRecord{Alias: "prod", URI: uri, Enabled: true}
	if err := st.SaveMongoSource(ctx, &record); err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := st.db.QueryRowContext(ctx,
		`SELECT uri_enc FROM mongo_sources WHERE id = ?`, record.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "hunter2") {
		t.Fatal("the password is readable in the database")
	}

	loaded, err := st.MongoSourceByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.URI != uri {
		t.Fatalf("decrypted URI is %q", loaded.URI)
	}
	if strings.Contains(loaded.Preview, "hunter2") {
		t.Fatalf("the preview leaks the password: %s", loaded.Preview)
	}
}

// Editing an account in the UI never re-sends the password, so an empty URI on
// update has to mean "keep the stored one".
func TestSaveMongoSourceKeepsSecretWhenBlank(t *testing.T) {
	st, ctx := testStore(t)

	record := MongoSourceRecord{Alias: "prod", URI: "mongodb://user:secret@host:27017", Enabled: true}
	if err := st.SaveMongoSource(ctx, &record); err != nil {
		t.Fatal(err)
	}

	update := MongoSourceRecord{ID: record.ID, Alias: "prod-renamed", Enabled: true}
	if err := st.SaveMongoSource(ctx, &update); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.MongoSourceByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Alias != "prod-renamed" {
		t.Fatalf("alias was not updated: %q", loaded.Alias)
	}
	if loaded.URI != "mongodb://user:secret@host:27017" {
		t.Fatalf("the stored connection string was lost: %q", loaded.URI)
	}
}

func TestDuplicateAliasIsRejectedClearly(t *testing.T) {
	st, ctx := testStore(t)

	first := MongoSourceRecord{Alias: "prod", URI: "mongodb://a", Enabled: true}
	if err := st.SaveMongoSource(ctx, &first); err != nil {
		t.Fatal(err)
	}
	second := MongoSourceRecord{Alias: "prod", URI: "mongodb://b", Enabled: true}

	err := st.SaveMongoSource(ctx, &second)
	if err == nil {
		t.Fatal("expected a duplicate alias to be refused")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error does not explain the problem: %v", err)
	}
}

// Removing an account a config still points at would leave a job that fails at
// its next scheduled run, unattended.
func TestDeletingAnAccountInUseIsRefused(t *testing.T) {
	st, ctx := testStore(t)
	source, destination := seedAccounts(t, st, ctx)
	seedJob(t, st, ctx, "orders-archive", true)

	err := st.DeleteMongoSource(ctx, source.ID)
	var inUse *ErrInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("expected an in-use error, got %v", err)
	}
	if len(inUse.Jobs) != 1 || inUse.Jobs[0] != "orders-archive" {
		t.Fatalf("in-use error does not name the config: %v", inUse.Jobs)
	}

	if err := st.DeleteDestination(ctx, destination.ID); !errors.As(err, &inUse) {
		t.Fatalf("expected the destination to be protected too, got %v", err)
	}
}

func TestJobRoundTripPreservesEverything(t *testing.T) {
	st, ctx := testStore(t)
	seedAccounts(t, st, ctx)
	record := seedJob(t, st, ctx, "orders-archive", true)

	loaded, err := st.JobByName(ctx, "orders-archive")
	if err != nil {
		t.Fatal(err)
	}
	job := loaded.Config

	if job.Source.MongoSource != "prod" {
		t.Fatalf("mongo account is %q", job.Source.MongoSource)
	}
	if !job.Retention.DeleteAfterArchive {
		t.Fatal("the deletion flag did not survive the round trip")
	}
	if got := job.Output.KeyTemplate; got != "archive/{db}/{collection}/{config}-{runId}-{seq}{ext}" {
		t.Fatalf("file name template is %q", got)
	}
	if len(job.Destinations) != 1 || job.Destinations[0] != "cold" {
		t.Fatalf("destinations are %v", job.Destinations)
	}
	if job.Schedule.Location == nil || job.Schedule.Location.String() != "Asia/Kolkata" {
		t.Fatal("the schedule was not resolved on load")
	}
	if job.Batch.MaxBytes != 8<<20 {
		t.Fatalf("batch size is %d", job.Batch.MaxBytes)
	}
	if loaded.ID != record.ID {
		t.Fatal("saving twice created a second row")
	}
}

// The deletion switch is the one setting that destroys data, so it can be
// changed on its own without touching anything else.
func TestSetJobDeleteAfterArchive(t *testing.T) {
	st, ctx := testStore(t)
	seedAccounts(t, st, ctx)
	record := seedJob(t, st, ctx, "orders-archive", false)

	if err := st.SetJobDeleteAfterArchive(ctx, record.ID, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.JobByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Config.Retention.DeleteAfterArchive {
		t.Fatal("deletion was not turned on")
	}

	if err := st.SetJobDeleteAfterArchive(ctx, record.ID, false); err != nil {
		t.Fatal(err)
	}
	loaded, err = st.JobByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Retention.DeleteAfterArchive {
		t.Fatal("deletion was not turned off")
	}
}

func TestSaveJobRejectsUnknownAccounts(t *testing.T) {
	st, ctx := testStore(t)
	seedAccounts(t, st, ctx)

	job := newTestJob("orders-archive", true)
	job.Source.MongoSource = "nonexistent"
	record := JobRecord{Config: job}

	if err := st.SaveJob(ctx, &record); err == nil {
		t.Fatal("expected a config naming an unknown MongoDB account to be refused")
	}
}

// The catalogue is the whole point: it has to answer what was archived, when,
// how big it was, how many documents it held, and where every copy went.
func TestCatalogueRecordsFileDateCountSizeAndLocation(t *testing.T) {
	st, ctx := testStore(t)

	run := &Run{ConfigName: "orders-archive", Database: "shop", Collection: "orders",
		FilterJSON: `{"status":"completed"}`, Trigger: TriggerCron, Host: "test"}
	if err := st.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	minTS := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTS := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	artifact := &Artifact{
		RunID: run.ID, ConfigName: "orders-archive", Database: "shop", Collection: "orders",
		BatchSeq: 1, FileName: "orders-archive-00001.ndjson.gz", Format: "ndjson", Codec: "gzip",
		DocCount: 3289, BytesRaw: 1025433, BytesCompressed: 68662,
		SHA256: "d4571c24", MinTS: &minTS, MaxTS: &maxTS,
	}
	if err := st.InsertArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}

	copies := []*ArtifactDestination{
		{ArtifactID: artifact.ID, Alias: "s3-primary", Provider: "s3", Bucket: "archive",
			ObjectKey: "feather/shop/orders/2026/01/orders-archive-00001.ndjson.gz", SizeBytes: 68662},
		{ArtifactID: artifact.ID, Alias: "oci-cold", Provider: "oci", Bucket: "cold",
			Endpoint:  "https://ns.compat.objectstorage.ap-mumbai-1.oraclecloud.com",
			ObjectKey: "feather/shop/orders/2026/01/orders-archive-00001.ndjson.gz", SizeBytes: 68662},
	}
	for _, copy := range copies {
		if err := st.InsertArtifactDestination(ctx, copy); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkDestinationUploaded(ctx, copy.ID, "etag", 68662); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkDestinationVerified(ctx, copy.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkArtifactDeleted(ctx, artifact.ID, 3289); err != nil {
		t.Fatal(err)
	}

	found, total, err := st.Artifacts(ctx, ArtifactQuery{ConfigName: "orders-archive"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(found) != 1 {
		t.Fatalf("expected one archive, got %d", total)
	}

	got := found[0]
	if got.FileName != "orders-archive-00001.ndjson.gz" {
		t.Fatalf("file name is %q", got.FileName)
	}
	if got.DocCount != 3289 || got.BytesCompressed != 68662 {
		t.Fatalf("count/size are %d/%d", got.DocCount, got.BytesCompressed)
	}
	if got.CreatedAt.IsZero() || got.MinTS == nil || !got.MinTS.Equal(minTS) {
		t.Fatal("the file's dates were not recorded")
	}
	if !got.DeletedFromSource || got.DeletedCount != 3289 {
		t.Fatal("the deletion of the source documents was not recorded")
	}
	if len(got.Copies) != 2 {
		t.Fatalf("expected two copies, got %d", len(got.Copies))
	}

	locations := map[string]string{}
	for _, copy := range got.Copies {
		locations[copy.Alias] = copy.LocationURL
		if copy.Status != DestVerified {
			t.Fatalf("copy %s is %s", copy.Alias, copy.Status)
		}
	}
	if want := "s3://archive/feather/shop/orders/2026/01/orders-archive-00001.ndjson.gz"; locations["s3-primary"] != want {
		t.Fatalf("AWS location is %q", locations["s3-primary"])
	}
	if want := "https://ns.compat.objectstorage.ap-mumbai-1.oraclecloud.com/cold/feather/shop/orders/2026/01/orders-archive-00001.ndjson.gz"; locations["oci-cold"] != want {
		t.Fatalf("Oracle location is %q", locations["oci-cold"])
	}
}

func TestArtifactSearchAndFilters(t *testing.T) {
	st, ctx := testStore(t)

	for i, name := range []string{"alpha", "beta"} {
		run := &Run{ConfigName: name, Database: "shop", Collection: "orders",
			FilterJSON: "{}", Trigger: TriggerManual, Host: "test"}
		if err := st.StartRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		artifact := &Artifact{RunID: run.ID, ConfigName: name, Database: "shop", Collection: "orders",
			BatchSeq: 1, FileName: name + "-00001.ndjson.gz", Format: "ndjson", Codec: "gzip",
			DocCount: int64(10 * (i + 1)), BytesCompressed: 100, SHA256: "x"}
		if err := st.InsertArtifact(ctx, artifact); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := st.MarkArtifactDeleted(ctx, artifact.ID, 10); err != nil {
				t.Fatal(err)
			}
		}
	}

	byConfig, _, err := st.Artifacts(ctx, ArtifactQuery{ConfigName: "beta"})
	if err != nil || len(byConfig) != 1 || byConfig[0].ConfigName != "beta" {
		t.Fatalf("filtering by config failed: %v %v", byConfig, err)
	}

	bySearch, _, err := st.Artifacts(ctx, ArtifactQuery{Search: "alpha"})
	if err != nil || len(bySearch) != 1 {
		t.Fatalf("searching by file name failed: %v %v", bySearch, err)
	}

	deleted, _, err := st.Artifacts(ctx, ArtifactQuery{DeletedOnly: true})
	if err != nil || len(deleted) != 1 || deleted[0].ConfigName != "alpha" {
		t.Fatalf("filtering to deleted-source archives failed: %v %v", deleted, err)
	}
}

// Two runs of one config must not overlap, whether they come from the
// scheduler, the UI, or a second process against the same database.
func TestJobLeaseIsExclusive(t *testing.T) {
	st, ctx := testStore(t)

	first, ok, err := st.TryLockJob(ctx, "orders-archive")
	if err != nil || !ok {
		t.Fatalf("first lock: %v %v", ok, err)
	}

	if _, ok, err := st.TryLockJob(ctx, "orders-archive"); err != nil || ok {
		t.Fatalf("second lock should have been refused: %v %v", ok, err)
	}
	// A different config is unaffected.
	other, ok, err := st.TryLockJob(ctx, "other-config")
	if err != nil || !ok {
		t.Fatalf("locking a different config: %v %v", ok, err)
	}
	other.Release(ctx)

	running, err := st.RunningConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !running["orders-archive"] {
		t.Fatal("the held lease is not reported as running")
	}

	first.Release(ctx)
	if _, ok, err := st.TryLockJob(ctx, "orders-archive"); err != nil || !ok {
		t.Fatalf("lock after release: %v %v", ok, err)
	}
}

func TestFailStaleRunsClosesThisHostsRuns(t *testing.T) {
	st, ctx := testStore(t)

	run := &Run{ConfigName: "orders-archive", Database: "shop", Collection: "orders",
		FilterJSON: "{}", Trigger: TriggerCron, Host: "crashed-host"}
	if err := st.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	closed, err := st.FailStaleRuns(ctx, "crashed-host")
	if err != nil || closed != 1 {
		t.Fatalf("expected one run to be closed, got %d (%v)", closed, err)
	}

	summary, err := st.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != RunFailed || summary.FinishedAt == nil {
		t.Fatalf("run was left as %s", summary.Status)
	}
}

// Startup cleanup must not touch a run that is genuinely still going in another
// process on the same host. Marking it failed made the catalogue flicker: the
// live run's own FinishRun overwrote the status again minutes later, and in
// between the UI reported a failure that had not happened.
func TestFailStaleRunsLeavesALiveRunAlone(t *testing.T) {
	st, ctx := testStore(t)

	// A live run, holding its lease the way a real one does.
	lease, ok, err := st.TryLockJob(ctx, "orders-archive")
	if err != nil || !ok {
		t.Fatalf("taking the lease: %v %v", ok, err)
	}
	defer lease.Release(ctx)

	host, _ := os.Hostname()
	live := &Run{ConfigName: "orders-archive", Database: "shop", Collection: "orders",
		FilterJSON: "{}", Trigger: TriggerCron, Host: host}
	if err := st.StartRun(ctx, live); err != nil {
		t.Fatal(err)
	}

	// A second run row on the same host with no lease behind it: this one was
	// abandoned by a process that died, and is the one to close.
	abandoned := &Run{ConfigName: "invoices-archive", Database: "shop", Collection: "invoices",
		FilterJSON: "{}", Trigger: TriggerCron, Host: host}
	if err := st.StartRun(ctx, abandoned); err != nil {
		t.Fatal(err)
	}

	closed, err := st.FailStaleRuns(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Errorf("closed %d runs, want only the abandoned one", closed)
	}

	stillRunning, err := st.Run(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillRunning.Status != RunRunning {
		t.Errorf("a live run in another process was marked %s", stillRunning.Status)
	}
}

// ClearStaleLeases used to be `WHERE host = ? OR expires_at < ?`, so any
// feather starting up deleted every live lease held by any other feather on the
// same host — and both then archived the same config at once.
func TestClearStaleLeasesLeavesALiveLeaseAlone(t *testing.T) {
	st, ctx := testStore(t)

	lease, ok, err := st.TryLockJob(ctx, "orders-archive")
	if err != nil || !ok {
		t.Fatalf("taking the lease: %v %v", ok, err)
	}
	defer lease.Release(ctx)

	host, _ := os.Hostname()
	if _, err := st.ClearStaleLeases(ctx, host); err != nil {
		t.Fatal(err)
	}

	running, err := st.RunningConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !running["orders-archive"] {
		t.Fatal("startup cleanup deleted a live lease held by another process on this host")
	}

	// And the config is still locked, which is the consequence that matters:
	// if the lease were gone, a second process would archive it concurrently.
	if _, ok, err := st.TryLockJob(ctx, "orders-archive"); err != nil || ok {
		t.Fatalf("the config became lockable after cleanup: %v %v", ok, err)
	}
}

// The other half: a lease whose expiry has passed belongs to a process that is
// not coming back, and must not block the config forever.
func TestClearStaleLeasesRemovesAnExpiredLease(t *testing.T) {
	st, ctx := testStore(t)

	expired := timeText(time.Now().UTC().Add(-time.Hour))
	_, err := st.DB().ExecContext(ctx, `
		INSERT INTO job_leases (config_name, holder, host, pid, acquired_at, expires_at)
		VALUES (?,?,?,?,?,?)`,
		"orders-archive", "ghost/1/1", "some-other-host", 999999, expired, expired)
	if err != nil {
		t.Fatal(err)
	}

	host, _ := os.Hostname()
	cleared, err := st.ClearStaleLeases(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 1 {
		t.Errorf("cleared %d leases, want the one that had expired", cleared)
	}
	if _, ok, err := st.TryLockJob(ctx, "orders-archive"); err != nil || !ok {
		t.Fatalf("the config is still locked by a dead process: %v %v", ok, err)
	}
}

// A lease still inside its TTL whose owning process is gone — the crash case.
// Only this host can tell, so only this host's rows are examined.
func TestClearStaleLeasesRemovesALeaseWhoseProcessIsGone(t *testing.T) {
	st, ctx := testStore(t)

	host, _ := os.Hostname()
	future := timeText(time.Now().UTC().Add(time.Hour))
	// A PID that cannot be running: above the maximum on every platform
	// feather targets.
	_, err := st.DB().ExecContext(ctx, `
		INSERT INTO job_leases (config_name, holder, host, pid, acquired_at, expires_at)
		VALUES (?,?,?,?,?,?)`,
		"orders-archive", "crashed/4194305/1", host, 4194305, nowText(), future)
	if err != nil {
		t.Fatal(err)
	}

	cleared, err := st.ClearStaleLeases(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 1 {
		t.Errorf("cleared %d leases, want the one whose process had died", cleared)
	}
	if _, ok, err := st.TryLockJob(ctx, "orders-archive"); err != nil || !ok {
		t.Fatalf("a crashed process blocked the config for its whole TTL: %v %v", ok, err)
	}
}

// Timestamps are stored as text and ordered as text, so the two orderings have
// to be the same thing. They were not: Go's ".999" layout trims trailing zeros,
// writing a whole second as "12:00:00Z" and five milliseconds later as
// "12:00:00.005Z" — and '.' sorts before 'Z', so the later instant compared as
// earlier. It surfaced as a run list that occasionally showed the wrong "last
// run", and it silently misanchored interval schedules, which read the last
// success time to decide when to fire next.
func TestStoredTimestampsSortChronologically(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	instants := []time.Time{
		base,                             // exactly on the second: the failing case
		base.Add(5 * time.Millisecond),   // .005
		base.Add(100 * time.Millisecond), // .1  under the old layout
		base.Add(999 * time.Millisecond),
		base.Add(time.Second),
		base.Add(time.Minute),
	}

	for i := 1; i < len(instants); i++ {
		earlier, later := timeText(instants[i-1]), timeText(instants[i])
		if earlier >= later {
			t.Errorf("%s (%s) does not sort before %s (%s)",
				earlier, instants[i-1].Format(time.RFC3339Nano),
				later, instants[i].Format(time.RFC3339Nano))
		}
		if len(earlier) != len(later) {
			t.Errorf("stored timestamps are not fixed width: %q vs %q", earlier, later)
		}
	}
}

// The same property, end to end through SQLite's ORDER BY rather than Go's
// string comparison.
func TestRunsComeBackInTheOrderTheyHappened(t *testing.T) {
	st, ctx := testStore(t)

	const runs = 25
	var ids []uuid.UUID
	for i := range runs {
		run := &Run{ConfigName: "orders-archive", Database: "shop", Collection: "orders",
			FilterJSON: "{}", Trigger: TriggerCron, Host: "test"}
		if err := st.StartRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		run.Status = RunSuccess
		run.DocsArchived = int64(i + 1)
		if err := st.FinishRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
		// Land on a variety of sub-second offsets, including whole seconds.
		time.Sleep(time.Duration(i%7) * time.Millisecond)
	}

	listed, err := st.RecentRuns(ctx, "", runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != runs {
		t.Fatalf("listed %d runs, want %d", len(listed), runs)
	}
	// Newest first, so the listing reversed is the order they were created in.
	for i, summary := range listed {
		want := ids[runs-1-i]
		if summary.RunID != want {
			t.Fatalf("position %d is run %s, want %s — runs came back out of order",
				i, summary.RunID, want)
		}
	}
}

func TestLatestRunsPicksTheMostRecentPerConfig(t *testing.T) {
	st, ctx := testStore(t)

	for i := 0; i < 3; i++ {
		run := &Run{ConfigName: "orders-archive", Database: "shop", Collection: "orders",
			FilterJSON: "{}", Trigger: TriggerCron, Host: "test"}
		if err := st.StartRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		run.Status = RunSuccess
		run.DocsArchived = int64(i + 1)
		if err := st.FinishRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	latest, err := st.LatestRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := latest["orders-archive"].DocsArchived; got != 3 {
		t.Fatalf("latest run reports %d documents, want the last one (3)", got)
	}
}

// --- administrators --------------------------------------------------------

func TestSetupJourneyAndAuthentication(t *testing.T) {
	st, ctx := testStore(t)

	needsSetup, err := st.NeedsSetup(ctx)
	if err != nil || !needsSetup {
		t.Fatalf("a fresh install should need setup: %v %v", needsSetup, err)
	}

	if _, err := st.CreateAdmin(ctx, "ops@example.com", "short"); err == nil {
		t.Fatal("expected a short password to be refused")
	}
	if _, err := st.CreateAdmin(ctx, "not-an-email", "correct-horse-battery"); err == nil {
		t.Fatal("expected an invalid email to be refused")
	}

	user, err := st.CreateAdmin(ctx, "Ops@Example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "ops@example.com" {
		t.Fatalf("email was not normalised: %q", user.Email)
	}
	if strings.Contains(user.PasswordHash, "correct-horse") {
		t.Fatal("the password was stored in the clear")
	}

	needsSetup, err = st.NeedsSetup(ctx)
	if err != nil || needsSetup {
		t.Fatal("setup should be complete once an administrator exists")
	}

	// Case-insensitive email, exact password.
	if _, err := st.Authenticate(ctx, "OPS@example.com", "correct-horse-battery"); err != nil {
		t.Fatalf("valid credentials were rejected: %v", err)
	}
	if _, err := st.Authenticate(ctx, "ops@example.com", "wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password gave %v", err)
	}
	// An unknown account must fail the same way, so the form cannot be used to
	// discover which emails exist.
	if _, err := st.Authenticate(ctx, "nobody@example.com", "correct-horse-battery"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown account gave %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	st, ctx := testStore(t)

	user, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}

	token, session, err := st.CreateSession(ctx, user.ID, "curl/8", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || session.CSRFToken == "" {
		t.Fatal("session is missing its tokens")
	}

	// Only the hash is stored, so a stolen database cannot be replayed.
	var storedHash string
	if err := st.db.QueryRowContext(ctx,
		`SELECT token_hash FROM admin_sessions LIMIT 1`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == token {
		t.Fatal("the raw session token was stored")
	}

	found, foundUser, err := st.SessionByToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if foundUser.ID != user.ID || found.CSRFToken != session.CSRFToken {
		t.Fatal("the session did not resolve to its owner")
	}

	if err := st.DeleteSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionByToken(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted session still resolves: %v", err)
	}
}

// An expired session must not be usable, even before the purge runs.
func TestExpiredSessionIsRejected(t *testing.T) {
	st, ctx := testStore(t)

	user, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreateSession(ctx, user.ID, "curl/8", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.db.ExecContext(ctx,
		`UPDATE admin_sessions SET expires_at = ?`, timeText(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.SessionByToken(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired session was accepted: %v", err)
	}
}

func TestResetPasswordSignsOutExistingSessions(t *testing.T) {
	st, ctx := testStore(t)

	user, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreateSession(ctx, user.ID, "curl/8", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.ResetPassword(ctx, "ops@example.com", "another-good-passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionByToken(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatal("the old session survived a password reset")
	}
	if _, err := st.Authenticate(ctx, "ops@example.com", "another-good-passphrase"); err != nil {
		t.Fatalf("the new password does not work: %v", err)
	}
}

// Deleting the last administrator would lock everyone out with no way back in
// from the UI.
func TestCannotDeleteTheOnlyAdministrator(t *testing.T) {
	st, ctx := testStore(t)

	user, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAdmin(ctx, user.ID); err == nil {
		t.Fatal("expected removing the only administrator to be refused")
	}

	second, err := st.CreateAdmin(ctx, "second@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAdmin(ctx, second.ID); err != nil {
		t.Fatalf("removing a second administrator: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()

	key, err := secrets.GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.Resolve(key, "")
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "feather.db")
	st, err := Open(ctx, path, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return st, ctx
}

func seedAccounts(t *testing.T, st *Store, ctx context.Context) (MongoSourceRecord, DestinationRecord) {
	t.Helper()

	source := MongoSourceRecord{Alias: "prod", URI: "mongodb://user:secret@127.0.0.1:27017", Enabled: true}
	if err := st.SaveMongoSource(ctx, &source); err != nil {
		t.Fatal(err)
	}

	destination := DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "cold", Provider: config.ProviderS3, Region: "ap-south-1", Bucket: "archive",
		AccessKeyID: "AKIA", SecretAccessKey: "secret",
	}}
	if err := st.SaveDestination(ctx, &destination); err != nil {
		t.Fatal(err)
	}
	return source, destination
}

func newTestJob(name string, deleteAfter bool) *config.JobConfig {
	enabled := true
	return &config.JobConfig{
		Name:    name,
		Enabled: &enabled,
		Source: config.Source{
			MongoSource: "prod",
			Database:    "shop",
			Collection:  "orders",
			Filter:      json.RawMessage(`{"status":"completed"}`),
		},
		Batch:        config.Batch{MaxDocs: 10000, MaxBytes: 8 << 20},
		Output:       config.Output{KeyTemplate: "archive/{db}/{collection}/{config}-{runId}-{seq}{ext}"},
		Destinations: []string{"cold"},
		Retention:    config.Retention{DeleteAfterArchive: deleteAfter},
		Schedule:     config.Schedule{Every: "1d", At: "02:30", Timezone: "Asia/Kolkata"},
	}
}

func seedJob(t *testing.T, st *Store, ctx context.Context, name string, deleteAfter bool) JobRecord {
	t.Helper()

	record := JobRecord{Config: newTestJob(name, deleteAfter)}
	if err := st.SaveJob(ctx, &record); err != nil {
		t.Fatal(err)
	}
	if record.ID == "" || record.Config.SHA256 == "" {
		t.Fatal("saving did not populate the record")
	}
	_ = uuid.MustParse(record.ID)
	return record
}

const testServiceAccountKey = `{"type":"service_account","client_email":"feather@example.iam.gserviceaccount.com"}`

// Drive credentials go through the same sealing as everything else, and an edit
// that leaves the secret fields blank must not wipe them.
func TestGoogleDriveCredentialsAreEncryptedAndKeptOnEdit(t *testing.T) {
	st, ctx := testStore(t)

	record := DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "drive", Provider: config.ProviderGDrive, DriveFolderID: "1AbCfolder",
		OAuthClientID: "id.apps.googleusercontent.com", OAuthClientSecret: "client-secret",
		OAuthRefreshToken: "1//refresh-token",
	}}
	if err := st.SaveDestination(ctx, &record); err != nil {
		t.Fatal(err)
	}

	var clientSecret, refreshToken []byte
	if err := st.db.QueryRowContext(ctx,
		`SELECT oauth_client_secret_enc, oauth_refresh_token_enc FROM destinations WHERE id = ?`,
		record.ID).Scan(&clientSecret, &refreshToken); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clientSecret), "client-secret") || strings.Contains(string(refreshToken), "refresh-token") {
		t.Fatal("Google credentials are readable in the database")
	}

	loaded, err := st.DestinationByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OAuthRefreshToken != "1//refresh-token" || loaded.OAuthClientSecret != "client-secret" {
		t.Fatalf("credentials did not survive the round trip: %+v", loaded.Destination)
	}
	if loaded.DriveFolderID != "1AbCfolder" {
		t.Fatalf("the folder is %q", loaded.DriveFolderID)
	}

	// The edit form never renders the secrets back, so a save without them is
	// the normal case rather than a request to forget them.
	edit := loaded
	edit.OAuthClientSecret, edit.OAuthRefreshToken = "", ""
	edit.Note = "disaster recovery"
	if err := st.SaveDestination(ctx, &edit); err != nil {
		t.Fatal(err)
	}
	again, err := st.DestinationByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.OAuthRefreshToken != "1//refresh-token" || again.OAuthClientSecret != "client-secret" {
		t.Fatal("editing a Drive destination erased its credentials")
	}
}

// An OAuth destination is savable before it is authorised — the authorisation
// starts from the saved account — but nothing may write to it until it is.
func TestUnconnectedDriveDestinationIsSavedButNotOffered(t *testing.T) {
	st, ctx := testStore(t)

	record := DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "drive", Provider: config.ProviderGDrive,
		OAuthClientID: "id.apps.googleusercontent.com", OAuthClientSecret: "client-secret",
	}}
	if err := st.SaveDestination(ctx, &record); err != nil {
		t.Fatalf("an unconnected Drive account could not be saved: %v", err)
	}

	usable, err := st.DestinationMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := usable["drive"]; ok {
		t.Fatal("an unconnected Drive destination is offered to configs")
	}

	// Connecting is what the OAuth callback does.
	record.OAuthRefreshToken = "1//refresh-token"
	if err := st.SaveDestination(ctx, &record); err != nil {
		t.Fatal(err)
	}
	usable, err = st.DestinationMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := usable["drive"]; !ok {
		t.Fatal("a connected Drive destination is still not offered")
	}

	if err := st.ClearDriveAuthorisation(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	after, err := st.DestinationByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OAuthRefreshToken != "" || after.Ready() {
		t.Fatal("disconnecting left the destination authorised")
	}
	if after.OAuthClientID == "" {
		t.Fatal("disconnecting forgot the OAuth client too, so reconnecting means retyping it")
	}
}

// Switching an existing S3 account to Drive must not leave a bucket and region
// behind for the UI and the catalogue to show.
func TestSwitchingProviderToDriveClearsTheS3Fields(t *testing.T) {
	st, ctx := testStore(t)
	_, destination := seedAccounts(t, st, ctx)

	destination.Provider = config.ProviderGDrive
	destination.ServiceAccountJSON = testServiceAccountKey
	destination.DriveFolderID = "1AbCfolder"
	if err := st.SaveDestination(ctx, &destination); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.DestinationByID(ctx, destination.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Bucket != "" || loaded.Region != "" || loaded.StorageClass != "" {
		t.Fatalf("S3 fields survived the switch: %+v", loaded.Destination)
	}
}

func TestLocationURLNamesEachProvidersAddress(t *testing.T) {
	cases := []struct {
		provider, endpoint, bucket, key, want string
	}{
		{"s3", "", "archive", "feather/orders.ndjson.gz", "s3://archive/feather/orders.ndjson.gz"},
		{"oci", "https://ns.compat.objectstorage.ap-mumbai-1.oraclecloud.com/", "cold", "orders.ndjson.gz",
			"https://ns.compat.objectstorage.ap-mumbai-1.oraclecloud.com/cold/orders.ndjson.gz"},
		{"gdrive", "", "1AbCfolder", "feather/shop/orders.ndjson.gz", "gdrive://1AbCfolder/feather/shop/orders.ndjson.gz"},
	}
	for _, tc := range cases {
		if got := LocationURL(tc.provider, tc.endpoint, tc.bucket, tc.key); got != tc.want {
			t.Errorf("%s copy is at %q, want %q", tc.provider, got, tc.want)
		}
	}
}

// Editing a destination whose endpoint is not AWS never re-sends its secret,
// so validation has to judge the account as stored plus what was posted, not
// the posted half on its own.
func TestEditingAnOracleDestinationWithoutRetypingItsSecret(t *testing.T) {
	st, ctx := testStore(t)

	record := DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "oci-cold", Provider: config.ProviderOCI, Region: "ap-mumbai-1", Bucket: "archive",
		Endpoint:    "https://ns.compat.objectstorage.ap-mumbai-1.oraclecloud.com",
		AccessKeyID: "key", SecretAccessKey: "customer-secret",
	}}
	if err := st.SaveDestination(ctx, &record); err != nil {
		t.Fatal(err)
	}

	edit := record
	edit.SecretAccessKey = ""
	edit.Prefix = "cold"
	if err := st.SaveDestination(ctx, &edit); err != nil {
		t.Fatalf("editing rejected an account whose secret is already stored: %v", err)
	}

	loaded, err := st.DestinationByID(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SecretAccessKey != "customer-secret" {
		t.Errorf("the stored secret became %q", loaded.SecretAccessKey)
	}
	if loaded.Prefix != "cold" {
		t.Errorf("the edit did not save: prefix is %q", loaded.Prefix)
	}
}
