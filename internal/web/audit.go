package web

import (
	"context"
	"net/http"
	"time"

	"github.com/unifers/feather/internal/store"
)

// audit records one change, attributed to whoever is signed in.
//
// It never returns an error and never blocks the response. That is a deliberate
// trade: an audit write failing must not undo or hide a change the operator
// already made, and a change that happened but was not recorded is a smaller
// problem than one that was refused because the log was busy. The failure is
// logged at error level, which is where anyone monitoring the trail's integrity
// will be looking.
//
// The context is detached from the request on purpose. A browser closing the
// connection right after a successful POST must not be the reason the record of
// it is missing.
func (s *Server) audit(r *http.Request, action, targetKind, target, detail string) {
	event := store.AuditEvent{
		Actor:      currentUserEmail(r),
		ActorIP:    s.clientIP(r),
		Action:     action,
		TargetKind: targetKind,
		Target:     target,
		Detail:     detail,
	}
	if event.Actor == "" {
		// Reachable from the setup flow, before a session exists.
		event.Actor = "(unauthenticated)"
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()

	if err := s.app.Store.RecordAudit(ctx, event); err != nil {
		s.log.Error("could not record an audit event",
			"action", action, "target", target, "actor", event.Actor, "error", err)
	}
}
