package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/runner"
	"github.com/unifers/feather/internal/store"
)

// Scheduler fires retention jobs on their configured schedules. Its set of jobs
// is not fixed: an operator adding a config in the UI reloads it in place.
type Scheduler struct {
	runner *runner.Runner
	store  *store.Store
	log    *slog.Logger
	slots  chan struct{}

	// runCtx is the context every scheduled run gets, fixed for the life of the
	// scheduler. It is deliberately not the context passed to New or Reload:
	// those are request-scoped, and capturing one of them in a cron closure made
	// a job's cancellability depend on whether anyone had saved a config since
	// boot. A job scheduled at startup was killed instantly by SIGTERM while the
	// same job after any UI edit could not be cancelled at all.
	runCtx context.Context

	// track registers an in-flight run with the process so shutdown waits for
	// it. May be nil, which means nothing is tracking.
	track func() func()

	mu      sync.Mutex
	cron    *cron.Cron
	entries []*entry
	started bool

	// pending collects entries while a cron instance is being built, so a
	// failure part way through leaves the running schedule untouched.
	pending []*entry
}

type entry struct {
	job      *config.JobConfig
	schedule cron.Schedule
	interval *intervalSchedule // nil for cron-expression schedules
	id       cron.EntryID
}

// New builds a scheduler over the enabled jobs. maxConcurrent caps how many
// jobs may archive at once, so a VPS is not saturated when several schedules
// coincide.
//
// ctx is used for the setup queries only. runCtx is what scheduled runs get and
// must outlive the process's signal handling — cancelling a run the instant
// SIGTERM arrives cuts it off mid-upload, and the shutdown drain then has
// nothing left to wait for. Shutdown is Stop plus App.Drain, not cancellation.
//
// An empty job list is fine: a fresh install has no configs until someone
// creates one, and the server still has to come up so they can.
func New(ctx, runCtx context.Context, jobs []*config.JobConfig, r *runner.Runner, st *store.Store, maxConcurrent int, log *slog.Logger) (*Scheduler, error) {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	s := &Scheduler{
		runner: r,
		store:  st,
		log:    log,
		slots:  make(chan struct{}, maxConcurrent),
		cron:   cron.New(),
		runCtx: runCtx,
	}
	if err := s.load(ctx, s.cron, jobs); err != nil {
		return nil, err
	}
	s.entries = s.takePending()
	return s, nil
}

// TrackRunsWith registers a hook called around every scheduled run, so the
// process can wait for in-flight work at shutdown.
func (s *Scheduler) TrackRunsWith(track func() func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.track = track
}

// load registers every enabled job on the given cron instance.
func (s *Scheduler) load(ctx context.Context, c *cron.Cron, jobs []*config.JobConfig) error {
	s.pending = nil
	for _, job := range jobs {
		if !job.IsEnabled() {
			s.log.Debug("config disabled, not scheduling", "config", job.Name)
			continue
		}
		if err := s.add(ctx, c, job); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) takePending() []*entry {
	out := s.pending
	s.pending = nil
	return out
}

func (s *Scheduler) add(ctx context.Context, c *cron.Cron, job *config.JobConfig) error {
	e := &entry{job: job}

	if job.Schedule.Cron != "" {
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
		parsed, err := parser.Parse(job.Schedule.Cron)
		if err != nil {
			return fmt.Errorf("config %s: schedule.cron %q: %w", job.Name, job.Schedule.Cron, err)
		}
		e.schedule = timezoned{inner: parsed, loc: job.Schedule.Location}
	} else {
		// Anchor the interval on the last completed run so restarts do not
		// reset the cycle.
		lastRun, _, err := s.store.LastSuccessAt(ctx, job.Name)
		if err != nil {
			return fmt.Errorf("config %s: reading last run time: %w", job.Name, err)
		}
		e.interval = newIntervalSchedule(
			job.Schedule.EveryDuration, job.Schedule.AtHour, job.Schedule.AtMinute,
			job.Schedule.Location, lastRun)
		e.schedule = e.interval
	}

	// s.runCtx, not the ctx this setup call was given: see the field comment.
	e.id = c.Schedule(e.schedule, cron.FuncJob(func() { s.fire(e) }))
	s.pending = append(s.pending, e)

	s.log.Info("scheduled",
		"config", job.Name,
		"schedule", describeSchedule(job.Schedule),
		"next_run", e.peek(time.Now()).Format(time.RFC3339))
	return nil
}

// Reload rebuilds the schedule from a new set of configs. Runs already in
// flight are left alone — they hold their own job lease and finish normally.
func (s *Scheduler) Reload(ctx context.Context, jobs []*config.JobConfig) error {
	fresh := cron.New()

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.load(ctx, fresh, jobs); err != nil {
		// The previous schedule keeps running; the operator sees the error and
		// fixes the config rather than losing every schedule to one bad edit.
		s.pending = nil
		return err
	}

	previous := s.cron
	s.cron = fresh
	s.entries = s.takePending()
	if s.started {
		fresh.Start()
	}
	// Stop returns once no new fires will start; in-flight ones are not waited
	// on here, since that would block an HTTP request behind a running archive.
	go previous.Stop()
	return nil
}

// Start begins firing schedules.
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.cron.Start()
}

