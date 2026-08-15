package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/unifers/feather/internal/config"
)

// ErrNotFound is returned when a lookup by id or alias finds nothing.
var ErrNotFound = errors.New("not found")

// ErrInUse blocks deleting an account a retention config still points at.
// Removing it silently would leave a job that fails at its next scheduled run,
// at 2am, with nobody watching.
type ErrInUse struct {
	Kind  string
	Alias string
	Jobs  []string
}

func (e *ErrInUse) Error() string {
	return fmt.Sprintf("%s %q is still used by %d retention config(s): %s",
		e.Kind, e.Alias, len(e.Jobs), strings.Join(e.Jobs, ", "))
}

// --- MongoDB accounts ------------------------------------------------------

// ListMongoSources returns every stored MongoDB account, connection strings
// decrypted, sorted by alias.
func (s *Store) ListMongoSources(ctx context.Context) ([]MongoSourceRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, alias, uri_enc, uri_preview, auth_source, app_name, enabled, COALESCE(note, ''),
		       last_checked_at, last_check_ok, COALESCE(last_check_error, ''), created_at, updated_at
		FROM mongo_sources ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MongoSourceRecord
	for rows.Next() {
		record, err := scanMongoSource(rows, s)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// MongoSourceByID fetches one account.
func (s *Store) MongoSourceByID(ctx context.Context, id string) (MongoSourceRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, alias, uri_enc, uri_preview, auth_source, app_name, enabled, COALESCE(note, ''),
		       last_checked_at, last_check_ok, COALESCE(last_check_error, ''), created_at, updated_at
		FROM mongo_sources WHERE id = ?`, id)

	record, err := scanMongoSource(row, s)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	return record, err
}

// MongoSourceMap is the runtime view the connection pool needs: alias to
// account, enabled ones only.
func (s *Store) MongoSourceMap(ctx context.Context) (map[string]config.MongoSource, error) {
	records, err := s.ListMongoSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]config.MongoSource, len(records))
	for _, record := range records {
		if record.Enabled {
			out[record.Alias] = record.Source()
		}
	}
	return out, nil
}

// SaveMongoSource inserts or updates an account. An empty URI on an update
// keeps the stored one, which is what lets the UI edit an account without ever
// rendering its password back to the browser.
func (s *Store) SaveMongoSource(ctx context.Context, record *MongoSourceRecord) error {
	if record.Alias == "" {
		return errors.New("an alias is required")
	}
	if record.AuthSource == "" {
		record.AuthSource = "admin"
	}
	if record.AppName == "" {
		record.AppName = "feather"
	}

	now := time.Now().UTC()
	if record.ID == "" {
		if record.URI == "" {
			return errors.New("a connection string is required")
		}
		sealed, err := s.secrets.Seal(record.URI)
		if err != nil {
			return err
		}
		record.ID = uuid.NewString()
		record.CreatedAt, record.UpdatedAt = now, now
		record.Preview = config.RedactMongoURI(record.URI)

		_, err = s.db.ExecContext(ctx, `
			INSERT INTO mongo_sources (id, alias, uri_enc, uri_preview, auth_source, app_name,
				enabled, note, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			record.ID, record.Alias, sealed, record.Preview, record.AuthSource, record.AppName,
			boolToInt(record.Enabled), nullIfEmpty(record.Note), timeText(now), timeText(now))
		return aliasError("MongoDB account", record.Alias, err)
	}

	record.UpdatedAt = now
	if record.URI == "" {
		_, err := s.db.ExecContext(ctx, `
			UPDATE mongo_sources SET alias = ?, auth_source = ?, app_name = ?, enabled = ?,
				note = ?, updated_at = ? WHERE id = ?`,
			record.Alias, record.AuthSource, record.AppName, boolToInt(record.Enabled),
			nullIfEmpty(record.Note), timeText(now), record.ID)
		return aliasError("MongoDB account", record.Alias, err)
	}

	sealed, err := s.secrets.Seal(record.URI)
	if err != nil {
		return err
	}
	record.Preview = config.RedactMongoURI(record.URI)
	_, err = s.db.ExecContext(ctx, `
		UPDATE mongo_sources SET alias = ?, uri_enc = ?, uri_preview = ?, auth_source = ?,
			app_name = ?, enabled = ?, note = ?, updated_at = ?,
			last_checked_at = NULL, last_check_ok = NULL, last_check_error = NULL
		WHERE id = ?`,
		record.Alias, sealed, record.Preview, record.AuthSource, record.AppName,
		boolToInt(record.Enabled), nullIfEmpty(record.Note), timeText(now), record.ID)
	return aliasError("MongoDB account", record.Alias, err)
}

// DeleteMongoSource removes an account, unless a config still reads from it.
func (s *Store) DeleteMongoSource(ctx context.Context, id string) error {
	record, err := s.MongoSourceByID(ctx, id)
	if err != nil {
		return err
	}
	users, err := s.jobNames(ctx, `SELECT name FROM job_configs WHERE mongo_source_alias = ? ORDER BY name`, record.Alias)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return &ErrInUse{Kind: "MongoDB account", Alias: record.Alias, Jobs: users}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM mongo_sources WHERE id = ?`, id)
	return err
}

// RecordMongoCheck stores the outcome of a connectivity test.
func (s *Store) RecordMongoCheck(ctx context.Context, id string, checkErr error) error {
	message := ""
	if checkErr != nil {
		message = truncate(checkErr.Error(), 2000)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE mongo_sources SET last_checked_at = ?, last_check_ok = ?, last_check_error = NULLIF(?, '')
		WHERE id = ?`, nowText(), boolToInt(checkErr == nil), message, id)
	return err
}

