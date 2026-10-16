-- Imports: archived documents put back into MongoDB from the panel.
--
-- Not feather_runs. A run belongs to a retention config, and everything that
-- reads that table — the health of each config, "last successful run", the
-- failure streak an alert fires on — would be skewed by rows that are not runs
-- of anything. An import is a person reading data back, so it gets its own
-- history.
--
-- Timestamps use the same fixed-width text layout as every other table, so
-- ORDER BY started_at agrees with time order (see timeLayout in db.go).
CREATE TABLE IF NOT EXISTS feather_imports (
    id                TEXT PRIMARY KEY,

    -- Who started it. Email rather than a foreign key, for the same reason as
    -- the audit log: the record has to outlive the account.
    actor             TEXT NOT NULL,
    actor_ip          TEXT NOT NULL DEFAULT '',

    -- Where the documents came from: one catalogued archive, every archive of
    -- one run, or a file uploaded through the browser.
    source_kind       TEXT NOT NULL,           -- archive | run | upload
    source_ref        TEXT NOT NULL DEFAULT '', -- archive or run id; empty for an upload
    source_label      TEXT NOT NULL DEFAULT '', -- file name or run description, for display
    prefer_alias      TEXT NOT NULL DEFAULT '', -- destination to read from; empty means any

    -- Where they went.
    target_alias      TEXT NOT NULL,
    target_db         TEXT NOT NULL,
    target_collection TEXT NOT NULL,

    mode              TEXT NOT NULL,           -- insert | replace
    dry_run           INTEGER NOT NULL DEFAULT 0,

    status            TEXT NOT NULL,
    started_at        TEXT NOT NULL,
    finished_at       TEXT,

    files_total       INTEGER NOT NULL DEFAULT 0,
    files_done        INTEGER NOT NULL DEFAULT 0,
    docs_expected     INTEGER NOT NULL DEFAULT 0,
    bytes_expected    INTEGER NOT NULL DEFAULT 0,
    docs_read         INTEGER NOT NULL DEFAULT 0,
    docs_inserted     INTEGER NOT NULL DEFAULT 0,
    docs_present      INTEGER NOT NULL DEFAULT 0,
    docs_replaced     INTEGER NOT NULL DEFAULT 0,
    bytes_read        INTEGER NOT NULL DEFAULT 0,

    -- The digest of what was read: checked against the catalogue for an
    -- archive, recorded for an upload so the file can be identified later.
    sha256            TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_imports_started ON feather_imports(started_at DESC);
