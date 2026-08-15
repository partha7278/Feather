// Package store is feather's single database: administrators, credentials,
// retention configs, and the catalogue of every archive ever written.
//
// It is one SQLite file. That is a deliberate fit for the target deployment —
// a VPS running one feather process — and it means a backup of feather itself
// is a file copy.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/unifers/feather/internal/secrets"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store owns the database handle and the key that unseals the credentials in
// it. Everything that touches persistent state goes through here.
type Store struct {
	db      *sql.DB
	secrets *secrets.Box
	path    string

	// log is used by the background goroutines the store owns — lease renewal
	// in particular, which has no caller to return an error to. Nil is legal
	// and means silence, which is what tests want.
	log *slog.Logger

	// SQLite serialises writers anyway; this keeps job locking honest within
	// the process without a round trip.
	localLocks sync.Map // config name -> struct{}
}

// WithLogger attaches a logger for the store's own background work. It returns
// the store so it can be chained onto Open.
func (s *Store) WithLogger(log *slog.Logger) *Store {
	s.log = log
	return s
}

// Open connects to the SQLite database at path, creating it if needed.
//
// The pragmas matter: WAL so a long read cannot block the writer, a busy
// timeout so a concurrent write waits instead of failing, and foreign keys on
// so deleting a run really does remove its artifacts.
func Open(ctx context.Context, path string, box *secrets.Box) (*Store, error) {
	if path == "" {
		return nil, errors.New("no database path configured")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating database directory %s: %w", dir, err)
		}
	}

	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection removes SQLITE_BUSY between feather's own goroutines
	// entirely. The workload is a handful of small writes per batch, so the
	// serialisation costs nothing measurable.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Close failures on these paths have nowhere to go: the caller is already
	// being handed the error that matters.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// The file holds cloud credentials, even sealed ones.
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		_ = db.Close()
		return nil, fmt.Errorf("securing %s: %w", path, err)
	}

	return &Store{db: db, secrets: box, path: path}, nil
}

// Close releases the database handle.
func (s *Store) Close() {
	if s.db != nil {
		_ = s.db.Close()
	}
}

// Path is the database file's location, for startup logs and the UI.
func (s *Store) Path() string { return s.path }

// DB exposes the handle for the rare caller that needs its own query.
func (s *Store) DB() *sql.DB { return s.db }

// appliedMigrations reads the ledger of migrations that have already run.
func (s *Store) appliedMigrations(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM feather_schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// Migrate applies every embedded migration that has not run yet, each in its
// own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS feather_schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("creating migration table: %w", err)
	}

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: filename must start with a version number", name)
		}
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("applying migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO feather_schema_migrations (version, name, applied_at) VALUES (?,?,?)`,
			version, name, nowText()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// --- meta ------------------------------------------------------------------

// Meta reads a settings value, returning fallback when it has never been set.
func (s *Store) Meta(ctx context.Context, key, fallback string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM feather_meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// MetaInt reads a settings value as an integer.
func (s *Store) MetaInt(ctx context.Context, key string, fallback int) (int, error) {
	raw, err := s.Meta(ctx, key, "")
	if err != nil || raw == "" {
		return fallback, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback, nil
	}
	return value, nil
}

// SetMeta writes a settings value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO feather_meta (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, nowText())
	return err
}

// --- time and null helpers -------------------------------------------------
//
// SQLite has no timestamp type. Times are stored as RFC3339 UTC text, which
// sorts correctly as a string and stays readable to anyone opening the file
// with the sqlite3 CLI.

// timeLayout is fixed-width on purpose.
//
// Timestamps are stored as text and compared as text — `ORDER BY started_at
// DESC`, `WHERE expires_at < ?`, `MAX(finished_at)`. That only agrees with
// chronological order if every value is the same shape, and the obvious layout
// (".999") is not: it trims trailing zeros, so a time landing exactly on a
// second is written "12:00:00Z" while a time 5ms later is "12:00:00.005Z".
// Lexically '.' (0x2E) sorts before 'Z' (0x5A), so the later instant compares
// as earlier. The result was a run list that occasionally showed the wrong
// "last run", and an interval schedule anchored on the wrong time.
//
// ".000000" always writes six digits, which makes text order and time order the
// same thing. Microseconds rather than milliseconds because ties matter too: a
// run's rows are written in well under a millisecond, so at that precision two
// runs regularly shared a timestamp and came back in whatever order SQLite
// happened to scan them. Migration 003 rewrites values stored under the old
// layout.
const timeLayout = "2006-01-02T15:04:05.000000Z07:00"

func nowText() string { return time.Now().UTC().Format(timeLayout) }

func timeText(t time.Time) string { return t.UTC().Format(timeLayout) }

func timeTextPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timeText(*t)
}

func parseTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, raw)
}

// scanTime reads a nullable timestamp column.
func scanTime(raw sql.NullString) *time.Time {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	t, err := parseTime(raw.String)
	if err != nil {
		return nil
	}
	return &t
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
