// Package app assembles the running pieces of feather — the database, the
// MongoDB connections, the destination clients, the runner and the scheduler —
// and keeps them in step with what is stored in the database.
//
// Everything is reloadable. An operator adding an account or a retention config
// in the web UI expects it to take effect, not to require a restart.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/mongox"
	"github.com/unifers/feather/internal/notify"
	"github.com/unifers/feather/internal/runner"
	"github.com/unifers/feather/internal/scheduler"
	"github.com/unifers/feather/internal/secrets"
	"github.com/unifers/feather/internal/store"
)

// App is the assembled process.
type App struct {
	Env       *config.Env
	Log       *slog.Logger
	Store     *store.Store
	Mongo     *mongox.Pool
	Registry  *dest.Registry
	Runner    *runner.Runner
	Scheduler *scheduler.Scheduler
	Host      string

	// baseCtx outlives any single HTTP request, so a run started from the UI is
	// not cancelled when the browser gets its response. It is also the context
	// every scheduled run gets, so that a run's lifetime does not depend on
	// which code path started it.
	baseCtx context.Context

	// inFlight counts runs this process owns, whoever started them. Close waits
	// on it: a run interrupted between its DeleteMany and its catalogue write
	// leaves documents gone from MongoDB with nothing recording where they
	// went, which is the worst state feather can be left in.
	inFlight sync.WaitGroup

	mu      sync.Mutex
	running map[string]time.Time
}

// Open connects the database, unseals the credentials in it, and builds the
// live clients. The scheduler is created but not started.
func Open(ctx context.Context, env *config.Env, log *slog.Logger) (*App, error) {
	box, err := secrets.Resolve(env.MasterKey, env.MasterKeyPath)
	if err != nil {
		return nil, err
	}

	st, err := store.Open(ctx, env.DBPath, box)
	if err != nil {
		return nil, err
	}
	st.WithLogger(log)
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, err
	}

	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}

	a := &App{
		Env:     env,
		Log:     log,
		Store:   st,
		Host:    host,
		baseCtx: context.WithoutCancel(ctx),
		running: map[string]time.Time{},
	}

	sources, err := st.MongoSourceMap(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	destinations, err := st.DestinationMap(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}

	a.Mongo = mongox.NewPool(sources)
	registry, err := dest.NewRegistry(ctx, destinations)
	if err != nil {
		st.Close()
		return nil, err
	}
	a.Registry = registry
	a.Runner = runner.New(env, a.Mongo, st, registry, log)

	jobs, err := st.Jobs(ctx)
	if err != nil {
		a.Close(ctx)
		return nil, err
	}
	maxConcurrent, err := st.MetaInt(ctx, MetaMaxConcurrentJobs, env.MaxConcurrentJobs)
	if err != nil {
		a.Close(ctx)
		return nil, err
	}
	sched, err := scheduler.New(ctx, a.baseCtx, jobs, a.Runner, st, maxConcurrent, log)
	if err != nil {
		a.Close(ctx)
		return nil, err
	}
	sched.TrackRunsWith(a.Track)
	a.Scheduler = sched

	if err := a.applyNotifier(ctx); err != nil {
		a.Close(ctx)
		return nil, err
	}

	// A parked config archives nothing. It must not be a line the operator has
	// to go looking for, so it is reported at error level, by name, with the
	// reason — every time the process starts.
	if broken, err := st.BrokenJobs(ctx); err != nil {
		log.Warn("could not check for parked configs", "error", err)
	} else {
		for name, problem := range broken {
			log.Error("retention config is parked and will not run until it is fixed",
				"config", name, "problem", problem)
		}
	}

	log.Info("feather ready",
		"database", st.Path(),
		"master_key", box.Source,
		"mongo_accounts", len(sources),
		"destinations", len(destinations),
		"configs", len(jobs))
	return a, nil
}

// Settings keys the UI can change without a restart.
const (
	MetaMaxConcurrentJobs = "max_concurrent_jobs"
	MetaInstanceName      = "instance_name"
	MetaWebhookURL        = "notify_webhook_url"
	MetaNotifyOnSuccess   = "notify_on_success"
)