func scanMongoSource(row interface{ Scan(...any) error }, s *Store) (MongoSourceRecord, error) {
	var (
		record               MongoSourceRecord
		sealed               []byte
		enabled              int
		checkedAt            sql.NullString
		checkOK              sql.NullInt64
		createdAt, updatedAt string
	)
	if err := row.Scan(&record.ID, &record.Alias, &sealed, &record.Preview, &record.AuthSource,
		&record.AppName, &enabled, &record.Note, &checkedAt, &checkOK, &record.Check.Error,
		&createdAt, &updatedAt); err != nil {
		return record, err
	}

	uri, err := s.secrets.Open(sealed)
	if err != nil {
		return record, fmt.Errorf("MongoDB account %q: %w", record.Alias, err)
	}
	record.URI = uri
	record.Enabled = enabled != 0
	record.Check.At = scanTime(checkedAt)
	record.Check.OK = checkOK.Valid && checkOK.Int64 != 0
	record.CreatedAt, _ = parseTime(createdAt)
	record.UpdatedAt, _ = parseTime(updatedAt)
	return record, nil
}

// --- Backup destinations ---------------------------------------------------

const destinationColumns = `
	id, alias, provider, region, bucket, COALESCE(endpoint, ''), COALESCE(access_key_id, ''),
	secret_access_key_enc, session_token_enc, COALESCE(storage_class, ''), COALESCE(key_prefix, ''),
	force_path_style, COALESCE(drive_folder_id, ''), COALESCE(drive_id, ''), COALESCE(drive_subject, ''),
	COALESCE(oauth_client_id, ''), service_account_json_enc, oauth_client_secret_enc,
	oauth_refresh_token_enc, enabled, COALESCE(note, ''), last_checked_at, last_check_ok,
	COALESCE(last_check_error, ''), created_at, updated_at`