// Stop halts new fires and waits for in-flight runs to finish, bounded by the
// caller's context.
func (s *Scheduler) Stop(ctx context.Context) {
	s.mu.Lock()
	c := s.cron
	s.started = false
	s.mu.Unlock()

	stopped := c.Stop()
	select {
	case <-stopped.Done():
	case <-ctx.Done():
		s.log.Warn("shutdown timed out waiting for a run to finish")
	}
}

// NextRuns reports each job's next fire time, for `feather list` and the UI.
func (s *Scheduler) NextRuns() map[string]time.Time {
	s.mu.Lock()
	entries := append([]*entry(nil), s.entries...)
	s.mu.Unlock()

	out := make(map[string]time.Time, len(entries))
	now := time.Now()
	for _, e := range entries {
		out[e.job.Name] = e.peek(now)
	}
	return out
}

// peek reports the next fire time without disturbing the schedule's state.
func (e *entry) peek(now time.Time) time.Time {
	if e.interval != nil {
		return e.interval.preview(now).In(e.job.Schedule.Location)
	}
	return e.schedule.Next(now).In(e.job.Schedule.Location)
}

func (s *Scheduler) fire(e *entry) {
	s.mu.Lock()
	ctx, track := s.runCtx, s.track
	s.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	if track != nil {
		defer track()()
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		// All slots busy. Skipping is right: the filter is relative, so the next
		// fire picks up everything this one would have, plus whatever arrived
		// since. Queueing would only stack work on an already-loaded host.
		s.log.Warn("skipping scheduled run, concurrency limit reached", "config", e.job.Name)
		return
	}

	// The schedule already advanced itself when cron computed the next slot, so
	// a failing job cannot cause a tight retry loop here.
	if _, err := s.runner.Run(ctx, e.job, store.TriggerCron); err != nil && !errors.Is(err, runner.ErrLocked) {
		s.log.Error("scheduled run failed", "config", e.job.Name, "error", err)
	}
}

// timezoned evaluates a cron expression in the job's own timezone, so
// "02:30 Asia/Kolkata" means that regardless of the server's clock setting.
type timezoned struct {
	inner cron.Schedule
	loc   *time.Location
}

func (t timezoned) Next(from time.Time) time.Time {
	return t.inner.Next(from.In(t.loc))
}

func describeSchedule(s config.Schedule) string {
	if s.Cron != "" {
		return fmt.Sprintf("cron %q (%s)", s.Cron, s.Timezone)
	}
	if s.AtHour >= 0 {
		return fmt.Sprintf("every %s at %02d:%02d (%s)", s.Every, s.AtHour, s.AtMinute, s.Timezone)
	}
	return fmt.Sprintf("every %s", s.Every)
}
