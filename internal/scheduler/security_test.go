package scheduler

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/runner"
)

// The scheduler is the long-lived half of feather: the web UI lives in the same
// process. A bad job must not take it down, and a schedule must not be able to
// hammer the database or a production collection faster than the rules allow.

// panickingHarness returns a harness whose runner panics as soon as a safe job
// reaches it: it has no store, so taking the lease dereferences nil. That
// stands in for any bug below the scheduler.
func panickingHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.runner = runner.New(&config.Env{SpoolDir: t.TempDir()}, nil, nil, dest.NewRegistryWith(nil), h.log)
	return h
}

func safeFastJob(h *harness, name string) *config.JobConfig {
	job := h.fastJob(name)
	job.Output.KeyTemplate = config.DefaultKeyTemplate // passes pre-flight, so the runner goes on to the lease
	return job
}

func TestAPanickingRunDoesNotEscapeFire(t *testing.T) {
	h := panickingHarness(t)
	s := h.newScheduler(t, safeFastJob(h, "explodes"))

	var tracked, released int
	s.TrackRunsWith(func() func() {
		tracked++
		return func() { released++ }
	})

	var escaped any
	func() {
		defer func() { escaped = recover() }()
		s.fire(s.entries[0])
	}()

	if escaped != nil {
		t.Errorf("a panic in a scheduled run escaped fire (and so the cron goroutine, killing the process): %v", escaped)
	}
	if len(s.slots) != 0 {
		t.Error("the panicking run kept its concurrency slot")
	}
	if tracked != 1 || released != 1 {
		t.Errorf("tracking hook: %d begun, %d released; shutdown would wait forever", tracked, released)
	}
}

const childEnv = "FEATHER_SCHEDULER_PANIC_CHILD"

// The same property end to end: a started scheduler whose job panics must leave
// the process running. Run in a child process, since the failure mode is the
// whole process exiting.
func TestAPanickingScheduledJobDoesNotCrashTheProcess(t *testing.T) {
	if os.Getenv(childEnv) == "1" {
		h := panickingHarness(t)
		s := h.newScheduler(t, safeFastJob(h, "explodes"))
		s.Start()
		time.Sleep(300 * time.Millisecond) // many 20ms fires
		s.Stop(context.Background())
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestAPanickingScheduledJobDoesNotCrashTheProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		first := string(out)
		if i := strings.Index(first, "goroutine "); i > 0 {
			first = first[:i]
		}
		t.Errorf("a panicking scheduled job crashed the process (%v):\n%s", err, first)
	}
}

// Every hostile or degenerate cron expression is refused when the schedule is
// built, naming the config, rather than being accepted and misbehaving later.
func TestHostileCronExpressionsAreRefused(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name, expr string
	}{
		{"a second schedule smuggled after a newline", "30 2 * * *\n* * * * *"},
		{"a NUL byte", "30 2 * * *\x00"},
		{"a zero step", "*/0 * * * *"},
		{"a negative value", "-1 * * * *"},
		{"an enormous range", "0-99999999999999999999 * * * *"},
		{"a timezone that walks the filesystem", "CRON_TZ=../../../../etc/passwd 30 2 * * *"},
		{"an unknown timezone", "TZ=Nowhere/Atlantis 30 2 * * *"},
		{"an unknown descriptor", "@sometimes"},
		{"an empty every", "@every"},
		{"a negative every", "@every -1h"},
		// The interval form refuses anything below a minute; the cron
		// descriptor must not be a way round that rule.
		{"every second", "@every 1s"},
		{"every nanosecond", "@every 1ns"},
		{"every thirty seconds", "@every 30s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(t.Context(), context.Background(),
				[]*config.JobConfig{h.cronJob("hostile", tc.expr, time.UTC)},
				h.runner, h.store, 1, h.log)
			if err == nil {
				t.Errorf("cron %q was accepted", truncateForLog(tc.expr))
			}
		})
	}
}

// A schedule that can never fire must not spin or report a bogus next run in
// the past.
func TestACronThatNeverFiresDoesNotSpin(t *testing.T) {
	h := newHarness(t)
	done := make(chan *Scheduler, 1)
	go func() {
		s, err := New(t.Context(), context.Background(),
			[]*config.JobConfig{h.cronJob("february-30", "0 0 30 2 *", time.UTC)},
			h.runner, h.store, 1, h.log)
		if err != nil {
			done <- nil
			return
		}
		done <- s
	}()

	select {
	case s := <-done:
		if s == nil {
			return // refusing it is fine
		}
		next := s.NextRuns()["february-30"]
		if !next.IsZero() && next.Before(time.Now()) {
			t.Errorf("an impossible schedule reports a next run in the past: %s", next)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("building an impossible schedule did not return")
	}
}

func truncateForLog(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