// ListDestinations returns every stored S3 or Oracle account, secrets
// decrypted, sorted by alias.
func (s *Store) ListDestinations(ctx context.Context) ([]DestinationRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+destinationColumns+` FROM destinations ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DestinationRecord
	for rows.Next() {
		record, err := scanDestination(rows, s)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// DestinationByID fetches one destination.
func (s *Store) DestinationByID(ctx context.Context, id string) (DestinationRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+destinationColumns+` FROM destinations WHERE id = ?`, id)
	record, err := scanDestination(row, s)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	return record, err
}

// DestinationMap is the runtime view the upload registry needs. A destination
// that is disabled, or that is waiting on an authorisation it has not been
// given yet, is left out: it cannot be written to, so a config must not be able
// to name it either.
func (s *Store) DestinationMap(ctx context.Context) (map[string]config.Destination, error) {
	records, err := s.ListDestinations(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]config.Destination, len(records))
	for _, record := range records {
		if record.Enabled && record.Ready() {
			out[record.Alias] = record.Destination
		}
	}
	return out, nil
}

// SaveDestination inserts or updates a backup account. As with Mongo, an empty
// secret on an update means "keep the one already stored".
func (s *Store) SaveDestination(ctx context.Context, record *DestinationRecord) error {
	record.Destination.Alias = strings.TrimSpace(record.Destination.Alias)
	switch record.Provider {
	case config.ProviderOCI:
		// Oracle's S3 compatibility layer only speaks path-style addressing.
		record.ForcePathStyle = true
	case config.ProviderGDrive:
		// Drive has none of the S3 vocabulary; keeping stale values out of the
		// row means the UI and the catalogue never show a region for a folder.
		record.Region, record.Bucket, record.Endpoint = "", "", ""
		record.AccessKeyID, record.SecretAccessKey, record.SessionToken = "", "", ""
		record.StorageClass, record.ForcePathStyle = "", false
	}

	// Validation needs the whole account, and on an edit the secrets live in
	// the database rather than in the form that was just posted.
	complete := record.Destination
	if record.ID != "" {
		stored, err := s.DestinationByID(ctx, record.ID)
		if err != nil {
			return err
		}
		if complete.SecretAccessKey == "" {
			complete.SecretAccessKey = stored.SecretAccessKey
		}
		if complete.ServiceAccountJSON == "" {
			complete.ServiceAccountJSON = stored.ServiceAccountJSON
		}
		if complete.OAuthClientSecret == "" {
			complete.OAuthClientSecret = stored.OAuthClientSecret
		}
		if complete.OAuthRefreshToken == "" && complete.OAuthClientID == stored.OAuthClientID {
			// A refresh token belongs to the client that obtained it; changing
			// the client ID invalidates it, and the account must be reconnected.
			complete.OAuthRefreshToken = stored.OAuthRefreshToken
		}
	}
	if err := config.ValidateDestination(complete); err != nil {
		return err
	}

	now := time.Now().UTC()
	if record.ID == "" {
		sealed, err := s.sealDestinationSecrets(record)
		if err != nil {
			return err
		}
		record.ID = uuid.NewString()
		record.CreatedAt, record.UpdatedAt = now, now

		_, err = s.db.ExecContext(ctx, `
			INSERT INTO destinations (id, alias, provider, region, bucket, endpoint, access_key_id,
				secret_access_key_enc, session_token_enc, storage_class, key_prefix, force_path_style,
				drive_folder_id, drive_id, drive_subject, oauth_client_id,
				service_account_json_enc, oauth_client_secret_enc, oauth_refresh_token_enc,
				enabled, note, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			record.ID, record.Alias, string(record.Provider), record.Region, record.Bucket,
			nullIfEmpty(record.Endpoint), nullIfEmpty(record.AccessKeyID), sealed.secret, sealed.token,
			nullIfEmpty(record.StorageClass), nullIfEmpty(record.Prefix), boolToInt(record.ForcePathStyle),
			nullIfEmpty(record.DriveFolderID), nullIfEmpty(record.DriveID), nullIfEmpty(record.DriveSubject),
			nullIfEmpty(record.OAuthClientID), sealed.serviceAccount, sealed.clientSecret, sealed.refreshToken,
			boolToInt(record.Enabled), nullIfEmpty(record.Note), timeText(now), timeText(now))
		return aliasError("destination", record.Alias, err)
	}

	record.UpdatedAt = now
	sets := []string{
		"alias = ?", "provider = ?", "region = ?", "bucket = ?", "endpoint = ?", "access_key_id = ?",
		"storage_class = ?", "key_prefix = ?", "force_path_style = ?", "drive_folder_id = ?",
		"drive_id = ?", "drive_subject = ?", "oauth_client_id = ?", "enabled = ?", "note = ?",
		"updated_at = ?",
	}
	args := []any{
		record.Alias, string(record.Provider), record.Region, record.Bucket,
		nullIfEmpty(record.Endpoint), nullIfEmpty(record.AccessKeyID),
		nullIfEmpty(record.StorageClass), nullIfEmpty(record.Prefix),
		boolToInt(record.ForcePathStyle), nullIfEmpty(record.DriveFolderID),
		nullIfEmpty(record.DriveID), nullIfEmpty(record.DriveSubject),
		nullIfEmpty(record.OAuthClientID), boolToInt(record.Enabled), nullIfEmpty(record.Note),
		timeText(now),
	}

	// Each credential is written only when the form supplied one, so a blank
	// field keeps the stored value instead of erasing it.
	rekeyed := false
	for _, credential := range []struct {
		column string
		value  string
	}{
		{"secret_access_key_enc", record.SecretAccessKey},
		{"session_token_enc", record.SessionToken},
		{"service_account_json_enc", record.ServiceAccountJSON},
		{"oauth_client_secret_enc", record.OAuthClientSecret},
		{"oauth_refresh_token_enc", record.OAuthRefreshToken},
	} {
		if credential.value == "" {
			continue
		}
		sealed, err := s.secrets.Seal(credential.value)
		if err != nil {
			return err
		}
		sets = append(sets, credential.column+" = ?")
		args = append(args, sealed)
		rekeyed = true
	}
	// New credentials invalidate whatever the last connectivity test said.
	if rekeyed {
		sets = append(sets, "last_checked_at = NULL", "last_check_ok = NULL", "last_check_error = NULL")
	}
	args = append(args, record.ID)

	_, err := s.db.ExecContext(ctx,
		`UPDATE destinations SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return aliasError("destination", record.Alias, err)
}

// DeleteDestination removes a backup account, unless a config still writes to
// it. Catalogue rows keep the alias regardless: history stays readable even
// after an account is retired.
func (s *Store) DeleteDestination(ctx context.Context, id string) error {
	record, err := s.DestinationByID(ctx, id)
	if err != nil {
		return err
	}
	users, err := s.jobNames(ctx, `
		SELECT j.name FROM job_configs j
		JOIN job_destinations d ON d.job_id = j.id
		WHERE d.destination_alias = ? ORDER BY j.name`, record.Alias)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return &ErrInUse{Kind: "destination", Alias: record.Alias, Jobs: users}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM destinations WHERE id = ?`, id)
	return err
}

