package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/runner"
	"github.com/unifers/feather/internal/secrets"
	"github.com/unifers/feather/internal/store"
)

// These tests drive the real scheduler over a real SQLite catalogue and a real
// runner. No job here can reach MongoDB: every fired job either fails the
// runner's pre-flight safety check or finds its lease already taken, both of
// which return before a source is opened.

func TestNewSchedulesOnlyEnabledJobs(t *testing.T) {
	h := newHarness(t)
	disabled := false

	cronJob := h.cronJob("nightly", "30 2 * * *", time.UTC)
	off := h.cronJob("switched-off", "0 3 * * *", time.UTC)
	off.Enabled = &disabled
	intervalJob := h.intervalJob("every-two-days", 48*time.Hour, 2, 30, time.UTC)

	s := h.newScheduler(t, cronJob, off, intervalJob)

	next := s.NextRuns()
	if len(next) != 2 {
		t.Fatalf("got next runs for %v, want exactly the two enabled configs", keys(next))
	}
	for _, name := range []string{"nightly", "every-two-days"} {
		if _, ok := next[name]; !ok {
			t.Errorf("enabled config %q was not scheduled", name)
		}
	}
	if _, ok := next["switched-off"]; ok {
		t.Error("a disabled config was scheduled")
	}
}

// A fresh install has no configs yet, and the server still has to come up so
// someone can create one.
func TestNewAcceptsAnEmptyJobList(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t)

	if next := s.NextRuns(); len(next) != 0 {
		t.Fatalf("an empty scheduler reported next runs: %v", next)
	}
}

func TestNewRaisesAConcurrencyLimitBelowOneToOne(t *testing.T) {
	h := newHarness(t)
	for _, limit := range []int{-3, 0, 1} {
		s, err := New(t.Context(), context.Background(), nil, h.runner, h.store, limit, h.log)
		if err != nil {
			t.Fatal(err)
		}
		if got := cap(s.slots); got != 1 {
			t.Errorf("maxConcurrent %d gave %d slots, want 1", limit, got)
		}
	}

	s, err := New(t.Context(), context.Background(), nil, h.runner, h.store, 4, h.log)
	if err != nil {
		t.Fatal(err)
	}
	if got := cap(s.slots); got != 4 {
		t.Errorf("maxConcurrent 4 gave %d slots", got)
	}
}

func TestNewRejectsAnUnparseableCronExpression(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name string
		expr string
	}{
		{"too few fields", "30 2 *"},
		{"seconds field is not accepted", "0 30 2 * * *"},
		{"out of range hour", "0 25 * * *"},
		{"not a schedule", "whenever"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(t.Context(), context.Background(),
				[]*config.JobConfig{h.cronJob("broken", tc.expr, time.UTC)},
				h.runner, h.store, 1, h.log)
			if err == nil {
				t.Fatalf("cron %q was accepted", tc.expr)
			}
			if !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), tc.expr) {
				t.Errorf("error does not name the config and the expression: %v", err)
			}
		})
	}
}

// An interval is anchored on the catalogue; if the catalogue cannot be read the
// scheduler must say so rather than silently restarting the cycle.
func TestNewReportsACatalogueFailureWhenAnchoringAnInterval(t *testing.T) {
	h := newHarness(t)

	broken := openStore(t)
	broken.Close()

	_, err := New(t.Context(), context.Background(),
		[]*config.JobConfig{h.intervalJob("anchored", 24*time.Hour, -1, -1, time.UTC)},
		h.runner, broken, 1, h.log)
	if err == nil {
		t.Fatal("expected an error reading the last run time from a closed catalogue")
	}
	if !strings.Contains(err.Error(), "reading last run time") {
		t.Errorf("error does not explain what failed: %v", err)
	}
}

// "02:30 Asia/Kolkata" means that, regardless of the server's own timezone.
func TestCronScheduleIsEvaluatedInTheJobsTimezone(t *testing.T) {
	h := newHarness(t)
	ist := mustLoad(t, "Asia/Kolkata")

	s := h.newScheduler(t, h.cronJob("nightly", "30 2 * * *", ist))
	before := time.Now()
	next := s.NextRuns()["nightly"]

	if next.Location().String() != ist.String() {
		t.Errorf("next run reported in %s, want %s", next.Location(), ist)
	}
	if next.Hour() != 2 || next.Minute() != 30 {
		t.Errorf("next run at %s, want 02:30 in %s", next.Format(time.RFC3339), ist)
	}
	if !next.After(before) || next.Sub(before) > 24*time.Hour {
		t.Errorf("next run %s is not within the coming day", next.Format(time.RFC3339))
	}
}