// Reload re-reads accounts and configs from the database and swaps them into
// the running process. Called after every change made through the UI.
func (a *App) Reload(ctx context.Context) error {
	sources, err := a.Store.MongoSourceMap(ctx)
	if err != nil {
		return err
	}
	destinations, err := a.Store.DestinationMap(ctx)
	if err != nil {
		return err
	}
	jobs, err := a.Store.Jobs(ctx)
	if err != nil {
		return err
	}

	if err := a.Registry.Replace(ctx, destinations); err != nil {
		return err
	}
	a.Mongo.Replace(ctx, sources)
	if err := a.Scheduler.Reload(a.baseCtx, jobs); err != nil {
		return err
	}

	if err := a.applyNotifier(ctx); err != nil {
		return err
	}

	a.Log.Info("configuration reloaded",
		"mongo_accounts", len(sources), "destinations", len(destinations), "configs", len(jobs))
	return nil
}

// applyNotifier reads the notification settings and swaps them into the runner,
// so a corrected webhook takes effect without a restart.
func (a *App) applyNotifier(ctx context.Context) error {
	url, err := a.Store.Meta(ctx, MetaWebhookURL, "")
	if err != nil {
		return err
	}
	onSuccess, err := a.Store.MetaInt(ctx, MetaNotifyOnSuccess, 0)
	if err != nil {
		return err
	}
	a.Runner.SetNotifier(notify.New(notify.Settings{
		WebhookURL: url,
		OnSuccess:  onSuccess == 1,
	}, a.Log))
	return nil
}

// StartRun launches one retention job in the background and reports whether it
// started. It returns immediately: an archive can take hours, and the caller is
// usually an HTTP handler.
func (a *App) StartRun(name string, trigger store.TriggerKind, dryRun bool) (bool, error) {
	record, err := a.Store.JobByName(a.baseCtx, name)
	if err != nil {
		return false, err
	}
	job := record.Config
	if dryRun {
		job.DryRun = true
	}

	a.mu.Lock()
	if _, busy := a.running[name]; busy {
		a.mu.Unlock()
		return false, nil
	}
	a.running[name] = time.Now()
	a.mu.Unlock()

	a.inFlight.Add(1)
	go func() {
		defer a.inFlight.Done()
		defer func() {
			a.mu.Lock()
			delete(a.running, name)
			a.mu.Unlock()
		}()

		if _, err := a.Runner.Run(a.baseCtx, job, trigger); err != nil {
			a.Log.Error("run failed", "config", name, "error", err)
		}
	}()
	return true, nil
}

// Track registers a run this process owns so that Close waits for it. The
// scheduler calls it; StartRun does the same thing inline.
func (a *App) Track() func() {
	a.inFlight.Add(1)
	return a.inFlight.Done
}

// Drain waits for every run this process owns to finish, or for the deadline to
// pass. It reports whether everything finished.
//
// Being interrupted mid-run is not itself dangerous — the ordering means
// nothing is deleted until it is stored and verified. What is dangerous is
// being interrupted in the narrow window after a DeleteMany and before the
// catalogue records it, which leaves documents gone with no record of the
// archive holding them. Waiting closes that window in the normal case.
func (a *App) Drain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		a.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		a.mu.Lock()
		names := make([]string, 0, len(a.running))
		for name := range a.running {
			names = append(names, name)
		}
		a.mu.Unlock()
		a.Log.Warn("gave up waiting for in-flight runs", "configs", names)
		return false
	}
}

// Housekeeping closes run rows a previous process left open, clears its leases,
// and drops expired sessions. Called once at startup.
func (a *App) Housekeeping(ctx context.Context) {
	// Leases first, and the order is load-bearing: FailStaleRuns treats a
	// surviving lease as proof that the run behind it is still alive, which is
	// only true once the abandoned ones have been cleared.
	if cleared, err := a.Store.ClearStaleLeases(ctx, a.Host); err != nil {
		a.Log.Warn("could not clear stale job leases", "error", err)
	} else if cleared > 0 {
		a.Log.Info("cleared stale job leases", "count", cleared)
	}
	if failed, err := a.Store.FailStaleRuns(ctx, a.Host); err != nil {
		a.Log.Warn("could not close stale run rows", "error", err)
	} else if failed > 0 {
		a.Log.Warn("closed run rows left open by a previous process", "count", failed)
	}
	a.purge(ctx)
}