// ClearDriveAuthorisation forgets a Google Drive refresh token. The account
// keeps its OAuth client and its folder, so reconnecting is one click; until
// then it is not ready, and no config can write to it.
func (s *Store) ClearDriveAuthorisation(ctx context.Context, id string) error {
	record, err := s.DestinationByID(ctx, id)
	if err != nil {
		return err
	}
	if record.Provider != config.ProviderGDrive {
		return fmt.Errorf("destination %q is not a Google Drive account", record.Alias)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE destinations SET oauth_refresh_token_enc = NULL, updated_at = ?,
			last_checked_at = NULL, last_check_ok = NULL, last_check_error = NULL
		WHERE id = ?`, nowText(), id)
	return err
}

// RecordDestinationCheck stores the outcome of a reachability test.
func (s *Store) RecordDestinationCheck(ctx context.Context, id string, checkErr error) error {
	message := ""
	if checkErr != nil {
		message = truncate(checkErr.Error(), 2000)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE destinations SET last_checked_at = ?, last_check_ok = ?, last_check_error = NULLIF(?, '')
		WHERE id = ?`, nowText(), boolToInt(checkErr == nil), message, id)
	return err
}

// sealedDestination is one account's credentials, encrypted and ready to write.
type sealedDestination struct {
	secret, token                              []byte
	serviceAccount, clientSecret, refreshToken []byte
}

func (s *Store) sealDestinationSecrets(record *DestinationRecord) (sealedDestination, error) {
	var (
		out sealedDestination
		err error
	)
	for _, credential := range []struct {
		into  *[]byte
		value string
	}{
		{&out.secret, record.SecretAccessKey},
		{&out.token, record.SessionToken},
		{&out.serviceAccount, record.ServiceAccountJSON},
		{&out.clientSecret, record.OAuthClientSecret},
		{&out.refreshToken, record.OAuthRefreshToken},
	} {
		if *credential.into, err = s.secrets.Seal(credential.value); err != nil {
			return out, err
		}
	}
	return out, nil
}

