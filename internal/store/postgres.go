package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/unifers/feather/internal/config"
)

// --- PostgreSQL accounts ---------------------------------------------------

const postgresSourceColumns = `
	id, alias, dsn_enc, dsn_preview, app_name, enabled, COALESCE(note, ''),
	last_checked_at, last_check_ok, COALESCE(last_check_error, ''), created_at, updated_at`

// ListPostgresSources returns every stored PostgreSQL account, connection
// strings decrypted, sorted by alias.
func (s *Store) ListPostgresSources(ctx context.Context) ([]PostgresSourceRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+postgresSourceColumns+` FROM postgres_sources ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PostgresSourceRecord
	for rows.Next() {
		record, err := scanPostgresSource(rows, s)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// PostgresSourceByID fetches one account.
func (s *Store) PostgresSourceByID(ctx context.Context, id string) (PostgresSourceRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+postgresSourceColumns+` FROM postgres_sources WHERE id = ?`, id)
	record, err := scanPostgresSource(row, s)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	return record, err
}

// PostgresSourceMap is the runtime view the connection pool needs: alias to
// account, enabled ones only.
func (s *Store) PostgresSourceMap(ctx context.Context) (map[string]config.PostgresSource, error) {
	records, err := s.ListPostgresSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]config.PostgresSource, len(records))
	for _, record := range records {
		if record.Enabled {
			out[record.Alias] = record.Source()
		}
	}
	return out, nil
}

// SavePostgresSource inserts or updates an account. An empty DSN on an update
// keeps the stored one, so the UI can edit an account without ever rendering
// its password back to the browser.
func (s *Store) SavePostgresSource(ctx context.Context, record *PostgresSourceRecord) error {
	if record.Alias == "" {
		return errors.New("an alias is required")
	}
	if record.AppName == "" {
		record.AppName = "feather"
	}

	now := time.Now().UTC()
	if record.ID == "" {
		if record.DSN == "" {
			return errors.New("a connection string is required")
		}
		sealed, err := s.secrets.Seal(record.DSN)
		if err != nil {
			return err
		}
		record.ID = uuid.NewString()
		record.CreatedAt, record.UpdatedAt = now, now
		record.Preview = config.RedactPostgresDSN(record.DSN)

		_, err = s.db.ExecContext(ctx, `
			INSERT INTO postgres_sources (id, alias, dsn_enc, dsn_preview, app_name, enabled, note,
				created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			record.ID, record.Alias, sealed, record.Preview, record.AppName,
			boolToInt(record.Enabled), nullIfEmpty(record.Note), timeText(now), timeText(now))
		return aliasError("PostgreSQL account", record.Alias, err)
	}

	record.UpdatedAt = now
	if record.DSN == "" {
		_, err := s.db.ExecContext(ctx, `
			UPDATE postgres_sources SET alias = ?, app_name = ?, enabled = ?, note = ?, updated_at = ?
			WHERE id = ?`,
			record.Alias, record.AppName, boolToInt(record.Enabled), nullIfEmpty(record.Note),
			timeText(now), record.ID)
		return aliasError("PostgreSQL account", record.Alias, err)
	}

	sealed, err := s.secrets.Seal(record.DSN)
	if err != nil {
		return err
	}
	record.Preview = config.RedactPostgresDSN(record.DSN)
	// A new connection string invalidates the last test: it was a test of a
	// different string.
	_, err = s.db.ExecContext(ctx, `
		UPDATE postgres_sources SET alias = ?, dsn_enc = ?, dsn_preview = ?, app_name = ?, enabled = ?,
			note = ?, updated_at = ?, last_checked_at = NULL, last_check_ok = NULL, last_check_error = NULL
		WHERE id = ?`,
		record.Alias, sealed, record.Preview, record.AppName, boolToInt(record.Enabled),
		nullIfEmpty(record.Note), timeText(now), record.ID)
	return aliasError("PostgreSQL account", record.Alias, err)
}

// DeletePostgresSource removes an account, unless a config still reads from it.
func (s *Store) DeletePostgresSource(ctx context.Context, id string) error {
	record, err := s.PostgresSourceByID(ctx, id)
	if err != nil {
		return err
	}
	users, err := s.jobNames(ctx, `
		SELECT name FROM job_configs
		WHERE source_engine = 'postgres' AND postgres_source_alias = ? ORDER BY name`, record.Alias)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return &ErrInUse{Kind: "PostgreSQL account", Alias: record.Alias, Jobs: users}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM postgres_sources WHERE id = ?`, id)
	return err
}

// RecordPostgresCheck stores the outcome of a connectivity test, redacted
// before it is stored: drivers quote connection strings back in their errors.
func (s *Store) RecordPostgresCheck(ctx context.Context, id string, checkErr error) error {
	message := ""
	if checkErr != nil {
		message = truncate(config.RedactSecretsFromError(checkErr), 2000)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE postgres_sources SET last_checked_at = ?, last_check_ok = ?, last_check_error = NULLIF(?, '')
		WHERE id = ?`, nowText(), boolToInt(checkErr == nil), message, id)
	return err
}

func scanPostgresSource(row interface{ Scan(...any) error }, s *Store) (PostgresSourceRecord, error) {
	var (
		record               PostgresSourceRecord
		sealed               []byte
		enabled              int
		checkedAt            sql.NullString
		checkOK              sql.NullInt64
		createdAt, updatedAt string
	)
	if err := row.Scan(&record.ID, &record.Alias, &sealed, &record.Preview, &record.AppName, &enabled,
		&record.Note, &checkedAt, &checkOK, &record.Check.Error, &createdAt, &updatedAt); err != nil {
		return record, err
	}

	dsn, err := s.secrets.Open(sealed)
	if err != nil {
		return record, fmt.Errorf("PostgreSQL account %q: %w", record.Alias, err)
	}
	record.DSN = dsn
	record.Enabled = enabled != 0
	record.Check.At = scanTime(checkedAt)
	record.Check.OK = checkOK.Valid && checkOK.Int64 != 0
	record.CreatedAt, _ = parseTime(createdAt)
	record.UpdatedAt, _ = parseTime(updatedAt)
	return record, nil
}
