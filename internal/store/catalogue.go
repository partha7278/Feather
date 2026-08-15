package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/unifers/feather/internal/config"
)

// StartRun opens a run row. Everything downstream references its ID.
func (s *Store) StartRun(ctx context.Context, run *Run) error {
	if run.ID == uuid.Nil {
		run.ID = uuid.New()
	}
	run.Status = RunRunning
	run.StartedAt = time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO feather_runs (
			id, config_name, config_sha256, db_name, collection_name, filter_json,
			trigger, host, status, dry_run, started_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID.String(), run.ConfigName, run.ConfigSHA256, run.Database, run.Collection, run.FilterJSON,
		string(run.Trigger), run.Host, string(run.Status), boolToInt(run.DryRun), timeText(run.StartedAt))
	if err != nil {
		return fmt.Errorf("opening run row: %w", err)
	}
	return nil
}

// FinishRun closes a run row with its final counters and status.
func (s *Store) FinishRun(ctx context.Context, run *Run) error {
	finished := time.Now().UTC()
	run.FinishedAt = &finished

	_, err := s.db.ExecContext(ctx, `
		UPDATE feather_runs SET
			status = ?, finished_at = ?, docs_matched = ?, docs_scanned = ?, docs_archived = ?,
			docs_deleted = ?, bytes_raw = ?, bytes_compressed = ?, batch_count = ?,
			error = NULLIF(?, '')
		WHERE id = ?`,
		string(run.Status), timeText(finished), run.DocsMatched, run.DocsScanned, run.DocsArchived,
		run.DocsDeleted, run.BytesRaw, run.BytesCompressed, run.BatchCount,
		truncate(run.Error, 4000), run.ID.String())
	if err != nil {
		return fmt.Errorf("closing run row: %w", err)
	}
	return nil
}

// UpdateRunProgress flushes counters mid-run so a long job is observable while
// it is still going, and so a hard kill leaves useful numbers behind.
func (s *Store) UpdateRunProgress(ctx context.Context, run *Run) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feather_runs SET
			docs_matched = ?, docs_scanned = ?, docs_archived = ?, docs_deleted = ?,
			bytes_raw = ?, bytes_compressed = ?, batch_count = ?
		WHERE id = ?`,
		run.DocsMatched, run.DocsScanned, run.DocsArchived, run.DocsDeleted,
		run.BytesRaw, run.BytesCompressed, run.BatchCount, run.ID.String())
	return err
}

// InsertArtifact records one compressed file.
func (s *Store) InsertArtifact(ctx context.Context, a *Artifact) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	a.CreatedAt = time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO feather_artifacts (
			id, run_id, config_name, db_name, collection_name, batch_seq, file_name,
			format, codec, doc_count, bytes_raw, bytes_compressed, sha256,
			min_id, max_id, min_ts, max_ts, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID.String(), a.RunID.String(), a.ConfigName, a.Database, a.Collection, a.BatchSeq, a.FileName,
		a.Format, a.Codec, a.DocCount, a.BytesRaw, a.BytesCompressed, a.SHA256,
		nullIfEmpty(a.MinID), nullIfEmpty(a.MaxID), timeTextPtr(a.MinTS), timeTextPtr(a.MaxTS),
		timeText(a.CreatedAt))
	if err != nil {
		return fmt.Errorf("recording artifact: %w", err)
	}
	return nil
}

// MarkArtifactDeleted records that the source documents for an artifact were
// removed from Mongo, and how many actually went.
func (s *Store) MarkArtifactDeleted(ctx context.Context, artifactID uuid.UUID, deleted int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feather_artifacts
		SET deleted_from_source = 1, deleted_count = ?, deleted_at = ?
		WHERE id = ?`, deleted, nowText(), artifactID.String())
	return err
}

// InsertArtifactDestination opens a pending row for one copy of one artifact.
func (s *Store) InsertArtifactDestination(ctx context.Context, d *ArtifactDestination) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	d.LocationURL = LocationURL(d.Provider, d.Endpoint, d.Bucket, d.ObjectKey)
	d.CreatedAt = time.Now().UTC()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO feather_artifact_destinations (
			id, artifact_id, destination_alias, provider, bucket, object_key, location_url,
			region, endpoint, storage_class, size_bytes, status, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID.String(), d.ArtifactID.String(), d.Alias, d.Provider, d.Bucket, d.ObjectKey, d.LocationURL,
		nullIfEmpty(d.Region), nullIfEmpty(d.Endpoint), nullIfEmpty(d.StorageClass),
		d.SizeBytes, string(DestPending), timeText(d.CreatedAt))
	if err != nil {
		return fmt.Errorf("recording destination %s: %w", d.Alias, err)
	}
	d.Status = DestPending
	return nil
}