func scanDestination(row interface{ Scan(...any) error }, s *Store) (DestinationRecord, error) {
	var (
		record                                     DestinationRecord
		provider                                   string
		secret, token                              []byte
		serviceAccount, clientSecret, refreshToken []byte
		pathStyle, enabled                         int
		checkedAt                                  sql.NullString
		checkOK                                    sql.NullInt64
		createdAt, updatedAt                       string
	)
	if err := row.Scan(&record.ID, &record.Destination.Alias, &provider, &record.Region, &record.Bucket,
		&record.Endpoint, &record.AccessKeyID, &secret, &token, &record.StorageClass, &record.Prefix,
		&pathStyle, &record.DriveFolderID, &record.DriveID, &record.DriveSubject, &record.OAuthClientID,
		&serviceAccount, &clientSecret, &refreshToken,
		&enabled, &record.Note, &checkedAt, &checkOK, &record.Check.Error,
		&createdAt, &updatedAt); err != nil {
		return record, err
	}

	for _, credential := range []struct {
		into   *string
		sealed []byte
	}{
		{&record.SecretAccessKey, secret},
		{&record.SessionToken, token},
		{&record.ServiceAccountJSON, serviceAccount},
		{&record.OAuthClientSecret, clientSecret},
		{&record.OAuthRefreshToken, refreshToken},
	} {
		plain, err := s.secrets.Open(credential.sealed)
		if err != nil {
			return record, fmt.Errorf("destination %q: %w", record.Destination.Alias, err)
		}
		*credential.into = plain
	}

	record.Provider = config.Provider(provider)
	record.ForcePathStyle = pathStyle != 0
	record.Enabled = enabled != 0
	record.Check.At = scanTime(checkedAt)
	record.Check.OK = checkOK.Valid && checkOK.Int64 != 0
	record.CreatedAt, _ = parseTime(createdAt)
	record.UpdatedAt, _ = parseTime(updatedAt)
	return record, nil
}

// --- Retention configs -----------------------------------------------------

const jobColumns = `
	id, name, enabled, mongo_source_alias, db_name, collection_name, filter_json,
	COALESCE(projection_json, ''), read_preference, COALESCE(timestamp_field, ''),
	batch_max_docs, batch_max_bytes, batch_fetch_size, codec, codec_level, output_format,
	key_template, delete_after_archive, verify_before_delete, delete_batch_size,
	COALESCE(schedule_cron, ''), COALESCE(schedule_every, ''), COALESCE(schedule_at, ''),
	schedule_timezone, max_docs_per_run, max_runtime, dry_run, sha256, created_at, updated_at`

// ListJobs returns every retention config, each prepared, and each marked with
// the reason it cannot run if there is one.
//
// A config that no longer prepares used to fail the whole call, and app.Open
// calls it — so one bad row meant `feather serve` would not start, and nobody
// could open the panel to fix the row that was blocking the boot. Worse, the
// rows that can break this way are ones feather itself writes: a one-click
// deletion toggle, a timezone the host has no tzdata for, a rule tightened in
// an upgrade.
//
// So a broken config is now parked rather than fatal. It is returned with
// Problem set, it appears in the UI with its error, and Jobs — what the
// scheduler and the runner see — leaves it out entirely. The other configs keep
// archiving, which is what an operator with ten configs and one typo wants.
func (s *Store) ListJobs(ctx context.Context) ([]JobRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM job_configs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var (
		out  []JobRecord
		byID = map[string]*config.JobConfig{}
	)
	for rows.Next() {
		record, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		byID[record.ID] = record.Config
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	if err := s.attachJobDestinations(ctx, byID); err != nil {
		return nil, err
	}
	for i := range out {
		record := &out[i]
		if err := record.Config.Prepare(); err != nil {
			record.Problem = err.Error()
			continue
		}
		// Prepare says feather can make sense of it; CheckSafeToRun says
		// running it would not destroy anything. Both have to hold before a
		// config is allowed near a collection.
		if err := record.Config.CheckSafeToRun(); err != nil {
			record.Problem = err.Error()
		}
	}
	return out, nil
}

// Jobs is the plain list the scheduler and the runner want: every config that
// is fit to run. Broken ones are left out here and reported by ListJobs.
func (s *Store) Jobs(ctx context.Context) ([]*config.JobConfig, error) {
	records, err := s.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*config.JobConfig, 0, len(records))
	for _, record := range records {
		if record.Broken() {
			continue
		}
		out = append(out, record.Config)
	}
	return out, nil
}

