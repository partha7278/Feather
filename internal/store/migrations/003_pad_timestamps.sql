-- Normalise stored timestamps to a fixed width.
--
-- Timestamps live in TEXT columns and are ordered and compared as text:
-- `ORDER BY started_at DESC`, `WHERE expires_at < ?`, `MAX(finished_at)`. They
-- were written with Go's ".999" layout, which trims trailing zeros, so the same
-- instant could be stored as any of:
--
--     2026-08-15T12:00:00Z        (landed on a whole second)
--     2026-08-15T12:00:00.1Z
--     2026-08-15T12:00:00.12Z
--     2026-08-15T12:00:00.123Z
--
-- Text comparison then disagrees with chronological order: '.' (0x2E) sorts
-- before 'Z' (0x5A), so "12:00:00.005Z" compares as EARLIER than "12:00:00Z"
-- even though it is five milliseconds later. That made "the latest run"
-- occasionally the wrong run, and it misanchored interval schedules, which read
-- the last success time to decide when to fire next.
--
-- feather now writes ".000000" — always six digits. Six rather than three
-- because ties matter too: a run's rows are written in well under a
-- millisecond, so at millisecond precision two runs regularly shared a
-- timestamp and came back in whatever order the scan produced.
--
-- Each statement below rebuilds the value as
--
--     <everything before the '.' or the 'Z'> . <fraction padded to 6> Z
--
-- which leaves an already-normalised value untouched, so rerunning this is a
-- no-op. Values are UTC, so the suffix is always 'Z'; anything not shaped like
-- a timestamp is passed through unchanged.