func TestTimezonedScheduleConvertsBeforeAskingTheInnerSchedule(t *testing.T) {
	ist := mustLoad(t, "Asia/Kolkata")
	var seen time.Time
	tz := timezoned{inner: scheduleFunc(func(t time.Time) time.Time {
		seen = t
		return t.Add(time.Hour)
	}), loc: ist}

	from := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	got := tz.Next(from)

	if seen.Location() != ist {
		t.Errorf("inner schedule saw %s, want a time in %s", seen.Location(), ist)
	}
	if !got.Equal(from.Add(time.Hour)) {
		t.Errorf("got %s, want an hour after %s", got, from)
	}
}

// Restarting must not reset an interval: the next run is computed from the last
// run that completed, which the catalogue holds.
func TestIntervalNextRunIsAnchoredOnTheLastSuccessfulRun(t *testing.T) {
	h := newHarness(t)

	run := &store.Run{ConfigName: "six-hourly", Database: "shop", Collection: "orders", Trigger: store.TriggerCron}
	if err := h.store.StartRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunSuccess
	if err := h.store.FinishRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}

	s := h.newScheduler(t, h.intervalJob("six-hourly", 6*time.Hour, -1, -1, time.UTC))
	next := s.NextRuns()["six-hourly"]

	want := run.FinishedAt.Add(6 * time.Hour)
	if diff := next.Sub(want); diff < -time.Millisecond || diff > time.Millisecond {
		t.Errorf("next run %s, want six hours after the last success at %s",
			next.Format(time.RFC3339Nano), run.FinishedAt.Format(time.RFC3339Nano))
	}
}

// NextRuns is called by the UI on every page load. If looking consumed the
// interval's first-fire state, merely viewing the dashboard would change when
// the job actually runs.
func TestNextRunsDoesNotAdvanceAnIntervalSchedule(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t, h.intervalJob("daily", 24*time.Hour, 2, 30, time.UTC))

	first := s.NextRuns()["daily"]
	second := s.NextRuns()["daily"]
	if !first.Equal(second) {
		t.Errorf("two peeks disagreed: %s then %s", first, second)
	}
	if first.Hour() != 2 || first.Minute() != 30 {
		t.Errorf("next run at %s, want the pinned 02:30", first.Format(time.RFC3339))
	}

	s.mu.Lock()
	scheduled := s.entries[0].interval.scheduled
	s.mu.Unlock()
	if scheduled {
		t.Error("peeking marked the interval as scheduled")
	}
}

func TestReloadReplacesTheSetOfScheduledJobs(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t, h.cronJob("old", "0 1 * * *", time.UTC))

	if err := s.Reload(t.Context(), []*config.JobConfig{
		h.cronJob("new", "0 2 * * *", time.UTC),
		h.intervalJob("also-new", time.Hour, -1, -1, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	next := s.NextRuns()
	if _, ok := next["old"]; ok {
		t.Error("a config removed by the reload is still scheduled")
	}
	if len(next) != 2 {
		t.Errorf("after reload got %v, want new and also-new", keys(next))
	}
}

// One bad edit must not cost the operator every schedule they had.
func TestReloadKeepsThePreviousScheduleWhenAConfigIsInvalid(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t, h.cronJob("working", "0 1 * * *", time.UTC))
	before := s.NextRuns()["working"]

	err := s.Reload(t.Context(), []*config.JobConfig{
		h.cronJob("fine", "0 2 * * *", time.UTC),
		h.cronJob("typo", "0 2 * *", time.UTC),
	})
	if err == nil {
		t.Fatal("reload accepted an invalid cron expression")
	}

	next := s.NextRuns()
	if len(next) != 1 || !next["working"].Equal(before) {
		t.Errorf("the previous schedule was disturbed: %v", next)
	}

	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if pending != 0 {
		t.Errorf("%d half-built entries were left pending after a failed reload", pending)
	}
}

func TestStartFiresJobsAndStopWaitsForThem(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t, h.fastJob("fast"))

	var begun, ended atomic.Int64
	s.TrackRunsWith(func() func() {
		begun.Add(1)
		return func() { ended.Add(1) }
	})

	s.Start()
	waitFor(t, "the first scheduled fire", func() bool { return ended.Load() > 0 })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s.Stop(ctx)

	if b, e := begun.Load(), ended.Load(); b != e {
		t.Fatalf("Stop returned with %d runs begun but only %d finished", b, e)
	}
	settled := begun.Load()
	time.Sleep(60 * time.Millisecond) // three intervals of the fast job
	if got := begun.Load(); got != settled {
		t.Errorf("%d more runs fired after Stop", got-settled)
	}

	if !h.logs.contains("scheduled run failed") || !h.logs.contains("{runId}") {
		t.Errorf("the runner was not reached, or its failure was not logged:\n%s", h.logs)
	}
}