// AuditRetention is how long the activity trail is kept. Long enough to answer
// "what happened last quarter?", bounded so a years-old installation does not
// carry every click it has ever seen.
const AuditRetention = 365 * 24 * time.Hour

// purge drops what has aged out. Called at startup and then on a timer — doing
// it only at startup meant a process that stayed up for months never pruned
// anything, which is precisely the process that most needed to.
func (a *App) purge(ctx context.Context) {
	if purged, err := a.Store.PurgeExpiredSessions(ctx); err != nil {
		a.Log.Warn("could not purge expired sessions", "error", err)
	} else if purged > 0 {
		a.Log.Debug("purged expired sessions", "count", purged)
	}
	if purged, err := a.Store.PurgeAuditBefore(ctx, time.Now().Add(-AuditRetention)); err != nil {
		a.Log.Warn("could not purge old audit events", "error", err)
	} else if purged > 0 {
		a.Log.Info("purged audit events older than a year", "count", purged)
	}
}

// StartHousekeeping runs the periodic cleanup until ctx is done.
func (a *App) StartHousekeeping(ctx context.Context) {
	a.inFlight.Add(1)
	go func() {
		defer a.inFlight.Done()

		ticker := time.NewTicker(housekeepingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.purge(a.baseCtx)
			}
		}
	}()
}

// housekeepingInterval is deliberately slow: none of this is urgent, and every
// tick takes the single SQLite connection for a moment.
const housekeepingInterval = 6 * time.Hour

// Close shuts everything down, waiting for in-flight runs first.
//
// The order is the point. Closing the Mongo pool and the database out from
// under a running archive is how a run ends up having deleted documents it
// never got to catalogue, so the wait comes first and the teardown second.
func (a *App) Close(ctx context.Context) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	if !a.Drain(shutdownCtx) {
		a.Log.Warn("closing the database with runs still in flight")
	}

	if a.Mongo != nil {
		a.Mongo.Close(shutdownCtx)
	}
	if a.Store != nil {
		a.Store.Close()
	}
}

// shutdownGrace bounds the wait for in-flight runs. It is comfortably under the
// TimeoutStopSec in deploy/feather.service, so systemd's SIGKILL is the
// backstop rather than the normal path.
const shutdownGrace = 2 * time.Minute

// Catalog is the set of accounts a config may name, for validation and for
// populating the forms in the UI.
func (a *App) Catalog(ctx context.Context) (config.Catalog, error) {
	sources, err := a.Store.MongoSourceMap(ctx)
	if err != nil {
		return config.Catalog{}, err
	}
	destinations, err := a.Store.DestinationMap(ctx)
	if err != nil {
		return config.Catalog{}, err
	}
	return config.Catalog{Sources: sources, Destinations: destinations}, nil
}

// MaxConcurrentJobs is the effective concurrency cap: the stored setting if
// there is one, otherwise the process default.
func (a *App) MaxConcurrentJobs(ctx context.Context) int {
	value, err := a.Store.MetaInt(ctx, MetaMaxConcurrentJobs, a.Env.MaxConcurrentJobs)
	if err != nil || value < 1 {
		return a.Env.MaxConcurrentJobs
	}
	return value
}

// Describe is a one-line summary for startup logs.
func (a *App) Describe() string {
	return fmt.Sprintf("database=%s spool=%s", a.Store.Path(), a.Env.SpoolDir)
}

// Check is one readiness probe's verdict.
type Check struct {
	Name string
	Err  error
}

// Ready reports whether feather can actually do its job.
//
// The old health endpoint returned a constant string without touching
// anything, so a feather whose database had been deleted or whose master key
// had been swapped reported healthy right up until the next run failed. These
// are the two dependencies whose absence means nothing can be archived.
func (a *App) Ready(ctx context.Context) []Check {
	checks := []Check{{Name: "database", Err: a.Store.Ping(ctx)}}

	// A key that does not open what is sealed with it is the failure that
	// otherwise appears much later, as a decrypt error on whichever account
	// happened to be read first — by which point the operator is debugging the
	// wrong thing.
	checks = append(checks, Check{Name: "master_key", Err: a.Store.CheckMasterKey(ctx)})
	return checks
}

// Running is a snapshot of the runs this process owns, by config name and when
// each started.
func (a *App) Running() map[string]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make(map[string]time.Time, len(a.running))
	for name, started := range a.running {
		out[name] = started
	}
	return out
}
