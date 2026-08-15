package store

import (
	"context"
	"fmt"
	"os"
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
	}
	go lock.renew()
	return lock, true, nil
}

// renew keeps the lease alive for as long as the run is. Without it a run
// longer than the TTL would have its claim stolen mid-flight.
func (l *JobLock) renew() {
	defer close(l.stopped)
	ticker := time.NewTicker(leaseRenewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = l.store.db.ExecContext(ctx, `
				UPDATE job_leases SET expires_at = ? WHERE config_name = ? AND holder = ?`,
				timeText(time.Now().UTC().Add(leaseTTL)), l.key, l.holder)
			cancel()
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
func (s *Store) ClearStaleLeases(ctx context.Context, host string) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM job_leases WHERE host = ? OR expires_at < ?`, host, nowText())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
