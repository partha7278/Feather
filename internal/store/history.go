package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// RunHistory is one finished run with every file it wrote and every copy of
// those files: the execution log an export carries. The archives themselves
// stay in their destinations; this is the record of where they are.
type RunHistory struct {
	Run       Run
	Artifacts []ArtifactWithCopies
}

// FinishedRunHistory reads every finished run, oldest first, with its files.
// A run still going is left out: imported elsewhere it would sit as running
// forever, since no process there will ever finish it.
func (s *Store) FinishedRunHistory(ctx context.Context) ([]RunHistory, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, config_name, config_sha256, db_name, collection_name, filter_json,
		       trigger, host, status, dry_run, started_at, finished_at,
		       docs_matched, docs_scanned, docs_archived, docs_deleted,
		       bytes_raw, bytes_compressed, batch_count, COALESCE(error, '')
		FROM feather_runs
		WHERE finished_at IS NOT NULL
		ORDER BY started_at, rowid`)
	if err != nil {
		return nil, err
	}

	var out []RunHistory
	for rows.Next() {
		var (
			run                 Run
			id, trigger, status string
			dryRun              int
			startedAt           string
			finishedAt          sql.NullString
		)
		if err := rows.Scan(&id, &run.ConfigName, &run.ConfigSHA256, &run.Database, &run.Collection,
			&run.FilterJSON, &trigger, &run.Host, &status, &dryRun, &startedAt, &finishedAt,
			&run.DocsMatched, &run.DocsScanned, &run.DocsArchived, &run.DocsDeleted,
			&run.BytesRaw, &run.BytesCompressed, &run.BatchCount, &run.Error); err != nil {
			rows.Close()
			return nil, err
		}
		run.ID, _ = uuid.Parse(id)
		run.Trigger = TriggerKind(trigger)
		run.Status = RunStatus(status)
		run.DryRun = dryRun != 0
		if run.StartedAt, err = parseTime(startedAt); err != nil {
			rows.Close()
			return nil, err
		}
		run.FinishedAt = scanTime(finishedAt)
		out = append(out, RunHistory{Run: run})
	}
	// Closed before the per-run queries below: SQLite would otherwise hold the
	// read open across them.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		artifacts, err := s.ArtifactsForRun(ctx, out[i].Run.ID)
		if err != nil {
			return nil, fmt.Errorf("reading the files of run %s: %w", out[i].Run.ID, err)
		}
		out[i].Artifacts = artifacts
	}
	return out, nil
}

// ImportRunHistory writes one run from another installation, keeping its IDs
// and timestamps so it reads exactly as it did there. A run that is already
// here is left alone and reported as not imported, which makes repeating an
// import harmless.
func (s *Store) ImportRunHistory(ctx context.Context, h RunHistory) (bool, error) {
	run := h.Run
	if run.ID == uuid.Nil {
		return false, fmt.Errorf("a run without an ID")
	}
	if run.FinishedAt == nil {
		return false, fmt.Errorf("run %s never finished", run.ID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM feather_runs WHERE id = ?`, run.ID.String()).Scan(&exists)
	switch {
	case err == nil:
		return false, nil
	case err != sql.ErrNoRows:
		return false, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO feather_runs (
			id, config_name, config_sha256, db_name, collection_name, filter_json,
			trigger, host, status, dry_run, started_at, finished_at,
			docs_matched, docs_scanned, docs_archived, docs_deleted,
			bytes_raw, bytes_compressed, batch_count, error
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULLIF(?, ''))`,
		run.ID.String(), run.ConfigName, run.ConfigSHA256, run.Database, run.Collection, run.FilterJSON,
		string(run.Trigger), run.Host, string(run.Status), boolToInt(run.DryRun),
		timeText(run.StartedAt), timeTextPtr(run.FinishedAt),
		run.DocsMatched, run.DocsScanned, run.DocsArchived, run.DocsDeleted,
		run.BytesRaw, run.BytesCompressed, run.BatchCount, truncate(run.Error, 4000)); err != nil {
		return false, fmt.Errorf("recording run %s: %w", run.ID, err)
	}

	for _, a := range h.Artifacts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO feather_artifacts (
				id, run_id, config_name, db_name, collection_name, batch_seq, file_name,
				format, codec, doc_count, bytes_raw, bytes_compressed, sha256,
				min_id, max_id, min_ts, max_ts,
				deleted_from_source, deleted_count, deleted_at, created_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			a.ID.String(), run.ID.String(), a.ConfigName, a.Database, a.Collection, a.BatchSeq, a.FileName,
			a.Format, a.Codec, a.DocCount, a.BytesRaw, a.BytesCompressed, a.SHA256,
			nullIfEmpty(a.MinID), nullIfEmpty(a.MaxID), timeTextPtr(a.MinTS), timeTextPtr(a.MaxTS),
			boolToInt(a.DeletedFromSource), a.DeletedCount, timeTextPtr(a.DeletedAt),
			timeText(a.CreatedAt)); err != nil {
			return false, fmt.Errorf("recording file %s of run %s: %w", a.FileName, run.ID, err)
		}

		for _, d := range a.Copies {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO feather_artifact_destinations (
					id, artifact_id, destination_alias, provider, bucket, object_key, location_url,
					region, endpoint, storage_class, etag, size_bytes, status,
					uploaded_at, verified_at, error, created_at
				) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				d.ID.String(), a.ID.String(), d.Alias, d.Provider, d.Bucket, d.ObjectKey, d.LocationURL,
				nullIfEmpty(d.Region), nullIfEmpty(d.Endpoint), nullIfEmpty(d.StorageClass),
				nullIfEmpty(d.ETag), d.SizeBytes, string(d.Status),
				timeTextPtr(d.UploadedAt), timeTextPtr(d.VerifiedAt), nullIfEmpty(d.Error),
				timeText(d.CreatedAt)); err != nil {
				return false, fmt.Errorf("recording the %s copy of %s: %w", d.Alias, a.FileName, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
