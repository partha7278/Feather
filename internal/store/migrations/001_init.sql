-- feather keeps everything in one SQLite file: who may log in, which clusters
-- to read, where archives go, what each retention job does, and a record of
-- every file ever written. Timestamps are RFC3339 UTC text; booleans are 0/1.

-- Small key/value bag for process-wide settings the UI can edit.
CREATE TABLE IF NOT EXISTS feather_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- ---------------------------------------------------------------------------
-- Administration
-- ---------------------------------------------------------------------------

-- The first-run setup creates exactly one of these; more can be added later.
CREATE TABLE IF NOT EXISTS admin_users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL COLLATE NOCASE UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    last_login_at TEXT
);

-- Only the hash of a session token is stored, so a stolen database file cannot
-- be turned into a live session.
CREATE TABLE IF NOT EXISTS admin_sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES admin_users (id) ON DELETE CASCADE,
    csrf_token TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    user_agent TEXT,
    ip         TEXT
);

CREATE INDEX IF NOT EXISTS admin_sessions_user_idx    ON admin_sessions (user_id);
CREATE INDEX IF NOT EXISTS admin_sessions_expires_idx ON admin_sessions (expires_at);

-- ---------------------------------------------------------------------------
-- Credentials
-- ---------------------------------------------------------------------------