-- feather_meta
UPDATE feather_meta SET updated_at =
    (CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, 1, instr(updated_at, '.') - 1) ELSE substr(updated_at, 1, length(updated_at) - 1) END)
    || '.' || substr((CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, instr(updated_at, '.') + 1, length(updated_at) - instr(updated_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE updated_at LIKE '%T%Z';

-- admin_users
UPDATE admin_users SET created_at =
    (CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, 1, instr(created_at, '.') - 1) ELSE substr(created_at, 1, length(created_at) - 1) END)
    || '.' || substr((CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, instr(created_at, '.') + 1, length(created_at) - instr(created_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE created_at LIKE '%T%Z';
UPDATE admin_users SET updated_at =
    (CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, 1, instr(updated_at, '.') - 1) ELSE substr(updated_at, 1, length(updated_at) - 1) END)
    || '.' || substr((CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, instr(updated_at, '.') + 1, length(updated_at) - instr(updated_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE updated_at LIKE '%T%Z';
UPDATE admin_users SET last_login_at =
    (CASE WHEN instr(last_login_at, '.') > 0 THEN substr(last_login_at, 1, instr(last_login_at, '.') - 1) ELSE substr(last_login_at, 1, length(last_login_at) - 1) END)
    || '.' || substr((CASE WHEN instr(last_login_at, '.') > 0 THEN substr(last_login_at, instr(last_login_at, '.') + 1, length(last_login_at) - instr(last_login_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE last_login_at IS NOT NULL AND last_login_at LIKE '%T%Z';

-- admin_sessions
UPDATE admin_sessions SET created_at =
    (CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, 1, instr(created_at, '.') - 1) ELSE substr(created_at, 1, length(created_at) - 1) END)
    || '.' || substr((CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, instr(created_at, '.') + 1, length(created_at) - instr(created_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE created_at LIKE '%T%Z';
UPDATE admin_sessions SET expires_at =
    (CASE WHEN instr(expires_at, '.') > 0 THEN substr(expires_at, 1, instr(expires_at, '.') - 1) ELSE substr(expires_at, 1, length(expires_at) - 1) END)
    || '.' || substr((CASE WHEN instr(expires_at, '.') > 0 THEN substr(expires_at, instr(expires_at, '.') + 1, length(expires_at) - instr(expires_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE expires_at LIKE '%T%Z';

-- mongo_sources
UPDATE mongo_sources SET created_at =
    (CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, 1, instr(created_at, '.') - 1) ELSE substr(created_at, 1, length(created_at) - 1) END)
    || '.' || substr((CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, instr(created_at, '.') + 1, length(created_at) - instr(created_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE created_at LIKE '%T%Z';
UPDATE mongo_sources SET updated_at =
    (CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, 1, instr(updated_at, '.') - 1) ELSE substr(updated_at, 1, length(updated_at) - 1) END)
    || '.' || substr((CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, instr(updated_at, '.') + 1, length(updated_at) - instr(updated_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE updated_at LIKE '%T%Z';
UPDATE mongo_sources SET last_checked_at =
    (CASE WHEN instr(last_checked_at, '.') > 0 THEN substr(last_checked_at, 1, instr(last_checked_at, '.') - 1) ELSE substr(last_checked_at, 1, length(last_checked_at) - 1) END)
    || '.' || substr((CASE WHEN instr(last_checked_at, '.') > 0 THEN substr(last_checked_at, instr(last_checked_at, '.') + 1, length(last_checked_at) - instr(last_checked_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE last_checked_at IS NOT NULL AND last_checked_at LIKE '%T%Z';

-- destinations
UPDATE destinations SET created_at =
    (CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, 1, instr(created_at, '.') - 1) ELSE substr(created_at, 1, length(created_at) - 1) END)
    || '.' || substr((CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, instr(created_at, '.') + 1, length(created_at) - instr(created_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE created_at LIKE '%T%Z';
UPDATE destinations SET updated_at =
    (CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, 1, instr(updated_at, '.') - 1) ELSE substr(updated_at, 1, length(updated_at) - 1) END)
    || '.' || substr((CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, instr(updated_at, '.') + 1, length(updated_at) - instr(updated_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE updated_at LIKE '%T%Z';
UPDATE destinations SET last_checked_at =
    (CASE WHEN instr(last_checked_at, '.') > 0 THEN substr(last_checked_at, 1, instr(last_checked_at, '.') - 1) ELSE substr(last_checked_at, 1, length(last_checked_at) - 1) END)
    || '.' || substr((CASE WHEN instr(last_checked_at, '.') > 0 THEN substr(last_checked_at, instr(last_checked_at, '.') + 1, length(last_checked_at) - instr(last_checked_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE last_checked_at IS NOT NULL AND last_checked_at LIKE '%T%Z';

-- job_configs
UPDATE job_configs SET created_at =
    (CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, 1, instr(created_at, '.') - 1) ELSE substr(created_at, 1, length(created_at) - 1) END)
    || '.' || substr((CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, instr(created_at, '.') + 1, length(created_at) - instr(created_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE created_at LIKE '%T%Z';
UPDATE job_configs SET updated_at =
    (CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, 1, instr(updated_at, '.') - 1) ELSE substr(updated_at, 1, length(updated_at) - 1) END)
    || '.' || substr((CASE WHEN instr(updated_at, '.') > 0 THEN substr(updated_at, instr(updated_at, '.') + 1, length(updated_at) - instr(updated_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE updated_at LIKE '%T%Z';

-- feather_runs
UPDATE feather_runs SET started_at =
    (CASE WHEN instr(started_at, '.') > 0 THEN substr(started_at, 1, instr(started_at, '.') - 1) ELSE substr(started_at, 1, length(started_at) - 1) END)
    || '.' || substr((CASE WHEN instr(started_at, '.') > 0 THEN substr(started_at, instr(started_at, '.') + 1, length(started_at) - instr(started_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE started_at LIKE '%T%Z';
UPDATE feather_runs SET finished_at =
    (CASE WHEN instr(finished_at, '.') > 0 THEN substr(finished_at, 1, instr(finished_at, '.') - 1) ELSE substr(finished_at, 1, length(finished_at) - 1) END)
    || '.' || substr((CASE WHEN instr(finished_at, '.') > 0 THEN substr(finished_at, instr(finished_at, '.') + 1, length(finished_at) - instr(finished_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE finished_at IS NOT NULL AND finished_at LIKE '%T%Z';

-- feather_artifacts
UPDATE feather_artifacts SET created_at =
    (CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, 1, instr(created_at, '.') - 1) ELSE substr(created_at, 1, length(created_at) - 1) END)
    || '.' || substr((CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, instr(created_at, '.') + 1, length(created_at) - instr(created_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE created_at LIKE '%T%Z';
UPDATE feather_artifacts SET deleted_at =
    (CASE WHEN instr(deleted_at, '.') > 0 THEN substr(deleted_at, 1, instr(deleted_at, '.') - 1) ELSE substr(deleted_at, 1, length(deleted_at) - 1) END)
    || '.' || substr((CASE WHEN instr(deleted_at, '.') > 0 THEN substr(deleted_at, instr(deleted_at, '.') + 1, length(deleted_at) - instr(deleted_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE deleted_at IS NOT NULL AND deleted_at LIKE '%T%Z';

-- feather_artifact_destinations
UPDATE feather_artifact_destinations SET created_at =
    (CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, 1, instr(created_at, '.') - 1) ELSE substr(created_at, 1, length(created_at) - 1) END)
    || '.' || substr((CASE WHEN instr(created_at, '.') > 0 THEN substr(created_at, instr(created_at, '.') + 1, length(created_at) - instr(created_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE created_at LIKE '%T%Z';
UPDATE feather_artifact_destinations SET uploaded_at =
    (CASE WHEN instr(uploaded_at, '.') > 0 THEN substr(uploaded_at, 1, instr(uploaded_at, '.') - 1) ELSE substr(uploaded_at, 1, length(uploaded_at) - 1) END)
    || '.' || substr((CASE WHEN instr(uploaded_at, '.') > 0 THEN substr(uploaded_at, instr(uploaded_at, '.') + 1, length(uploaded_at) - instr(uploaded_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE uploaded_at IS NOT NULL AND uploaded_at LIKE '%T%Z';
UPDATE feather_artifact_destinations SET verified_at =
    (CASE WHEN instr(verified_at, '.') > 0 THEN substr(verified_at, 1, instr(verified_at, '.') - 1) ELSE substr(verified_at, 1, length(verified_at) - 1) END)
    || '.' || substr((CASE WHEN instr(verified_at, '.') > 0 THEN substr(verified_at, instr(verified_at, '.') + 1, length(verified_at) - instr(verified_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE verified_at IS NOT NULL AND verified_at LIKE '%T%Z';

-- job_leases
UPDATE job_leases SET acquired_at =
    (CASE WHEN instr(acquired_at, '.') > 0 THEN substr(acquired_at, 1, instr(acquired_at, '.') - 1) ELSE substr(acquired_at, 1, length(acquired_at) - 1) END)
    || '.' || substr((CASE WHEN instr(acquired_at, '.') > 0 THEN substr(acquired_at, instr(acquired_at, '.') + 1, length(acquired_at) - instr(acquired_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE acquired_at LIKE '%T%Z';
UPDATE job_leases SET expires_at =
    (CASE WHEN instr(expires_at, '.') > 0 THEN substr(expires_at, 1, instr(expires_at, '.') - 1) ELSE substr(expires_at, 1, length(expires_at) - 1) END)
    || '.' || substr((CASE WHEN instr(expires_at, '.') > 0 THEN substr(expires_at, instr(expires_at, '.') + 1, length(expires_at) - instr(expires_at, '.') - 1) ELSE '' END) || '000000', 1, 6) || 'Z'
WHERE expires_at LIKE '%T%Z';