// LocationURL renders the address of one stored copy. AWS gets the familiar
// s3:// form; anything with a custom endpoint gets the endpoint spelled out,
// since "s3://bucket/key" would be ambiguous across two Oracle tenancies.
// Google Drive has no addressable path at all, so a copy there is named by the
// folder it went into and the path underneath it — the file's own Drive ID is
// recorded separately, in the etag column.
func LocationURL(provider, endpoint, bucket, key string) string {
	if provider == string(config.ProviderGDrive) {
		return "gdrive://" + bucket + "/" + key
	}
	if endpoint == "" {
		return "s3://" + bucket + "/" + key
	}
	return strings.TrimSuffix(endpoint, "/") + "/" + bucket + "/" + key
}

// MarkDestinationUploaded advances a destination row after a successful PUT.
func (s *Store) MarkDestinationUploaded(ctx context.Context, id uuid.UUID, etag string, size int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feather_artifact_destinations
		SET status = ?, etag = NULLIF(?, ''), size_bytes = ?, uploaded_at = ?, error = NULL
		WHERE id = ?`, string(DestUploaded), etag, size, nowText(), id.String())
	return err
}

// MarkDestinationVerified is the gate for deletion: only artifacts whose every
// destination reaches this state may have their source documents removed.
func (s *Store) MarkDestinationVerified(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feather_artifact_destinations
		SET status = ?, verified_at = ?, error = NULL
		WHERE id = ?`, string(DestVerified), nowText(), id.String())
	return err
}

func (s *Store) MarkDestinationFailed(ctx context.Context, id uuid.UUID, cause string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feather_artifact_destinations SET status = ?, error = ? WHERE id = ?`,
		string(DestFailed), truncate(cause, 4000), id.String())
	return err
}

// LastSuccessAt drives interval scheduling ("every 2d"): the next fire is
// computed from the last run that actually completed, so restarts and missed
// windows do not silently skip a cycle.
func (s *Store) LastSuccessAt(ctx context.Context, configName string) (time.Time, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(finished_at, started_at)
		FROM feather_runs
		WHERE config_name = ? AND status IN (?, ?)
		ORDER BY started_at DESC
		LIMIT 1`, configName, string(RunSuccess), string(RunPartial)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	at, err := parseTime(raw)
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
}

// FailStaleRuns closes runs this host left open. A run row stuck in "running"
// after a crash or a hard restart would otherwise look like work in progress
// forever.
//
// "Left open" has to mean abandoned, not merely "running and on this host".
// The condition used to be the latter, which meant a starting feather marked a
// genuinely live run in a sibling process as failed; that run's own FinishRun
// then overwrote the status later, so the catalogue flickered between failed
// and successful for no reason an operator could see.
//
// A live run holds a lease on its config. Call this after ClearStaleLeases, so
// the only leases left are ones belonging to processes that are still alive,
// and a surviving lease is proof the run behind it is still going.
func (s *Store) FailStaleRuns(ctx context.Context, host string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE feather_runs
		SET status = ?, finished_at = ?,
		    error = COALESCE(error, 'run did not finish — feather restarted')
		WHERE status = ? AND host = ?
		  AND config_name NOT IN (SELECT config_name FROM job_leases WHERE expires_at >= ?)`,
		string(RunFailed), nowText(), string(RunRunning), host, nowText())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

const runColumns = `
	id, config_name, status, trigger, dry_run, started_at, finished_at,
	docs_matched, docs_scanned, docs_archived, docs_deleted, bytes_compressed, batch_count, COALESCE(error, '')`

func scanRunSummary(rows interface{ Scan(...any) error }) (RunSummary, error) {
	var (
		summary             RunSummary
		id, status, trigger string
		dryRun              int
		startedAt           string
		finishedAt          sql.NullString
	)
	if err := rows.Scan(&id, &summary.ConfigName, &status, &trigger, &dryRun, &startedAt, &finishedAt,
		&summary.DocsMatched, &summary.DocsScanned, &summary.DocsArchived, &summary.DocsDeleted,
		&summary.BytesStored, &summary.Batches, &summary.Error); err != nil {
		return summary, err
	}
	summary.RunID, _ = uuid.Parse(id)
	summary.Status = RunStatus(status)
	summary.Trigger = TriggerKind(trigger)
	summary.DryRun = dryRun != 0
	started, err := parseTime(startedAt)
	if err != nil {
		return summary, err
	}
	summary.StartedAt = started
	summary.FinishedAt = scanTime(finishedAt)
	return summary, nil
}

// LatestRuns returns the most recent run per config, for `feather list` and the
// config listing in the UI.
func (s *Store) LatestRuns(ctx context.Context) (map[string]RunSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+runColumns+`
		FROM feather_runs
		WHERE id IN (
			SELECT id FROM feather_runs r2
			WHERE r2.config_name = feather_runs.config_name
			ORDER BY started_at DESC LIMIT 1
		)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]RunSummary{}
	for rows.Next() {
		summary, err := scanRunSummary(rows)
		if err != nil {
			return nil, err
		}
		out[summary.ConfigName] = summary
	}
	return out, rows.Err()
}

