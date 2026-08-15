package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/store"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// csrfFor returns the token bound to a session, which is what a real form
// carries.
func csrfFor(t *testing.T, server *Server, token string) string {
	t.Helper()
	session, _, err := server.app.Store.SessionByToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return session.CSRFToken
}

// Changing a password is what someone does when they think a session of theirs
// was stolen. The doc comment on ChangePassword said it dropped every other
// session; the code did not, so the thief stayed signed in for up to twelve
// hours.
func TestChangingAPasswordSignsOutEverySession(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)

	// Two sessions for the same account: the one changing the password, and
	// the one that is the whole reason for changing it.
	changing := signIn(t, server)
	other := signIn(t, server)

	form := url.Values{
		"csrf":            {csrfFor(t, server, changing)},
		"currentPassword": {"correct-horse-battery"},
		"newPassword":     {"a-completely-different-one"},
		"confirmPassword": {"a-completely-different-one"},
	}
	request := httptest.NewRequest(http.MethodPost, "/settings/password", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: changing})

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("the password change returned %d: %s", response.Code, response.Body)
	}

	for name, token := range map[string]string{"the other session": other, "the changing session": changing} {
		if _, _, err := server.app.Store.SessionByToken(context.Background(), token); err == nil {
			t.Errorf("%s is still valid after the password changed", name)
		}
	}
}

// An open redirect on the login page turns it into a phishing hop: a link that
// really is feather's, that lands somewhere else.
func TestSafeNextRefusesToLeaveTheSite(t *testing.T) {
	offSite := []string{
		"//evil.com",
		`/\evil.com`, // passed the old prefix checks; browsers read it as //
		`/\/evil.com`,
		"https://evil.com",
		"http://evil.com/path",
		"javascript:alert(1)",
		"//evil.com/path?a=b",
		`\\evil.com`,
		"/%2F%2Fevil.com",
	}
	for _, raw := range offSite {
		if got := safeNext(raw); got != "" {
			t.Errorf("safeNext(%q) = %q, want it refused", raw, got)
		}
	}

	onSite := map[string]string{
		"/configs":               "/configs",
		"/backups?config=orders": "/backups?config=orders",
		"/runs/abc-123":          "/runs/abc-123",
	}
	for raw, want := range onSite {
		if got := safeNext(raw); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The address is used to throttle sign-ins and is written into the log and the
// session row. Believing a header any client can set made all three worthless.
func TestForwardedHeadersAreOnlyBelievedFromATrustedProxy(t *testing.T) {
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.2.3:44444"
		r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
		return r
	}

	untrusting := &Server{}
	if got := untrusting.clientIP(request()); got != "10.1.2.3" {
		t.Errorf("with no trusted proxies the address is %q, want the socket address", got)
	}

	trusting := &Server{}
	proxies, err := parseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	trusting.trustedProxies = proxies
	if got := trusting.clientIP(request()); got != "203.0.113.7" {
		t.Errorf("behind a trusted proxy the address is %q, want the forwarded client", got)
	}

	// A peer outside the trusted range is not believed, however it asks.
	outside := request()
	outside.RemoteAddr = "198.51.100.9:5555"
	if got := trusting.clientIP(outside); got != "198.51.100.9" {
		t.Errorf("an untrusted peer's header was believed: %q", got)
	}
}

func TestTrustedProxyShorthands(t *testing.T) {
	local, err := parseTrustedProxies("local")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{trustedProxies: local}

	loopback := httptest.NewRequest(http.MethodGet, "/", nil)
	loopback.RemoteAddr = "127.0.0.1:1234"
	loopback.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := server.clientIP(loopback); got != "203.0.113.7" {
		t.Errorf("a proxy on localhost was not trusted: %q", got)
	}

	if _, err := parseTrustedProxies("not-an-address"); err == nil {
		t.Error("a malformed trusted-proxy entry was accepted")
	}
}

// RFC 7239's Forwarded header quotes and brackets its values.
func TestForwardedHeaderIsParsed(t *testing.T) {
	proxies, _ := parseTrustedProxies("local")
	server := &Server{trustedProxies: proxies}

	cases := map[string]string{
		`for=192.0.2.60;proto=http;by=203.0.113.43`: "192.0.2.60",
		`for="[2001:db8::1]:443"`:                   "2001:db8::1",
		`for=192.0.2.43, for=198.51.100.17`:         "192.0.2.43",
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Forwarded", header)
		if got := server.clientIP(r); got != want {
			t.Errorf("Forwarded: %s gave %q, want %q", header, got, want)
		}
	}
}

// The throttle keyed only on the address, which is spoofable behind a proxy.
// The account key cannot be sidestepped: whoever is guessing has to name the
// account they are guessing at.
func TestRepeatedFailuresLockTheAccountNotJustTheAddress(t *testing.T) {
	th := newThrottle()
	_, account := throttleKeys("198.51.100.1", "ops@example.com")

	for range throttleAfter {
		th.fail(account)
	}
	if _, blocked := th.blocked(account); !blocked {
		t.Fatal("the account was not throttled after repeated failures")
	}

	// A different account is unaffected, so one attacker cannot lock everyone
	// else out of the panel.
	_, other := throttleKeys("198.51.100.1", "someone-else@example.com")
	if _, blocked := th.blocked(other); blocked {
		t.Error("throttling one account locked out another")
	}

	th.succeed(account)
	if _, blocked := th.blocked(account); blocked {
		t.Error("a successful sign-in did not clear the throttle")
	}
}

// Unbounded, the throttle maps grew with every spoofed address — inside the
// process that also runs the archives.
func TestThrottleDoesNotGrowWithoutLimit(t *testing.T) {
	th := newThrottle()
	for i := range throttleMaxKeys * 3 {
		th.fail("ip:198.51.100." + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26)))
	}
	if len(th.entries) > throttleMaxKeys {
		t.Errorf("the throttle holds %d keys, above its %d cap", len(th.entries), throttleMaxKeys)
	}
}

