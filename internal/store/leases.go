package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

// JobLock is a held claim on one config. Two things must not happen at once:
// the scheduler firing a job while an operator runs it from the UI, and two
// feather processes pointed at the same database both archiving it.
//
// Postgres offered advisory locks that died with the session. SQLite has no
// equivalent, so a lease row with an expiry stands in: a process that crashes
// mid-run leaves a lease that ages out rather than one that blocks the config
// forever.
type JobLock struct {
	store   *Store
	key     string
	holder  string
	stop    chan struct{}
	stopped chan struct{}

	// lost is closed when a renewal finds this lease is no longer ours. A run
	// that has lost its claim must stop: another process now believes it owns
	// the config, and two runs deleting from the same collection at once is
	// exactly what the lease exists to prevent.
	lost     chan struct{}
	lostOnce sync.Once
}

// leaseRenewInterval is how often a live run pushes its expiry out. A lease is
// always granted for several multiples of this, so a busy host that misses a
// renewal or two does not lose its claim.
const (
	leaseRenewInterval = 30 * time.Second
	leaseTTL           = 5 * time.Minute
)

// TryLockJob takes the per-config lease. A false return means another run of
// the same config is already in flight, here or on another host.
func (s *Store) TryLockJob(ctx context.Context, configName string) (*JobLock, bool, error) {
	if _, busy := s.localLocks.LoadOrStore(configName, struct{}{}); busy {
		return nil, false, nil
	}

	host, _ := os.Hostname()
	holder := fmt.Sprintf("%s/%d/%d", host, os.Getpid(), time.Now().UnixNano())
	now := time.Now().UTC()

	// The lease is taken only if there is no row, or the existing one has
	// expired — the WHERE on the upsert is what makes this atomic.
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO job_leases (config_name, holder, host, pid, acquired_at, expires_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT (config_name) DO UPDATE SET
			holder = excluded.holder, host = excluded.host, pid = excluded.pid,
			acquired_at = excluded.acquired_at, expires_at = excluded.expires_at
		WHERE job_leases.expires_at < ?`,
		configName, holder, host, os.Getpid(), timeText(now), timeText(now.Add(leaseTTL)), timeText(now))
	if err != nil {
		s.localLocks.Delete(configName)
		return nil, false, fmt.Errorf("taking job lease for %s: %w", configName, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		s.localLocks.Delete(configName)
		return nil, false, err
	}
	if affected == 0 {
		s.localLocks.Delete(configName)
		return nil, false, nil
	}

	lock := &JobLock{
		store:   s,
		key:     configName,
		holder:  holder,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		lost:    make(chan struct{}),
	}
	go lock.renew(s.log)
	return lock, true, nil
}

// Lost is closed if this lease is taken over by someone else while the run
// still believes it holds it. The runner selects on it and aborts.
func (l *JobLock) Lost() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.lost
}

// renew keeps the lease alive for as long as the run is. Without it a run
// longer than the TTL would have its claim stolen mid-flight.
//
// The result is checked, which it previously was not. A renewal can fail for
// two very different reasons, and only one of them is survivable: the database
// being briefly unavailable is worth another tick, but the update matching zero
// rows means the holder column no longer says us — the lease expired and
// somebody else took it. Continuing to archive and delete without a claim is
// the failure this whole mechanism exists to prevent, so that case gives up the
// run rather than the pretence.
func (l *JobLock) renew(log *slog.Logger) {
	defer close(l.stopped)
	ticker := time.NewTicker(leaseRenewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			result, err := l.store.db.ExecContext(ctx, `
				UPDATE job_leases SET expires_at = ? WHERE config_name = ? AND holder = ?`,
				timeText(time.Now().UTC().Add(leaseTTL)), l.key, l.holder)
			cancel()

			if err != nil {
				// Transient: the lease has several renewal intervals of slack
				// before it expires, so one failed tick is not fatal.
				if log != nil {
					log.Warn("could not renew the job lease", "config", l.key, "error", err)
				}
				continue
			}
			affected, err := result.RowsAffected()
			if err != nil || affected > 0 {
				continue
			}

			if log != nil {
				log.Error("lost the job lease — another process has taken this config; stopping the run",
					"config", l.key, "holder", l.holder)
			}
			l.lostOnce.Do(func() { close(l.lost) })
			return
		}
	}
}

// Release drops the lease. The holder check means a lease that was already
// taken over after an expiry is not deleted out from under its new owner.
func (l *JobLock) Release(ctx context.Context) {
	if l == nil || l.store == nil {
		return
	}
	close(l.stop)
	<-l.stopped

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, _ = l.store.db.ExecContext(releaseCtx,
		`DELETE FROM job_leases WHERE config_name = ? AND holder = ?`, l.key, l.holder)

	l.store.localLocks.Delete(l.key)
	l.store = nil
}

// RunningConfigs lists configs currently holding a live lease, so the UI can
// grey out a "run now" button rather than let it fail.
func (s *Store) RunningConfigs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT config_name FROM job_leases WHERE expires_at >= ?`, nowText())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// ClearStaleLeases drops leases left behind by a process that died, called at
// startup for this host.
//
// It used to be `WHERE host = ? OR expires_at < ?`, which was wrong in a way
// that only showed up with two processes: any feather starting up deleted every
// live lease belonging to any other feather on the same host. Both would then
// archive the same config at once — duplicate objects, double the read load on
// MongoDB, and two DeleteMany passes over the same documents. Restarting into
// an overlapping window, running a second container with the same hostname, or
// hitting the "unknown" hostname fallback were all enough to trigger it.
//
// A lease is now only cleared when it has actually been abandoned: its expiry
// has passed, or it belongs to this host and names a process that is gone.
func (s *Store) ClearStaleLeases(ctx context.Context, host string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM job_leases WHERE expires_at < ?`, nowText())
	if err != nil {
		return 0, err
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	// A crashed process leaves a lease that is still inside its TTL. Only this
	// host can tell whether that PID is alive, so only this host's rows are
	// examined — and never our own, since we have just started and hold none.
	rows, err := s.db.QueryContext(ctx,
		`SELECT config_name, pid FROM job_leases WHERE host = ? AND pid <> ?`, host, os.Getpid())
	if err != nil {
		return cleared, err
	}
	defer func() { _ = rows.Close() }()

	var abandoned []string
	for rows.Next() {
		var name string
		var pid int
		if err := rows.Scan(&name, &pid); err != nil {
			return cleared, err
		}
		if !processAlive(pid) {
			abandoned = append(abandoned, name)
		}
	}
	if err := rows.Err(); err != nil {
		return cleared, err
	}

	for _, name := range abandoned {
		result, err := s.db.ExecContext(ctx,
			`DELETE FROM job_leases WHERE config_name = ? AND host = ?`, name, host)
		if err != nil {
			return cleared, err
		}
		if affected, err := result.RowsAffected(); err == nil {
			cleared += affected
		}
	}
	return cleared, nil
}

// processAlive reports whether a PID on this host still names a running
// process. Signal 0 performs the permission and existence checks without
// delivering anything, which is exactly the question being asked.
//
// It answers true when it cannot tell — a PID owned by another user gives
// EPERM, which proves the process exists. Guessing "alive" is the safe
// direction: the cost is a lease that waits out its TTL, against the cost of
// deleting a live claim and running the config twice.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, os.ErrPermission)
}
