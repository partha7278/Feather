package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The audit actions feather records. They are short, stable strings rather than
// prose so the log can be filtered and alerted on: "show me every time deletion
// was turned on" should be a query, not a grep.
const (
	AuditSignedIn        = "auth.signed_in"
	AuditSignInFailed    = "auth.sign_in_failed"
	AuditSignedOut       = "auth.signed_out"
	AuditPasswordChanged = "auth.password_changed"
	AuditAdminCreated    = "admin.created"
	AuditAdminDeleted    = "admin.deleted"

	AuditConfigSaved    = "config.saved"
	AuditConfigDeleted  = "config.deleted"
	AuditConfigEnabled  = "config.enabled"
	AuditConfigDisabled = "config.disabled"
	// The two that destroy data, and the reason this table exists.
	AuditDeletionEnabled  = "config.deletion.enabled"
	AuditDeletionDisabled = "config.deletion.disabled"
	AuditRunStarted       = "config.run_started"

	AuditSourceSaved       = "mongo_source.saved"
	AuditSourceDeleted     = "mongo_source.deleted"
	AuditDestSaved         = "destination.saved"
	AuditDestDeleted       = "destination.deleted"
	AuditDriveConnected    = "destination.drive_connected"
	AuditDriveDisconnected = "destination.drive_disconnected"

	// Reading an archive is worth recording too: it is how the data leaves
	// feather, and "who pulled the January orders?" is a question someone
	// eventually asks.
	AuditArchiveDownloaded = "archive.downloaded"
	AuditDatabaseBackedUp  = "settings.backup_taken"

	AuditSettingsSaved  = "settings.saved"
	AuditBundleExported = "settings.bundle_exported"
	AuditBundleImported = "settings.bundle_imported"
)

// AuditEvent is one recorded change.
type AuditEvent struct {
	ID         string
	At         time.Time
	Actor      string
	ActorIP    string
	Action     string
	TargetKind string
	Target     string
	Detail     string
}

// Destructive reports whether this event changed something that can lose data,
// so the UI can mark it. These are the rows an operator scans for first.
func (e AuditEvent) Destructive() bool {
	switch e.Action {
	case AuditDeletionEnabled, AuditConfigDeleted, AuditSourceDeleted,
		AuditDestDeleted, AuditAdminDeleted, AuditBundleImported:
		return true
	}
	return false
}

// RecordAudit writes one event.
//
// It takes a context that should not be the request's: the point of an audit
// trail is that it survives, and a browser disconnecting after a successful
// mutation must not be the reason the record of it is missing.
func (s *Store) RecordAudit(ctx context.Context, event AuditEvent) error {
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, at, actor, actor_ip, action, target_kind, target, detail)
		VALUES (?,?,?,?,?,?,?,?)`,
		event.ID, timeText(event.At), event.Actor, event.ActorIP, event.Action,
		event.TargetKind, event.Target, truncate(event.Detail, 1000))
	if err != nil {
		return fmt.Errorf("recording audit event: %w", err)
	}
	return nil
}

// AuditQuery narrows the audit listing.
type AuditQuery struct {
	Action     string
	TargetKind string
	Target     string
	Actor      string
	Limit      int
	Offset     int
}

// AuditEvents returns matching events, newest first, with the total count.
func (s *Store) AuditEvents(ctx context.Context, q AuditQuery) ([]AuditEvent, int64, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}

	where := []string{"1=1"}
	args := []any{}
	if q.Action != "" {
		where = append(where, "action = ?")
		args = append(args, q.Action)
	}
	if q.TargetKind != "" {
		where = append(where, "target_kind = ?")
		args = append(args, q.TargetKind)
	}
	if q.Target != "" {
		where = append(where, "target = ?")
		args = append(args, q.Target)
	}
	if q.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, q.Actor)
	}
	clause := " WHERE " + joinAnd(where)

	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// rowid breaks ties, so a page refresh cannot reorder two events recorded
	// in the same microsecond.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, actor, actor_ip, action, target_kind, target, detail
		 FROM audit_events`+clause+` ORDER BY at DESC, rowid DESC LIMIT ? OFFSET ?`,
		append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var out []AuditEvent
	for rows.Next() {
		var event AuditEvent
		var at string
		if err := rows.Scan(&event.ID, &at, &event.Actor, &event.ActorIP,
			&event.Action, &event.TargetKind, &event.Target, &event.Detail); err != nil {
			return nil, 0, err
		}
		parsed, err := parseTime(at)
		if err != nil {
			return nil, 0, err
		}
		event.At = parsed
		out = append(out, event)
	}
	return out, total, rows.Err()
}

// PurgeAuditBefore drops events older than a cutoff, so the trail does not grow
// without limit on an installation that has been running for years.
func (s *Store) PurgeAuditBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM audit_events WHERE at < ?`, timeText(cutoff.UTC()))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func joinAnd(parts []string) string {
	out := parts[0]
	for _, part := range parts[1:] {
		out += " AND " + part
	}
	return out
}