// BrokenJobs lists the configs that are parked, and why. Startup logs it and
// `feather validate` reports it as a failure.
func (s *Store) BrokenJobs(ctx context.Context) (map[string]string, error) {
	records, err := s.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	broken := map[string]string{}
	for _, record := range records {
		if record.Broken() {
			broken[record.Config.Name] = record.Problem
		}
	}
	return broken, nil
}

// JobByID fetches one config.
func (s *Store) JobByID(ctx context.Context, id string) (JobRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM job_configs WHERE id = ?`, id)
	record, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if err := s.attachJobDestinations(ctx, map[string]*config.JobConfig{record.ID: record.Config}); err != nil {
		return record, err
	}
	if err := record.Config.Prepare(); err != nil {
		return record, err
	}
	return record, nil
}

// JobByName fetches one config by the name operators use.
func (s *Store) JobByName(ctx context.Context, name string) (JobRecord, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM job_configs WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return JobRecord{}, fmt.Errorf("no retention config named %q", name)
	}
	if err != nil {
		return JobRecord{}, err
	}
	return s.JobByID(ctx, id)
}

// SaveJob validates a config against the accounts that exist and stores it.
func (s *Store) SaveJob(ctx context.Context, record *JobRecord) error {
	sources, err := s.MongoSourceMap(ctx)
	if err != nil {
		return err
	}
	destinations, err := s.DestinationMap(ctx)
	if err != nil {
		return err
	}
	job := record.Config
	if err := job.Validate(config.Catalog{Destinations: destinations, Sources: sources}); err != nil {
		return err
	}
	job.SHA256 = job.ComputeSHA()

	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	args := []any{
		job.Name, boolToInt(job.IsEnabled()), job.Source.MongoSource, job.Source.Database,
		job.Source.Collection, string(job.Source.Filter), nullIfEmpty(string(job.Source.Projection)),
		job.Source.ReadPreference, nullIfEmpty(job.Source.TimestampField),
		job.Batch.MaxDocs, int64(job.Batch.MaxBytes), job.Batch.FetchSize,
		job.Compression.Codec, job.Compression.Level, job.Output.Format, job.Output.KeyTemplate,
		boolToInt(job.Retention.DeleteAfterArchive), boolToInt(job.ShouldVerify()), job.Retention.DeleteBatchSize,
		nullIfEmpty(job.Schedule.Cron), nullIfEmpty(job.Schedule.Every), nullIfEmpty(job.Schedule.At),
		job.Schedule.Timezone, job.Limits.MaxDocsPerRun, job.Limits.MaxRuntime.Std().String(),
		boolToInt(job.DryRun), job.SHA256, timeText(now),
	}

	if record.ID == "" {
		record.ID = uuid.NewString()
		record.CreatedAt, record.UpdatedAt = now, now
		_, err = tx.ExecContext(ctx, `
			INSERT INTO job_configs (name, enabled, mongo_source_alias, db_name, collection_name,
				filter_json, projection_json, read_preference, timestamp_field,
				batch_max_docs, batch_max_bytes, batch_fetch_size, codec, codec_level, output_format,
				key_template, delete_after_archive, verify_before_delete, delete_batch_size,
				schedule_cron, schedule_every, schedule_at, schedule_timezone,
				max_docs_per_run, max_runtime, dry_run, sha256, updated_at, id, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			append(args, record.ID, timeText(now))...)
	} else {
		record.UpdatedAt = now
		_, err = tx.ExecContext(ctx, `
			UPDATE job_configs SET name = ?, enabled = ?, mongo_source_alias = ?, db_name = ?,
				collection_name = ?, filter_json = ?, projection_json = ?, read_preference = ?,
				timestamp_field = ?, batch_max_docs = ?, batch_max_bytes = ?, batch_fetch_size = ?,
				codec = ?, codec_level = ?, output_format = ?, key_template = ?,
				delete_after_archive = ?, verify_before_delete = ?, delete_batch_size = ?,
				schedule_cron = ?, schedule_every = ?, schedule_at = ?, schedule_timezone = ?,
				max_docs_per_run = ?, max_runtime = ?, dry_run = ?, sha256 = ?, updated_at = ?
			WHERE id = ?`, append(args, record.ID)...)
	}
	if err != nil {
		return aliasError("retention config", job.Name, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM job_destinations WHERE job_id = ?`, record.ID); err != nil {
		return err
	}
	for position, alias := range job.Destinations {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_destinations (job_id, destination_alias, position) VALUES (?,?,?)`,
			record.ID, alias, position); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetJobEnabled parks or resumes a config without touching anything else.
func (s *Store) SetJobEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job_configs SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), nowText(), id)
	return err
}

