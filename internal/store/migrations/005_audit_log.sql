-- Who changed what.
--
-- feather deletes production data on a schedule, and the setting that decides
-- whether it does was changeable in one click while recording only that it had
-- changed — not by whom. Config, account and destination edits were not
-- recorded at all. "Why did last night's run delete 40,000 orders?" had no
-- answer in the product; it had to be reconstructed from process logs, if those
-- were still around.
--
-- This is deliberately a flat table rather than a general event store. It holds
-- one row per mutation, with enough context to answer the question without
-- needing to join anything, and it is never written on a read path.
CREATE TABLE IF NOT EXISTS audit_events (
    id          TEXT PRIMARY KEY,
    at          TEXT NOT NULL,

    -- Who. Email rather than a foreign key to admin_users: the trail has to
    -- survive the account being deleted, which is exactly when it matters.
    actor       TEXT NOT NULL,
    actor_ip    TEXT NOT NULL DEFAULT '',

    -- What. A short stable verb — "config.deletion.enabled", "admin.removed" —
    -- so the log can be filtered without matching prose.
    action      TEXT NOT NULL,

    -- To what.
    target_kind TEXT NOT NULL DEFAULT '',
    target      TEXT NOT NULL DEFAULT '',

    -- Anything worth keeping about the change, as a short human-readable note.
    -- Never a credential: everything written here is rendered in the UI.
    detail      TEXT NOT NULL DEFAULT ''
);

-- The listing is "newest first", optionally narrowed to one target.
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_events(at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_events(target_kind, target, at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_events(action, at DESC);
