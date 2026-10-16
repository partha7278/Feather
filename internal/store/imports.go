package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// StartImport opens an import row. Like StartRun it stamps the ID, status and
// start time itself, so every row starts life the same way.
func (s *Store) StartImport(ctx context.Context, imp *Import) error {
	if imp.ID == uuid.Nil {
		imp.ID = uuid.New()
	}
	imp.Status = ImportRunning
	imp.StartedAt = time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO feather_imports (
			id, actor, actor_ip, source_kind, source_ref, source_label, prefer_alias,
			target_alias, target_db, target_collection, mode, dry_run, status, started_at,
			files_total, docs_expected, bytes_expected
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		imp.ID.String(), imp.Actor, imp.ActorIP, imp.SourceKind, imp.SourceRef, imp.SourceLabel, imp.PreferAlias,
		imp.TargetAlias, imp.TargetDatabase, imp.TargetCollection, imp.Mode, boolToInt(imp.DryRun),
		string(imp.Status), timeText(imp.StartedAt),
		imp.FilesTotal, imp.DocsExpected, imp.BytesExpected)
	if err != nil {
		return fmt.Errorf("opening import row: %w", err)
	}
	return nil
}

// UpdateImportProgress flushes counters mid-import, so the detail page has
// something to show and a hard kill leaves the numbers it reached.
func (s *Store) UpdateImportProgress(ctx context.Context, imp *Import) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feather_imports SET
			files_done = ?, docs_read = ?, docs_inserted = ?, docs_present = ?, docs_replaced = ?,
			bytes_read = ?
		WHERE id = ?`,
		imp.FilesDone, imp.DocsRead, imp.DocsInserted, imp.DocsPresent, imp.DocsReplaced,
		imp.BytesRead, imp.ID.String())
	return err
}

// FinishImport closes an import row with its final counters and status.
func (s *Store) FinishImport(ctx context.Context, imp *Import) error {
	finished := time.Now().UTC()
	imp.FinishedAt = &finished

	_, err := s.db.ExecContext(ctx, `
		UPDATE feather_imports SET
			status = ?, finished_at = ?, files_done = ?, docs_read = ?, docs_inserted = ?,
			docs_present = ?, docs_replaced = ?, bytes_read = ?, sha256 = ?, error = ?
		WHERE id = ?`,
		string(imp.Status), timeText(finished), imp.FilesDone, imp.DocsRead, imp.DocsInserted,
		imp.DocsPresent, imp.DocsReplaced, imp.BytesRead, imp.SHA256, truncate(imp.Error, 4000),
		imp.ID.String())
	if err != nil {
		return fmt.Errorf("closing import row: %w", err)
	}
	return nil
}

// FailStaleImports closes imports a previous process left open.
//
// Unlike FailStaleRuns there is no lease to consult: imports only start from
// the panel, one process at a time, and nothing outside the process that
// started one can be running it. So at startup, every row still marked
// running was abandoned by a restart. Call it only then.
func (s *Store) FailStaleImports(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE feather_imports
		SET status = ?, finished_at = ?,
		    error = CASE WHEN error = '' THEN 'interrupted by a restart' ELSE error END
		WHERE status = ?`,
		string(ImportFailed), nowText(), string(ImportRunning))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

const importColumns = `
	id, actor, actor_ip, source_kind, source_ref, source_label, prefer_alias,
	target_alias, target_db, target_collection, mode, dry_run, status, started_at, finished_at,
	files_total, files_done, docs_expected, bytes_expected, docs_read, docs_inserted,
	docs_present, docs_replaced, bytes_read, sha256, error`

func scanImport(row interface{ Scan(...any) error }) (Import, error) {
	var (
		imp        Import
		id, status string
		dryRun     int
		startedAt  string
		finishedAt sql.NullString
	)
	if err := row.Scan(&id, &imp.Actor, &imp.ActorIP, &imp.SourceKind, &imp.SourceRef, &imp.SourceLabel,
		&imp.PreferAlias, &imp.TargetAlias, &imp.TargetDatabase, &imp.TargetCollection, &imp.Mode,
		&dryRun, &status, &startedAt, &finishedAt,
		&imp.FilesTotal, &imp.FilesDone, &imp.DocsExpected, &imp.BytesExpected, &imp.DocsRead,
		&imp.DocsInserted, &imp.DocsPresent, &imp.DocsReplaced, &imp.BytesRead, &imp.SHA256,
		&imp.Error); err != nil {
		return imp, err
	}
	imp.ID, _ = uuid.Parse(id)
	imp.Status = ImportStatus(status)
	imp.DryRun = dryRun != 0
	started, err := parseTime(startedAt)
	if err != nil {
		return imp, err
	}
	imp.StartedAt = started
	imp.FinishedAt = scanTime(finishedAt)
	return imp, nil
}

// Imports lists imports newest first.
func (s *Store) Imports(ctx context.Context, limit int) ([]Import, error) {
	if limit <= 0 {
		limit = 100
	}
	// rowid breaks ties, as it does for runs: two imports started in the same
	// microsecond must not swap places between page loads.
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+importColumns+` FROM feather_imports ORDER BY started_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Import
	for rows.Next() {
		imp, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, imp)
	}
	return out, rows.Err()
}

// ImportByID fetches one import.
func (s *Store) ImportByID(ctx context.Context, id uuid.UUID) (Import, error) {
	imp, err := scanImport(s.db.QueryRowContext(ctx,
		`SELECT `+importColumns+` FROM feather_imports WHERE id = ?`, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return Import{}, ErrNotFound
	}
	return imp, err
}