// Saving a config in the UI while the daemon is running must leave the new
// schedule firing, not merely registered.
func TestReloadOfAStartedSchedulerStartsTheNewSchedule(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t, h.cronJob("idle", "0 1 1 1 *", time.UTC))

	var fired atomic.Int64
	s.TrackRunsWith(func() func() {
		fired.Add(1)
		return func() {}
	})
	s.Start()
	t.Cleanup(func() { s.Stop(context.Background()) })

	if err := s.Reload(t.Context(), []*config.JobConfig{h.fastJob("fast")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a fire from the reloaded schedule", func() bool { return fired.Load() > 0 })
}

func TestReloadOfAStoppedSchedulerDoesNotStartIt(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t)

	var fired atomic.Int64
	s.TrackRunsWith(func() func() {
		fired.Add(1)
		return func() {}
	})

	if err := s.Reload(t.Context(), []*config.JobConfig{h.fastJob("fast")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if n := fired.Load(); n != 0 {
		t.Fatalf("a scheduler that was never started fired %d times after a reload", n)
	}
}

func TestStopGivesUpWaitingWhenItsContextEnds(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t, h.fastJob("slow"))

	release := make(chan struct{})
	var entered atomic.Int64
	s.TrackRunsWith(func() func() {
		entered.Add(1)
		<-release
		return func() {}
	})
	s.Start()
	waitFor(t, "a run to be in flight", func() bool { return entered.Load() > 0 })

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.Stop(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Stop ignored its context and kept waiting for an in-flight run")
	}
	if !h.logs.contains("shutdown timed out") {
		t.Errorf("a timed-out shutdown was not logged:\n%s", h.logs)
	}

	close(release)
	// Drain properly before the catalogue closes underneath the run.
	s.Stop(context.Background())
}

// When every slot is taken the fire is skipped, not queued: the filter is
// relative, so the next fire picks up whatever this one would have.
func TestFireSkipsWhenEveryConcurrencySlotIsBusy(t *testing.T) {
	h := newHarness(t)
	s := h.newScheduler(t, h.fastJob("busy"))

	var tracked, released atomic.Int64
	s.TrackRunsWith(func() func() {
		tracked.Add(1)
		return func() { released.Add(1) }
	})

	s.slots <- struct{}{} // the only slot
	s.fire(s.entries[0])
	<-s.slots

	if !h.logs.contains("concurrency limit reached") {
		t.Errorf("the skip was not logged:\n%s", h.logs)
	}
	if h.logs.contains("scheduled run failed") {
		t.Error("the runner was called even though no slot was free")
	}
	if tracked.Load() != 1 || released.Load() != 1 {
		t.Errorf("tracking hook: %d begun, %d released; a skipped fire must still balance", tracked.Load(), released.Load())
	}
	if len(s.slots) != 0 {
		t.Error("a skipped fire took a slot and never gave it back")
	}
}

func TestFireLogsAFailedRunButNotALockedOne(t *testing.T) {
	cases := []struct {
		name       string
		job        func(h *harness) *config.JobConfig
		holdLock   bool
		wantLogged bool
	}{
		{
			name:       "a failing run is reported",
			job:        func(h *harness) *config.JobConfig { return h.fastJob("unsafe") },
			wantLogged: true,
		},
		{
			// Another run already holds the lease. That is ordinary overlap,
			// not a failure, and paging someone over it would be noise.
			name: "a run that finds its lease taken is not",
			job: func(h *harness) *config.JobConfig {
				job := h.fastJob("overlapping")
				job.Output.KeyTemplate = config.DefaultKeyTemplate
				return job
			},
			holdLock:   true,
			wantLogged: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			job := tc.job(h)
			s := h.newScheduler(t, job)

			if tc.holdLock {
				lock, ok, err := h.store.TryLockJob(t.Context(), job.Name)
				if err != nil || !ok {
					t.Fatalf("taking the lease for the test: ok=%v err=%v", ok, err)
				}
				defer lock.Release(context.Background())
			}

			s.fire(s.entries[0])

			if got := h.logs.contains("scheduled run failed"); got != tc.wantLogged {
				t.Errorf("failure logged = %v, want %v:\n%s", got, tc.wantLogged, h.logs)
			}
			if len(s.slots) != 0 {
				t.Error("the run's concurrency slot was not returned")
			}
		})
	}
}

