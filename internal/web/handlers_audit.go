package web

import (
	"net/http"
	"strconv"

	"github.com/unifers/feather/internal/store"
)

// auditPageSize is how many events one page shows.
const auditPageSize = 60

// handleAudit shows who changed what.
//
// It is read-only and has no bulk actions on purpose: an audit trail an
// operator can edit from the panel is not an audit trail. Pruning is a
// scheduled job, not a button.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	query := store.AuditQuery{
		Action: r.URL.Query().Get("action"),
		Actor:  r.URL.Query().Get("actor"),
		Target: r.URL.Query().Get("target"),
		Limit:  auditPageSize,
	}

	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			page = parsed
		}
	}
	query.Offset = (page - 1) * auditPageSize

	events, total, err := s.app.Store.AuditEvents(r.Context(), query)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	pages := int((total + auditPageSize - 1) / auditPageSize)
	s.render(w, r, "audit", pageData{
		Title: "Activity",
		Nav:   "audit",
		Data: map[string]any{
			"Events":  events,
			"Total":   total,
			"Page":    page,
			"Pages":   pages,
			"HasPrev": page > 1,
			"HasNext": page < pages,
			"Filter":  query,
			"Actions": auditActionLabels,
		},
	})
}

// auditActionLabels give each recorded action a readable name, and fix the
// order of the filter dropdown so it does not shuffle between renders.
var auditActionLabels = []struct{ Action, Label string }{
	{store.AuditDeletionEnabled, "Deletion turned on"},
	{store.AuditDeletionDisabled, "Deletion turned off"},
	{store.AuditConfigSaved, "Config saved"},
	{store.AuditConfigDeleted, "Config removed"},
	{store.AuditConfigEnabled, "Config resumed"},
	{store.AuditConfigDisabled, "Config paused"},
	{store.AuditRunStarted, "Run started"},
	{store.AuditImportReplaceStarted, "Import started, overwriting by _id"},
	{store.AuditImportStarted, "Import started"},
	{store.AuditImportFinished, "Import finished"},
	{store.AuditSourceSaved, "MongoDB account saved"},
	{store.AuditSourceDeleted, "MongoDB account removed"},
	{store.AuditPostgresSaved, "PostgreSQL account saved"},
	{store.AuditPostgresDeleted, "PostgreSQL account removed"},
	{store.AuditDestSaved, "Destination saved"},
	{store.AuditDestDeleted, "Destination removed"},
	{store.AuditDriveConnected, "Google Drive connected"},
	{store.AuditDriveDisconnected, "Google Drive disconnected"},
	{store.AuditAdminCreated, "Administrator added"},
	{store.AuditAdminDeleted, "Administrator removed"},
	{store.AuditPasswordChanged, "Password changed"},
	{store.AuditSettingsSaved, "Settings saved"},
	{store.AuditBundleExported, "Bundle exported"},
	{store.AuditBundleImported, "Bundle imported"},
}

// auditLabel is the readable name for an action, falling back to the raw verb
// so an action added later still renders rather than showing a blank cell.
func auditLabel(action string) string {
	for _, entry := range auditActionLabels {
		if entry.Action == action {
			return entry.Label
		}
	}
	return action
}
