// Package pgsrc archives rows out of PostgreSQL tables, and puts them back.
//
// It is the PostgreSQL counterpart of mongox, and the runner drives it through
// the same source.Source contract, so the ordering feather exists for — no row
// is deleted until every copy of its archive is stored and verified — is the
// runner's, unchanged, rather than something re-implemented here.
//
// What differs is underneath:
//
//   - Rows are read by keyset pagination on the primary key, one short query
//     per page, rather than through a cursor held open for the whole run. A
//     run can last hours; a transaction held that long pins the xmin horizon,
//     so autovacuum cannot clean anything newer and the busiest tables bloat
//     for as long as the archive takes.
//   - Each row is archived as row_to_json — Postgres's own rendering of it,
//     which json_populate_record reads back to the same values. Timestamps
//     keep their microseconds and numerics keep every digit.
//   - A table needs a single-column primary key. It is what pages are keyed
//     on, what the run is fenced at, and what rows are deleted by.
package pgsrc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/retry"
)

const (
	connectTimeout = 15 * time.Second
	// A handful of concurrent jobs, each with one page read or one delete in
	// flight. Postgres connections are processes; a small VPS should not hold
	// dozens of them open against a shared database.
	maxConns = 8
)

// Connect opens a pool to one account and checks it answers.
//
// Two session settings are pinned rather than inherited from the server:
//
//   - standard_conforming_strings, because config.LintSQLFilter judges where
//     a '...' literal ends on the assumption that backslash is an ordinary
//     character inside it. With the setting off, a filter could end a literal
//     somewhere the lint did not expect.
//   - TimeZone=UTC, so a timestamp column without a zone is read as UTC when
//     it is recorded as an archive's date range, whatever the server's
//     default happens to be.
func Connect(ctx context.Context, src config.PostgresSource) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(src.DSN)
	if err != nil {
		// pgx masks the password in its own parse errors, and callers redact
		// again before showing anything; both, because one is not enough.
		return nil, fmt.Errorf("parsing the PostgreSQL connection string: %s", config.RedactSecretsFromError(err))
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	params := cfg.ConnConfig.RuntimeParams
	if params["application_name"] == "" {
		params["application_name"] = orDefault(src.AppName, "feather")
	}
	params["standard_conforming_strings"] = "on"
	params["TimeZone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	return pool, nil
}

// Ping opens a throwaway connection and closes it, which is how the UI tests an
// account the operator has just saved.
func Ping(ctx context.Context, src config.PostgresSource) error {
	pool, err := Connect(ctx, src)
	if err != nil {
		return err
	}
	pool.Close()
	return nil
}

// Pool holds one connection pool per configured PostgreSQL account, opened on
// first use, so a database that is down only breaks the configs that read it.
type Pool struct {
	mu      sync.Mutex
	sources map[string]config.PostgresSource
	pools   map[string]*pgxpool.Pool
}

// NewPool builds a pool over the given accounts, keyed by alias.
func NewPool(sources map[string]config.PostgresSource) *Pool {
	return &Pool{sources: copySources(sources), pools: map[string]*pgxpool.Pool{}}
}

// Get returns the pool for an alias, connecting if this is its first use.
func (p *Pool) Get(ctx context.Context, alias string) (*pgxpool.Pool, error) {
	p.mu.Lock()
	src, known := p.sources[alias]
	existing, connected := p.pools[alias]
	p.mu.Unlock()

	if !known {
		return nil, fmt.Errorf("PostgreSQL account %q is not configured", alias)
	}
	if connected {
		return existing, nil
	}

	fresh, err := Connect(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("PostgreSQL account %q: %w", alias, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Another goroutine may have connected the same alias meanwhile; keep
	// theirs rather than leak a pool.
	if raced, ok := p.pools[alias]; ok {
		go fresh.Close()
		return raced, nil
	}
	p.pools[alias] = fresh
	return fresh, nil
}

// Replace swaps in a new set of accounts, closing any whose settings changed or
// which no longer exist, so an edited DSN takes effect on the next run.
func (p *Pool) Replace(sources map[string]config.PostgresSource) {
	p.mu.Lock()
	var stale []*pgxpool.Pool
	for alias, pool := range p.pools {
		if next, ok := sources[alias]; !ok || next != p.sources[alias] {
			stale = append(stale, pool)
			delete(p.pools, alias)
		}
	}
	p.sources = copySources(sources)
	p.mu.Unlock()

	// Close waits for borrowed connections to come back, so a run still using
	// the old pool finishes its page before the pool goes.
	for _, pool := range stale {
		go pool.Close()
	}
}

// Close closes every pool.
func (p *Pool) Close() {
	p.mu.Lock()
	pools := make([]*pgxpool.Pool, 0, len(p.pools))
	for alias, pool := range p.pools {
		pools = append(pools, pool)
		delete(p.pools, alias)
	}
	p.mu.Unlock()
	for _, pool := range pools {
		pool.Close()
	}
}

func copySources(sources map[string]config.PostgresSource) map[string]config.PostgresSource {
	out := make(map[string]config.PostgresSource, len(sources))
	for alias, src := range sources {
		out[alias] = src
	}
	return out
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// classify marks the errors no retry can fix as permanent, so a typo in a
// filter fails the run once rather than once per backoff step. Everything else
// — a dropped connection, a serialization failure, a failover — is worth
// another attempt.
func classify(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return err
	}
	switch pgErr.Code[:2] {
	case "22", // data exception: a value that cannot be cast
		"23", // integrity constraint violation
		"28", // invalid authorization
		"3D", // invalid catalog name
		"3F", // invalid schema name
		"42": // syntax error or access rule violation
		return retry.Permanent(err)
	}
	return err
}