// SetJobDeleteAfterArchive flips the deletion switch for one config. This is
// the setting that decides whether archiving is a copy or a move, so it is
// changeable on its own, without editing anything else.
func (s *Store) SetJobDeleteAfterArchive(ctx context.Context, id string, deleteAfter bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job_configs SET delete_after_archive = ?, updated_at = ? WHERE id = ?`,
		boolToInt(deleteAfter), nowText(), id)
	return err
}

// DeleteJob removes a config. Its runs and archives stay in the catalogue —
// the data is still in the buckets, so the record of it must survive.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM job_configs WHERE id = ?`, id)
	return err
}

func (s *Store) attachJobDestinations(ctx context.Context, jobs map[string]*config.JobConfig) error {
	ids := make([]any, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")

	rows, err := s.db.QueryContext(ctx, `
		SELECT job_id, destination_alias FROM job_destinations
		WHERE job_id IN (`+placeholders+`) ORDER BY position`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var jobID, alias string
		if err := rows.Scan(&jobID, &alias); err != nil {
			return err
		}
		if job, ok := jobs[jobID]; ok {
			job.Destinations = append(job.Destinations, alias)
		}
	}
	return rows.Err()
}

func scanJob(row interface{ Scan(...any) error }) (JobRecord, error) {
	var (
		record                               JobRecord
		job                                  config.JobConfig
		enabled, deleteAfter, verify, dryRun int
		filterJSON, projectionJSON           string
		maxBytes                             int64
		maxRuntime                           string
		createdAt, updatedAt                 string
	)
	if err := row.Scan(&record.ID, &job.Name, &enabled, &job.Source.MongoSource, &job.Source.Database,
		&job.Source.Collection, &filterJSON, &projectionJSON, &job.Source.ReadPreference,
		&job.Source.TimestampField, &job.Batch.MaxDocs, &maxBytes, &job.Batch.FetchSize,
		&job.Compression.Codec, &job.Compression.Level, &job.Output.Format, &job.Output.KeyTemplate,
		&deleteAfter, &verify, &job.Retention.DeleteBatchSize, &job.Schedule.Cron, &job.Schedule.Every,
		&job.Schedule.At, &job.Schedule.Timezone, &job.Limits.MaxDocsPerRun, &maxRuntime,
		&dryRun, &job.SHA256, &createdAt, &updatedAt); err != nil {
		return record, err
	}

	job.Source.Filter = json.RawMessage(filterJSON)
	if projectionJSON != "" {
		job.Source.Projection = json.RawMessage(projectionJSON)
	}
	job.Batch.MaxBytes = config.ByteSize(maxBytes)
	job.Retention.DeleteAfterArchive = deleteAfter != 0
	verified := verify != 0
	job.Retention.VerifyBeforeDelete = &verified
	isEnabled := enabled != 0
	job.Enabled = &isEnabled
	job.DryRun = dryRun != 0
	if parsed, err := config.ParseDuration(maxRuntime); err == nil {
		job.Limits.MaxRuntime = config.Duration(parsed)
	}

	record.Config = &job
	record.CreatedAt, _ = parseTime(createdAt)
	record.UpdatedAt, _ = parseTime(updatedAt)
	return record, nil
}

func (s *Store) jobNames(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// aliasError turns SQLite's uniqueness complaint into something an operator can
// act on, since "UNIQUE constraint failed" does not name the field they typed.
func aliasError(kind, alias string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("a %s named %q already exists", kind, alias)
	}
	return err
}