// The headers were set inside render(), so everything that was not a rendered
// page — errors, 404s, the panic handler, static assets — had none at all.
func TestSecurityHeadersAreOnEveryResponse(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)

	paths := []string{
		"/login",          // a rendered page
		"/nope",           // a 404 from the mux
		"/static/app.css", // an embedded asset
		"/healthz",        // the health endpoint
	}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)

		for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
			if response.Header().Get(header) == "" {
				t.Errorf("%s has no %s (status %d)", path, header, response.Code)
			}
		}
	}
}

// HSTS is only honest over HTTPS: promising it on a plain-HTTP deployment locks
// a browser out of a panel that has no TLS to fall back on.
func TestHSTSOnlyWhenSecure(t *testing.T) {
	server := testServer(t)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/login", nil))
	if response.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS was sent over plain HTTP")
	}

	server.secure = true
	secure := httptest.NewRecorder()
	server.Handler().ServeHTTP(secure, httptest.NewRequest(http.MethodGet, "/login", nil))
	if secure.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS was not sent when the panel is served over HTTPS")
	}
}

// Deriving this from the base URL is what makes the shipped systemd unit and
// Dockerfile correct without an extra flag nobody remembers to pass.
func TestSecureCookiesFollowTheBaseURL(t *testing.T) {
	cases := map[string]bool{
		"https://feather.example.com": true,
		"http://feather.example.com":  false,
		"":                            false,
	}
	for baseURL, want := range cases {
		server, err := New(nil, testLogger(), Options{BaseURL: baseURL})
		if err != nil {
			t.Fatal(err)
		}
		if server.secure != want {
			t.Errorf("BaseURL %q gave secure=%v, want %v", baseURL, server.secure, want)
		}
	}

	// An explicit flag still wins, for the deployment that needs the opposite.
	no := false
	server, err := New(nil, testLogger(), Options{BaseURL: "https://feather.example.com", SecureCookies: &no})
	if err != nil {
		t.Fatal(err)
	}
	if server.secure {
		t.Error("--secure-cookies=false did not override the derived value")
	}
}

// A stale tab posting an expired token used to 303 to a path with no GET
// handler, so the operator got a bare 405 and the explanation was discarded.
func TestExpiredCSRFTokenExplainsItself(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)

	form := url.Values{"csrf": {"stale"}}
	request := httptest.NewRequest(http.MethodPost, "/configs/"+jobID+"/delete", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Errorf("a bad CSRF token returned %d, want 403", response.Code)
	}
	if !strings.Contains(response.Body.String(), "expired") {
		t.Errorf("the response does not explain what happened: %s", response.Body)
	}
	if _, err := server.app.Store.JobByID(context.Background(), jobID); err != nil {
		t.Error("the config was deleted despite the bad token")
	}
}

