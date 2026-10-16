package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// --- administrators and sessions ------------------------------------------

// Someone changing their password because they fear a stolen session expects
// the thief to be logged out — and there is no telling the thief's session
// from their own, so every session goes.
func TestChangePasswordSignsOutEverySessionIncludingTheCallers(t *testing.T) {
	st, ctx := testStore(t)

	user, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	mine, _, err := st.CreateSession(ctx, user.ID, "firefox", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	theirs, _, err := st.CreateSession(ctx, user.ID, "curl", "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.ChangePassword(ctx, user.ID, "correct-horse-battery", "a-brand-new-passphrase"); err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{"the caller's": mine, "another": theirs} {
		if _, _, err := st.SessionByToken(ctx, token); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s session survived a password change: %v", name, err)
		}
	}
	if _, err := st.Authenticate(ctx, "ops@example.com", "correct-horse-battery"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("the old password still works: %v", err)
	}
	if _, err := st.Authenticate(ctx, "ops@example.com", "a-brand-new-passphrase"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

func TestChangePasswordRefusesWithoutChangingAnything(t *testing.T) {
	st, ctx := testStore(t)

	user, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreateSession(ctx, user.ID, "firefox", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, userID, current, next, wantErr string
	}{
		{"a wrong current password", user.ID, "not-the-password", "a-brand-new-passphrase", "current password is not correct"},
		{"a new password that is too short", user.ID, "correct-horse-battery", "short", "at least"},
		{"the same password again", user.ID, "correct-horse-battery", "correct-horse-battery", "same as the current"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.ChangePassword(ctx, tc.userID, tc.current, tc.next)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}

	if err := st.ChangePassword(ctx, "no-such-user", "x", "a-brand-new-passphrase"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown user gave %v", err)
	}

	// A refused change must leave the session and the password alone.
	if _, _, err := st.SessionByToken(ctx, token); err != nil {
		t.Errorf("a refused change still signed the user out: %v", err)
	}
	if _, err := st.Authenticate(ctx, "ops@example.com", "correct-horse-battery"); err != nil {
		t.Errorf("a refused change altered the password: %v", err)
	}
}

func TestListAdminsIsSortedByEmailAndRecordsLogins(t *testing.T) {
	st, ctx := testStore(t)

	admins, err := st.ListAdmins(ctx)
	if err != nil || len(admins) != 0 {
		t.Fatalf("a fresh install lists %d admins (%v)", len(admins), err)
	}

	for _, email := range []string{"zoe@example.com", "amir@example.com"} {
		if _, err := st.CreateAdmin(ctx, email, "correct-horse-battery"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Authenticate(ctx, "zoe@example.com", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}

	admins, err = st.ListAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(admins) != 2 || admins[0].Email != "amir@example.com" || admins[1].Email != "zoe@example.com" {
		t.Fatalf("admins listed as %v", admins)
	}
	if admins[0].LastLoginAt != nil {
		t.Error("an admin who never signed in has a last login time")
	}
	if admins[1].LastLoginAt == nil || time.Since(*admins[1].LastLoginAt) > time.Minute {
		t.Errorf("the sign-in was not recorded: %v", admins[1].LastLoginAt)
	}
	if admins[0].CreatedAt.IsZero() || admins[0].ID == "" {
		t.Error("listed admins are missing their identity or creation time")
	}
}

// Someone working in the UI must not be logged out mid-edit.
func TestTouchSessionPushesTheExpiryOut(t *testing.T) {
	st, ctx := testStore(t)

	user, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreateSession(ctx, user.ID, "firefox", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE admin_sessions SET expires_at = ?`,
		timeText(time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	if err := st.TouchSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	session, _, err := st.SessionByToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if session.ExpiresAt.Before(before.Add(SessionTTL - time.Minute)) {
		t.Errorf("the session expires at %s, not a full TTL from now", session.ExpiresAt)
	}

	if err := st.TouchSession(ctx, "not-a-real-token"); err != nil {
		t.Errorf("touching an unknown token should be a quiet no-op: %v", err)
	}
}

func TestPurgeExpiredSessionsRemovesOnlyTheDeadOnes(t *testing.T) {
	st, ctx := testStore(t)

	user, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	live, _, err := st.CreateSession(ctx, user.ID, "firefox", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, dead, err := st.CreateSession(ctx, user.ID, "curl", "10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE admin_sessions SET expires_at = ? WHERE token_hash = ?`,
		timeText(time.Now().Add(-time.Second)), dead.TokenHash); err != nil {
		t.Fatal(err)
	}

	purged, err := st.PurgeExpiredSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Fatalf("purged %d sessions, want 1", purged)
	}
	if _, _, err := st.SessionByToken(ctx, live); err != nil {
		t.Errorf("the live session was purged: %v", err)
	}

	again, err := st.PurgeExpiredSessions(ctx)
	if err != nil || again != 0 {
		t.Errorf("a second purge removed %d (%v)", again, err)
	}
}

// --- audit trail -----------------------------------------------------------

func TestDestructiveMarksTheActionsThatCanLoseData(t *testing.T) {
	destructive := []string{
		AuditDeletionEnabled, AuditConfigDeleted, AuditSourceDeleted,
		AuditDestDeleted, AuditAdminDeleted, AuditBundleImported,
	}
	harmless := []string{
		AuditSignedIn, AuditSignInFailed, AuditPasswordChanged, AuditConfigSaved,
		AuditConfigEnabled, AuditConfigDisabled, AuditDeletionDisabled, AuditRunStarted,
		AuditArchiveDownloaded, AuditBundleExported, AuditSettingsSaved, "",
	}
	for _, action := range destructive {
		if !(AuditEvent{Action: action}).Destructive() {
			t.Errorf("%q can lose data but is not marked destructive", action)
		}
	}
	for _, action := range harmless {
		if (AuditEvent{Action: action}).Destructive() {
			t.Errorf("%q is marked destructive", action)
		}
	}
}

func TestRecordAuditFillsInIdentityAndTimeAndCapsTheDetail(t *testing.T) {
	st, ctx := testStore(t)

	before := time.Now().UTC().Add(-time.Second)
	long := strings.Repeat("é", 800) // 1600 bytes: the cap must cut on a rune boundary
	if err := st.RecordAudit(ctx, AuditEvent{Actor: "ops@example.com", ActorIP: "10.0.0.1",
		Action: AuditConfigSaved, TargetKind: "config", Target: "orders", Detail: long}); err != nil {
		t.Fatal(err)
	}

	events, total, err := st.AuditEvents(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("recorded %d events", total)
	}
	event := events[0]
	if event.ID == "" {
		t.Error("the event was not given an ID")
	}
	if event.At.Before(before) || event.At.After(time.Now().Add(time.Second)) {
		t.Errorf("the event was stamped %s, not now", event.At)
	}
	if event.Actor != "ops@example.com" || event.ActorIP != "10.0.0.1" || event.Target != "orders" {
		t.Errorf("event fields did not round trip: %+v", event)
	}
	if len(event.Detail) > 1000+len("…") || !utf8.ValidString(event.Detail) || !strings.HasSuffix(event.Detail, "…") {
		t.Errorf("detail was not capped cleanly: %d bytes, valid=%v", len(event.Detail), utf8.ValidString(event.Detail))
	}

	// A caller-supplied ID and time are kept, not replaced.
	at := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	if err := st.RecordAudit(ctx, AuditEvent{ID: "fixed-id", At: at, Action: AuditSignedIn}); err != nil {
		t.Fatal(err)
	}
	events, _, err = st.AuditEvents(ctx, AuditQuery{Action: AuditSignedIn})
	if err != nil || len(events) != 1 {
		t.Fatalf("lookup by action: %v %v", events, err)
	}
	if events[0].ID != "fixed-id" || !events[0].At.Equal(at) {
		t.Errorf("caller-supplied identity was overwritten: %s at %s", events[0].ID, events[0].At)
	}

	if err := st.RecordAudit(ctx, AuditEvent{ID: "fixed-id", Action: AuditSignedIn}); err == nil {
		t.Error("a duplicate event ID was accepted")
	}
}

func TestAuditEventsFilterByEveryFieldAndCountTheWholeMatch(t *testing.T) {
	st, ctx := testStore(t)
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	events := []AuditEvent{
		{Actor: "amy", Action: AuditConfigSaved, TargetKind: "config", Target: "orders"},
		{Actor: "amy", Action: AuditDeletionEnabled, TargetKind: "config", Target: "orders"},
		{Actor: "bob", Action: AuditConfigSaved, TargetKind: "config", Target: "events"},
		{Actor: "bob", Action: AuditSourceDeleted, TargetKind: "mongo_source", Target: "prod"},
		{Actor: "amy", Action: AuditSignedIn},
	}
	for i, event := range events {
		event.At = base.Add(time.Duration(i) * time.Minute)
		if err := st.RecordAudit(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name  string
		query AuditQuery
		want  int64
	}{
		{"everything", AuditQuery{}, 5},
		{"by action", AuditQuery{Action: AuditConfigSaved}, 2},
		{"by target kind", AuditQuery{TargetKind: "config"}, 3},
		{"by target", AuditQuery{Target: "orders"}, 2},
		{"by actor", AuditQuery{Actor: "bob"}, 2},
		{"combined", AuditQuery{Actor: "amy", Target: "orders", Action: AuditDeletionEnabled}, 1},
		{"no match", AuditQuery{Actor: "carol"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, total, err := st.AuditEvents(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if total != tc.want || int64(len(found)) != tc.want {
				t.Fatalf("matched %d (listed %d), want %d", total, len(found), tc.want)
			}
			for _, event := range found {
				if (tc.query.Actor != "" && event.Actor != tc.query.Actor) ||
					(tc.query.Action != "" && event.Action != tc.query.Action) ||
					(tc.query.Target != "" && event.Target != tc.query.Target) ||
					(tc.query.TargetKind != "" && event.TargetKind != tc.query.TargetKind) {
					t.Errorf("event %+v does not match the query %+v", event, tc.query)
				}
			}
		})
	}

	// The total is the whole match, not the page, so the UI can paginate.
	page, total, err := st.AuditEvents(ctx, AuditQuery{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(page) != 2 {
		t.Fatalf("page of 2 at offset 2: listed %d, total %d", len(page), total)
	}
	// Newest first: offset 2 skips the two latest, leaving the events recorded
	// at +2m and +1m.
	if !page[0].At.Equal(base.Add(2*time.Minute)) || !page[1].At.Equal(base.Add(time.Minute)) {
		t.Errorf("page holds events at %s and %s", page[0].At, page[1].At)
	}
}

func TestAuditEventsDefaultToAPageOfFifty(t *testing.T) {
	st, ctx := testStore(t)
	for i := range 55 {
		if err := st.RecordAudit(ctx, AuditEvent{Action: AuditSignedIn, Detail: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	found, total, err := st.AuditEvents(ctx, AuditQuery{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if total != 55 || len(found) != 50 {
		t.Fatalf("listed %d of %d, want 50 of 55", len(found), total)
	}
}

// The audit trail is read newest first. Sub-second offsets are exactly where
// the old variable-width timestamp layout sorted wrongly, and events recorded
// in the same instant must still come back in a stable, newest-first order.
func TestAuditEventsComeBackNewestFirstEvenWithinOneSecond(t *testing.T) {
	st, ctx := testStore(t)
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	// Recorded out of order, so insertion order cannot pass for time order.
	offsets := []time.Duration{100 * time.Millisecond, 0, time.Second, 5 * time.Millisecond, 999 * time.Millisecond}
	for _, offset := range offsets {
		if err := st.RecordAudit(ctx, AuditEvent{At: base.Add(offset), Action: AuditConfigSaved,
			Detail: offset.String()}); err != nil {
			t.Fatal(err)
		}
	}
	// Two events in the same microsecond: the later insert is the newer one.
	tie := base.Add(2 * time.Second)
	for _, detail := range []string{"tie-first", "tie-second"} {
		if err := st.RecordAudit(ctx, AuditEvent{At: tie, Action: AuditConfigSaved, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}

	found, _, err := st.AuditEvents(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, event := range found {
		order = append(order, event.Detail)
	}
	want := "tie-second,tie-first,1s,999ms,100ms,5ms,0s"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("audit order is %s, want %s", got, want)
	}
}

func TestPurgeAuditBeforeDropsOnlyOlderEvents(t *testing.T) {
	st, ctx := testStore(t)
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	for _, at := range []time.Time{
		cutoff.Add(-365 * 24 * time.Hour),
		cutoff.Add(-time.Microsecond),
		cutoff, // exactly on the cutoff is kept: "before" is strict
		cutoff.Add(time.Hour),
	} {
		if err := st.RecordAudit(ctx, AuditEvent{At: at, Action: AuditSignedIn}); err != nil {
			t.Fatal(err)
		}
	}

	// A cutoff expressed in another zone means the same instant.
	kolkata := time.FixedZone("IST", 5*3600+1800)
	purged, err := st.PurgeAuditBefore(ctx, cutoff.In(kolkata))
	if err != nil {
		t.Fatal(err)
	}
	if purged != 2 {
		t.Fatalf("purged %d events, want 2", purged)
	}

	left, total, err := st.AuditEvents(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("%d events survived, want 2", total)
	}
	for _, event := range left {
		if event.At.Before(cutoff) {
			t.Errorf("an event from %s survived the purge", event.At)
		}
	}
}