// RecentRuns lists runs newest first, optionally filtered to one config.
func (s *Store) RecentRuns(ctx context.Context, configName string, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT ` + runColumns + ` FROM feather_runs`
	args := []any{}
	if configName != "" {
		query += ` WHERE config_name = ?`
		args = append(args, configName)
	}
	// rowid breaks ties. Two runs starting inside the same microsecond is
	// vanishingly rare but not impossible, and without a tiebreaker SQLite is
	// free to return them in either order — so a page refresh could reorder the
	// list under the operator with nothing having changed. rowid is insertion
	// order, which for runs is exactly the order they started.
	query += ` ORDER BY started_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RunSummary
	for rows.Next() {
		summary, err := scanRunSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}

// Run fetches one run by ID.
func (s *Store) Run(ctx context.Context, id uuid.UUID) (RunSummary, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM feather_runs WHERE id = ?`, id.String())
	return scanRunSummary(row)
}

// RunFilter returns the resolved filter a run actually used, which is the
// record of what it was allowed to touch.
func (s *Store) RunFilter(ctx context.Context, id uuid.UUID) (string, error) {
	var filter string
	err := s.db.QueryRowContext(ctx, `SELECT filter_json FROM feather_runs WHERE id = ?`, id.String()).Scan(&filter)
	return filter, err
}

// ArtifactQuery narrows the backup listing.
type ArtifactQuery struct {
	ConfigName  string
	RunID       string
	Search      string // matches file name or object key
	DeletedOnly bool
	Limit       int
	Offset      int
}

const artifactColumns = `
	id, run_id, config_name, db_name, collection_name, batch_seq, file_name, format, codec,
	doc_count, bytes_raw, bytes_compressed, sha256, COALESCE(min_id, ''), COALESCE(max_id, ''),
	min_ts, max_ts, deleted_from_source, deleted_count, deleted_at, created_at`

// Artifacts lists archive files with every stored copy attached. This is the
// answer to "what was backed up, when, how big, how many documents, and where
// is it?" — the question the whole catalogue exists for.
func (s *Store) Artifacts(ctx context.Context, q ArtifactQuery) ([]ArtifactWithCopies, int64, error) {
	where := []string{"1=1"}
	args := []any{}

	if q.ConfigName != "" {
		where = append(where, "config_name = ?")
		args = append(args, q.ConfigName)
	}
	if q.RunID != "" {
		where = append(where, "run_id = ?")
		args = append(args, q.RunID)
	}
	if q.DeletedOnly {
		where = append(where, "deleted_from_source = 1")
	}
	if q.Search != "" {
		where = append(where, `(file_name LIKE ? OR id IN (
			SELECT artifact_id FROM feather_artifact_destinations WHERE object_key LIKE ?))`)
		pattern := "%" + q.Search + "%"
		args = append(args, pattern, pattern)
	}
	clause := strings.Join(where, " AND ")

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feather_artifacts WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+artifactColumns+` FROM feather_artifacts WHERE `+clause+
			` ORDER BY created_at DESC, batch_seq DESC LIMIT ? OFFSET ?`,
		append(args, limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		out []ArtifactWithCopies
		ids []string
	)
	index := map[string]int{}
	for rows.Next() {
		var (
			a                       Artifact
			id, runID               string
			minTS, maxTS, deletedAt sql.NullString
			createdAt               string
			deletedFromSource       int
		)
		if err := rows.Scan(&id, &runID, &a.ConfigName, &a.Database, &a.Collection, &a.BatchSeq,
			&a.FileName, &a.Format, &a.Codec, &a.DocCount, &a.BytesRaw, &a.BytesCompressed,
			&a.SHA256, &a.MinID, &a.MaxID, &minTS, &maxTS, &deletedFromSource, &a.DeletedCount,
			&deletedAt, &createdAt); err != nil {
			return nil, 0, err
		}
		a.ID, _ = uuid.Parse(id)
		a.RunID, _ = uuid.Parse(runID)
		a.MinTS, a.MaxTS, a.DeletedAt = scanTime(minTS), scanTime(maxTS), scanTime(deletedAt)
		a.DeletedFromSource = deletedFromSource != 0
		a.CreatedAt, _ = parseTime(createdAt)

		index[id] = len(out)
		out = append(out, ArtifactWithCopies{Artifact: a})
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(ids) == 0 {
		return out, total, nil
	}

	copies, err := s.copiesFor(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	for _, copy := range copies {
		if at, ok := index[copy.ArtifactID.String()]; ok {
			out[at].Copies = append(out[at].Copies, copy)
		}
	}
	return out, total, nil
}

// copiesFor loads every stored copy of the given artifacts in one query, so a
// page of fifty archives does not become fifty-one round trips.
func (s *Store) copiesFor(ctx context.Context, artifactIDs []string) ([]ArtifactDestination, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(artifactIDs)), ",")
	args := make([]any, len(artifactIDs))
	for i, id := range artifactIDs {
		args[i] = id
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, artifact_id, destination_alias, provider, bucket, object_key, location_url,
		       COALESCE(region, ''), COALESCE(endpoint, ''), COALESCE(storage_class, ''),
		       COALESCE(etag, ''), size_bytes, status, uploaded_at, verified_at,
		       COALESCE(error, ''), created_at
		FROM feather_artifact_destinations
		WHERE artifact_id IN (`+placeholders+`)
		ORDER BY destination_alias`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ArtifactDestination
	for rows.Next() {
		var (
			d                      ArtifactDestination
			id, artifactID, status string
			uploadedAt, verifiedAt sql.NullString
			createdAt              string
		)
		if err := rows.Scan(&id, &artifactID, &d.Alias, &d.Provider, &d.Bucket, &d.ObjectKey,
			&d.LocationURL, &d.Region, &d.Endpoint, &d.StorageClass, &d.ETag, &d.SizeBytes,
			&status, &uploadedAt, &verifiedAt, &d.Error, &createdAt); err != nil {
			return nil, err
		}
		d.ID, _ = uuid.Parse(id)
		d.ArtifactID, _ = uuid.Parse(artifactID)
		d.Status = DestinationStatus(status)
		d.UploadedAt, d.VerifiedAt = scanTime(uploadedAt), scanTime(verifiedAt)
		d.CreatedAt, _ = parseTime(createdAt)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Totals gathers the dashboard numbers in one pass.
func (s *Store) Totals(ctx context.Context) (Totals, error) {
	var t Totals

	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM job_configs),
			(SELECT COUNT(*) FROM job_configs WHERE enabled = 1),
			(SELECT COUNT(*) FROM destinations WHERE enabled = 1),
			(SELECT COUNT(*) FROM mongo_sources WHERE enabled = 1),
			(SELECT COUNT(*) FROM feather_artifacts),
			(SELECT COALESCE(SUM(doc_count), 0) FROM feather_artifacts),
			(SELECT COALESCE(SUM(deleted_count), 0) FROM feather_artifacts),
			(SELECT COALESCE(SUM(bytes_compressed), 0) FROM feather_artifacts),
			(SELECT COUNT(*) FROM feather_runs WHERE status IN ('failed','partial')),
			(SELECT COUNT(*) FROM feather_runs WHERE started_at >= ?)`,
		timeText(time.Now().Add(-24*time.Hour))).
		Scan(&t.Configs, &t.EnabledJobs, &t.Destinations, &t.MongoSources, &t.Artifacts,
			&t.DocsArchived, &t.DocsDeleted, &t.BytesStored, &t.FailedRuns, &t.RunsLast24h)
	if err != nil {
		return t, err
	}

	var last, oldest sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT MAX(started_at) FROM feather_runs),
		        (SELECT MIN(created_at) FROM feather_artifacts)`).Scan(&last, &oldest); err != nil {
		return t, err
	}
	t.LastRunAt, t.OldestArchive = scanTime(last), scanTime(oldest)
	return t, nil
}

// StorageByDestination totals what each destination is holding, which is the
// closest thing to a bill preview feather can offer.
type DestinationUsage struct {
	Alias     string
	Provider  string
	Bucket    string
	Objects   int64
	Bytes     int64
	Failed    int64
	LastWrite *time.Time
}

func (s *Store) StorageByDestination(ctx context.Context) ([]DestinationUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT destination_alias, provider, bucket,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN status IN ('uploaded','verified') THEN size_bytes ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
		       MAX(uploaded_at)
		FROM feather_artifact_destinations
		GROUP BY destination_alias, provider, bucket
		ORDER BY destination_alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DestinationUsage
	for rows.Next() {
		var (
			u    DestinationUsage
			last sql.NullString
		)
		if err := rows.Scan(&u.Alias, &u.Provider, &u.Bucket, &u.Objects, &u.Bytes, &u.Failed, &last); err != nil {
			return nil, err
		}
		u.LastWrite = scanTime(last)
		out = append(out, u)
	}
	return out, rows.Err()
}