// The one-click toggle wrote the flag without validating the config it
// produced. An empty filter plus deletion is "archive the collection, then
// empty it" — refused on save, and previously reachable in one click.
func TestDeletionToggleRefusesAConfigThatWouldEmptyTheCollection(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)

	ctx := context.Background()
	record, err := server.app.Store.JobByID(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}

	// Legal to save: an empty filter with deletion off is just "archive
	// everything", which is a backup.
	record.Config.Source.Filter = []byte(`{}`)
	record.Config.Retention.DeleteAfterArchive = false
	record.Config.Limits.MaxDocsPerRun = 0
	if err := server.app.Store.SaveJob(ctx, &record); err != nil {
		t.Fatalf("saving an empty-filter archive-only config: %v", err)
	}

	form := url.Values{"csrf": {csrfFor(t, server, session)}, "deleteAfterArchive": {"on"}}
	request := httptest.NewRequest(http.MethodPost, "/configs/"+jobID+"/deletion", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	server.Handler().ServeHTTP(httptest.NewRecorder(), request)

	after, err := server.app.Store.JobByID(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Config.Retention.DeleteAfterArchive {
		t.Fatal("one click armed a config that would archive and then empty the whole collection")
	}
	// And the config is still loadable — the old path wrote a row that Prepare
	// then rejected, which parked the config and 500'd every list page.
	if after.Broken() {
		t.Errorf("the config was left in a state it cannot be loaded from: %s", after.Problem)
	}
}

// The fingerprint recorded against every run has to describe the config that
// ran. A bare UPDATE left it describing one that no longer existed.
func TestTogglesRecomputeTheConfigFingerprint(t *testing.T) {
	server := testServer(t)
	ctx := context.Background()
	_, _, jobID := seedData(t, server)

	before, err := server.app.Store.JobByID(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.app.Store.SetJobDeleteAfterArchive(ctx, jobID, !before.Config.Retention.DeleteAfterArchive); err != nil {
		t.Fatal(err)
	}
	after, err := server.app.Store.JobByID(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Config.SHA256 == before.Config.SHA256 {
		t.Error("the deletion toggle left the fingerprint describing the previous config")
	}
}

// Every mutation should be attributable. The deletion toggle in particular
// recorded that it had changed but not who changed it.
func TestTheDeletionToggleIsAudited(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)

	form := url.Values{"csrf": {csrfFor(t, server, session)}, "deleteAfterArchive": {"on"}}
	request := httptest.NewRequest(http.MethodPost, "/configs/"+jobID+"/deletion", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	server.Handler().ServeHTTP(httptest.NewRecorder(), request)

	events, _, err := server.app.Store.AuditEvents(context.Background(),
		store.AuditQuery{Action: store.AuditDeletionEnabled})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("recorded %d audit events for enabling deletion, want 1", len(events))
	}
	if events[0].Actor != "ops@example.com" {
		t.Errorf("the event is attributed to %q", events[0].Actor)
	}
	if !events[0].Destructive() {
		t.Error("enabling deletion is not marked as destructive")
	}
}

// A stale bookmark to something that has been deleted is a 404, not a 500. The
// difference matters to anyone watching an error rate.
func TestUnknownIDsAreNotFoundRatherThanServerErrors(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	token := csrfFor(t, server, session)

	paths := []string{
		"/configs/00000000-0000-0000-0000-000000000000/deletion",
		"/configs/00000000-0000-0000-0000-000000000000/run",
		"/configs/00000000-0000-0000-0000-000000000000/delete",
		"/mongo/00000000-0000-0000-0000-000000000000/test",
		"/destinations/00000000-0000-0000-0000-000000000000/test",
	}
	for _, path := range paths {
		form := url.Values{"csrf": {token}}
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})

		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code == http.StatusInternalServerError {
			t.Errorf("%s returned 500 for an id that does not exist", path)
		}
	}
}