-- One MongoDB account feather reads from. Several may exist; each retention
-- config names the one it archives.
CREATE TABLE IF NOT EXISTS mongo_sources (
    id               TEXT PRIMARY KEY,
    alias            TEXT NOT NULL UNIQUE,
    uri_enc          BLOB NOT NULL,           -- AES-256-GCM; holds the password
    uri_preview      TEXT NOT NULL,           -- redacted form, safe to display
    auth_source      TEXT NOT NULL DEFAULT 'admin',
    app_name         TEXT NOT NULL DEFAULT 'feather',
    enabled          INTEGER NOT NULL DEFAULT 1,
    note             TEXT,
    last_checked_at  TEXT,
    last_check_ok    INTEGER,
    last_check_error TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- One backup destination: an AWS S3 account, or Oracle Cloud Object Storage
-- through its S3-compatible endpoint. Any number of each.
CREATE TABLE IF NOT EXISTS destinations (
    id                    TEXT PRIMARY KEY,
    alias                 TEXT NOT NULL UNIQUE,
    provider              TEXT NOT NULL,      -- 's3' | 'oci'
    region                TEXT NOT NULL,
    bucket                TEXT NOT NULL,
    endpoint              TEXT,
    access_key_id         TEXT,
    secret_access_key_enc BLOB,               -- AES-256-GCM
    session_token_enc     BLOB,               -- AES-256-GCM
    storage_class         TEXT,
    key_prefix            TEXT,
    force_path_style      INTEGER NOT NULL DEFAULT 0,
    enabled               INTEGER NOT NULL DEFAULT 1,
    note                  TEXT,
    last_checked_at       TEXT,
    last_check_ok         INTEGER,
    last_check_error      TEXT,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);

-- ---------------------------------------------------------------------------
-- Retention jobs
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS job_configs (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL UNIQUE,
    enabled            INTEGER NOT NULL DEFAULT 1,

    mongo_source_alias TEXT NOT NULL,
    db_name            TEXT NOT NULL,
    collection_name    TEXT NOT NULL,
    filter_json        TEXT NOT NULL DEFAULT '{}',
    projection_json    TEXT,
    read_preference    TEXT NOT NULL DEFAULT 'primary',
    timestamp_field    TEXT,

    batch_max_docs     INTEGER NOT NULL DEFAULT 50000,
    batch_max_bytes    INTEGER NOT NULL DEFAULT 268435456,
    batch_fetch_size   INTEGER NOT NULL DEFAULT 1000,

    codec              TEXT    NOT NULL DEFAULT 'gzip',
    codec_level        INTEGER NOT NULL DEFAULT 6,
    output_format      TEXT    NOT NULL DEFAULT 'ndjson',
    -- The operator's own file naming scheme, in {variable} form.
    key_template       TEXT    NOT NULL,

    -- The deletion switch: whether the source documents go once every copy of
    -- the archive is stored and verified.
    delete_after_archive INTEGER NOT NULL DEFAULT 0,
    verify_before_delete INTEGER NOT NULL DEFAULT 1,
    delete_batch_size    INTEGER NOT NULL DEFAULT 5000,

    schedule_cron      TEXT,
    schedule_every     TEXT,
    schedule_at        TEXT,
    schedule_timezone  TEXT NOT NULL DEFAULT 'UTC',

    max_docs_per_run   INTEGER NOT NULL DEFAULT 0,
    max_runtime        TEXT    NOT NULL DEFAULT '6h',
    dry_run            INTEGER NOT NULL DEFAULT 0,

    sha256             TEXT NOT NULL,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);

-- A job writes a full copy to each of its destinations; position preserves the
-- order the operator chose, which is the order logs and the catalogue use.
CREATE TABLE IF NOT EXISTS job_destinations (
    job_id            TEXT    NOT NULL REFERENCES job_configs (id) ON DELETE CASCADE,
    destination_alias TEXT    NOT NULL,
    position          INTEGER NOT NULL,
    PRIMARY KEY (job_id, destination_alias)
);

-- ---------------------------------------------------------------------------
-- Catalogue
-- ---------------------------------------------------------------------------

-- One row per execution of a retention config.
CREATE TABLE IF NOT EXISTS feather_runs (
    id               TEXT PRIMARY KEY,
    config_name      TEXT NOT NULL,
    config_sha256    TEXT NOT NULL,
    db_name          TEXT NOT NULL,
    collection_name  TEXT NOT NULL,
    filter_json      TEXT NOT NULL,
    trigger          TEXT NOT NULL,
    host             TEXT NOT NULL,
    status           TEXT NOT NULL,
    dry_run          INTEGER NOT NULL DEFAULT 0,
    started_at       TEXT NOT NULL,
    finished_at      TEXT,
    docs_scanned     INTEGER NOT NULL DEFAULT 0,
    docs_archived    INTEGER NOT NULL DEFAULT 0,
    docs_deleted     INTEGER NOT NULL DEFAULT 0,
    bytes_raw        INTEGER NOT NULL DEFAULT 0,
    bytes_compressed INTEGER NOT NULL DEFAULT 0,
    batch_count      INTEGER NOT NULL DEFAULT 0,
    error            TEXT
);

CREATE INDEX IF NOT EXISTS feather_runs_config_started_idx ON feather_runs (config_name, started_at DESC);
CREATE INDEX IF NOT EXISTS feather_runs_status_idx         ON feather_runs (status);
CREATE INDEX IF NOT EXISTS feather_runs_started_idx        ON feather_runs (started_at DESC);

-- One row per compressed file produced: its name, when it was written, how many
-- documents it holds, how big it is, and whether the source data was deleted.
CREATE TABLE IF NOT EXISTS feather_artifacts (
    id                  TEXT PRIMARY KEY,
    run_id              TEXT    NOT NULL REFERENCES feather_runs (id) ON DELETE CASCADE,
    config_name         TEXT    NOT NULL,
    db_name             TEXT    NOT NULL,
    collection_name     TEXT    NOT NULL,
    batch_seq           INTEGER NOT NULL,
    file_name           TEXT    NOT NULL,
    format              TEXT    NOT NULL,
    codec               TEXT    NOT NULL,
    doc_count           INTEGER NOT NULL,
    bytes_raw           INTEGER NOT NULL,
    bytes_compressed    INTEGER NOT NULL,
    sha256              TEXT    NOT NULL,
    min_id              TEXT,
    max_id              TEXT,
    min_ts              TEXT,
    max_ts              TEXT,
    deleted_from_source INTEGER NOT NULL DEFAULT 0,
    deleted_count       INTEGER NOT NULL DEFAULT 0,
    deleted_at          TEXT,
    created_at          TEXT    NOT NULL,
    UNIQUE (run_id, batch_seq)
);

CREATE INDEX IF NOT EXISTS feather_artifacts_run_idx     ON feather_artifacts (run_id);
CREATE INDEX IF NOT EXISTS feather_artifacts_config_idx  ON feather_artifacts (config_name, created_at DESC);
CREATE INDEX IF NOT EXISTS feather_artifacts_ts_idx      ON feather_artifacts (min_ts, max_ts);
CREATE INDEX IF NOT EXISTS feather_artifacts_file_idx    ON feather_artifacts (file_name);

-- One row per (artifact x destination): where that exact file landed.
CREATE TABLE IF NOT EXISTS feather_artifact_destinations (
    id                TEXT PRIMARY KEY,
    artifact_id       TEXT    NOT NULL REFERENCES feather_artifacts (id) ON DELETE CASCADE,
    destination_alias TEXT    NOT NULL,
    provider          TEXT    NOT NULL,
    bucket            TEXT    NOT NULL,
    object_key        TEXT    NOT NULL,
    location_url      TEXT    NOT NULL,   -- the full path to this copy
    region            TEXT,
    endpoint          TEXT,
    storage_class     TEXT,
    etag              TEXT,
    size_bytes        INTEGER NOT NULL DEFAULT 0,
    status            TEXT    NOT NULL,
    uploaded_at       TEXT,
    verified_at       TEXT,
    error             TEXT,
    created_at        TEXT    NOT NULL,
    UNIQUE (artifact_id, destination_alias)
);

CREATE INDEX IF NOT EXISTS feather_artifact_destinations_artifact_idx ON feather_artifact_destinations (artifact_id);
CREATE INDEX IF NOT EXISTS feather_artifact_destinations_key_idx      ON feather_artifact_destinations (bucket, object_key);

-- A run holds this for its duration so two processes cannot archive the same
-- config at once. The expiry is what lets a crashed process's lease be taken
-- over instead of blocking the config forever.
CREATE TABLE IF NOT EXISTS job_leases (
    config_name TEXT PRIMARY KEY,
    holder      TEXT NOT NULL,
    host        TEXT NOT NULL,
    pid         INTEGER NOT NULL,
    acquired_at TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
