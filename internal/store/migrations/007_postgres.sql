-- PostgreSQL as a second source engine.
--
-- Accounts get their own table rather than a provider column on mongo_sources:
-- the two share an alias and a sealed connection string and nothing else, and
-- a config names one or the other through its own column, so an alias may be
-- reused across engines without either lookup seeing the other's.
CREATE TABLE IF NOT EXISTS postgres_sources (
    id               TEXT PRIMARY KEY,
    alias            TEXT NOT NULL UNIQUE,
    dsn_enc          BLOB NOT NULL,           -- AES-256-GCM; holds the password
    dsn_preview      TEXT NOT NULL,           -- redacted form, safe to display
    app_name         TEXT NOT NULL DEFAULT 'feather',
    enabled          INTEGER NOT NULL DEFAULT 1,
    note             TEXT,
    last_checked_at  TEXT,
    last_check_ok    INTEGER,
    last_check_error TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- Which engine a config reads. Every existing config is MongoDB, which is what
-- the default says. For PostgreSQL, db_name holds the schema and
-- collection_name the table, and mongo_source_alias is left empty.
ALTER TABLE job_configs ADD COLUMN source_engine TEXT NOT NULL DEFAULT 'mongodb';
ALTER TABLE job_configs ADD COLUMN postgres_source_alias TEXT;

-- Which engine an archive came from. A MongoDB archive is canonical Extended
-- JSON and a PostgreSQL one is row_to_json; restoring either needs to know
-- which, long after the config that wrote it may have been deleted.
ALTER TABLE feather_artifacts ADD COLUMN source_engine TEXT NOT NULL DEFAULT 'mongodb';