// A scheduler built without a run context must still be able to fire.
func TestFireFallsBackToABackgroundContext(t *testing.T) {
	h := newHarness(t)
	job := h.fastJob("no-context")
	s, err := New(t.Context(), nil, []*config.JobConfig{job}, h.runner, h.store, 1, h.log)
	if err != nil {
		t.Fatal(err)
	}

	s.fire(s.entries[0])
	if !h.logs.contains("scheduled run failed") {
		t.Errorf("the runner was not reached:\n%s", h.logs)
	}
}

func TestDescribeSchedule(t *testing.T) {
	cases := []struct {
		name     string
		schedule config.Schedule
		want     string
	}{
		{
			name:     "cron expression with its timezone",
			schedule: config.Schedule{Cron: "30 2 * * *", Timezone: "Asia/Kolkata"},
			want:     `cron "30 2 * * *" (Asia/Kolkata)`,
		},
		{
			name:     "interval pinned to a time of day",
			schedule: config.Schedule{Every: "48h", AtHour: 2, AtMinute: 5, Timezone: "UTC"},
			want:     "every 48h at 02:05 (UTC)",
		},
		{
			name:     "bare interval",
			schedule: config.Schedule{Every: "6h", AtHour: -1, AtMinute: -1, Timezone: "UTC"},
			want:     "every 6h",
		},
		{
			name:     "cron wins over an interval",
			schedule: config.Schedule{Cron: "@daily", Every: "6h", Timezone: "UTC"},
			want:     `cron "@daily" (UTC)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeSchedule(tc.schedule); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Aligning to the pinned time can land on or before t when the interval is
// shorter than a day. Validation forbids that combination, but the schedule
// must still move forward rather than fire in the past.
func TestIntervalNeverSchedulesInThePast(t *testing.T) {
	utc := time.UTC
	s := newIntervalSchedule(time.Hour, 2, 30, utc, time.Time{})

	at := time.Date(2026, 8, 12, 3, 0, 0, 0, utc)
	got := s.after(at)
	want := time.Date(2026, 8, 13, 2, 30, 0, 0, utc)

	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// --- harness ----------------------------------------------------------------

type harness struct {
	store  *store.Store
	runner *runner.Runner
	log    *slog.Logger
	logs   *syncBuffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st := openStore(t)
	t.Cleanup(st.Close)

	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := runner.New(&config.Env{SpoolDir: t.TempDir()}, nil, st, dest.NewRegistryWith(nil), log)

	return &harness{store: st, runner: r, log: log, logs: logs}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()

	key, err := secrets.GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.Resolve(key, "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "feather.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(t.Context()); err != nil {
		st.Close()
		t.Fatal(err)
	}
	return st
}

func (h *harness) newScheduler(t *testing.T, jobs ...*config.JobConfig) *Scheduler {
	t.Helper()
	s, err := New(t.Context(), context.Background(), jobs, h.runner, h.store, 1, h.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(context.Background()) })
	return s
}

func (h *harness) cronJob(name, expr string, loc *time.Location) *config.JobConfig {
	return &config.JobConfig{
		Name:     name,
		Output:   config.Output{KeyTemplate: config.DefaultKeyTemplate},
		Schedule: config.Schedule{Cron: expr, Timezone: loc.String(), Location: loc},
	}
}

func (h *harness) intervalJob(name string, every time.Duration, hour, minute int, loc *time.Location) *config.JobConfig {
	return &config.JobConfig{
		Name:   name,
		Output: config.Output{KeyTemplate: config.DefaultKeyTemplate},
		Schedule: config.Schedule{
			Every: every.String(), Timezone: loc.String(),
			EveryDuration: every, AtHour: hour, AtMinute: minute, Location: loc,
		},
	}
}

// fastJob fires every 20ms. Its key template lacks {runId}, so the runner
// refuses it in pre-flight and returns at once without touching any source.
// Validation would never allow either property; this builds the struct directly.
func (h *harness) fastJob(name string) *config.JobConfig {
	job := h.intervalJob(name, 20*time.Millisecond, -1, -1, time.UTC)
	job.Output.KeyTemplate = "archive/{config}{ext}"
	return job
}

type scheduleFunc func(time.Time) time.Time

func (f scheduleFunc) Next(t time.Time) time.Time { return f(t) }

// syncBuffer is a log sink safe to write from cron goroutines and read from the
// test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) contains(s string) bool { return strings.Contains(b.String(), s) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func keys(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
