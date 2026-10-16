package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/unifers/feather/internal/store"
)

// --- the routing table, read from the source ------------------------------

// route is one registration in Server.Handler.
type route struct {
	method  string
	pattern string // the path half of the pattern, wildcards intact
	guarded bool
}

// registeredRoutes reads Server.Handler's registrations out of server.go.
//
// ServeMux cannot be asked what it holds, and a hand-maintained list is
// exactly what goes stale: the route added next month is the one that forgot
// s.guard. Reading the source means a new route is covered by these tests the
// moment it exists.
func registeredRoutes(t *testing.T) []route {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Identifiers that appear in patterns, resolved to their compiled values.
	constants := map[string]string{"gdriveCallbackPath": gdriveCallbackPath}

	var literal func(ast.Expr) string
	literal = func(expr ast.Expr) string {
		switch e := expr.(type) {
		case *ast.BasicLit:
			value, err := strconv.Unquote(e.Value)
			if err != nil {
				t.Fatal(err)
			}
			return value
		case *ast.Ident:
			value, ok := constants[e.Name]
			if !ok {
				t.Fatalf("route pattern uses %s; add it to registeredRoutes", e.Name)
			}
			return value
		case *ast.BinaryExpr:
			return literal(e.X) + literal(e.Y)
		}
		t.Fatalf("cannot read a route pattern from %T", expr)
		return ""
	}

	var routes []route
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (selector.Sel.Name != "HandleFunc" && selector.Sel.Name != "Handle") {
			return true
		}
		if receiver, ok := selector.X.(*ast.Ident); !ok || receiver.Name != "mux" {
			return true
		}

		method, path, found := strings.Cut(literal(call.Args[0]), " ")
		if !found {
			t.Fatalf("route %q has no method; every route should name one", path)
		}
		var handler bytes.Buffer
		if err := printer.Fprint(&handler, fset, call.Args[1]); err != nil {
			t.Fatal(err)
		}
		routes = append(routes, route{
			method: method, pattern: path,
			guarded: strings.Contains(handler.String(), "s.guard("),
		})
		return true
	})

	if len(routes) < 40 {
		t.Fatalf("found only %d routes in server.go; the parser has lost track of Handler", len(routes))
	}
	return routes
}

// concretePath fills a pattern's wildcards with IDs that exist, so a request
// that got past the guard would have something real to act on.
func concretePath(pattern string, ids map[string]string) string {
	path := strings.TrimSuffix(pattern, "{$}")
	if !strings.Contains(path, "{id}") {
		return path
	}
	id := "00000000-0000-0000-0000-000000000000"
	for prefix, value := range ids {
		if strings.HasPrefix(path, prefix) {
			id = value
		}
	}
	return strings.ReplaceAll(path, "{id}", id)
}

// databaseState is every row of every table, so a test can say "nothing at
// all changed" without enumerating what might have.
func databaseState(t *testing.T, server *Server) string {
	t.Helper()
	ctx := context.Background()
	db := server.app.Store.DB()

	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()

	var out strings.Builder
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, `SELECT * FROM "`+table+`" ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&out, "%s %v\n", table, values)
		}
		rows.Close()
	}
	return out.String()
}

// authzFixture is a signed-in installation with something in every table worth
// protecting, and a second administrator whose session is not ours.
type authzFixture struct {
	server       *Server
	session      string
	otherSession string
	ids          map[string]string
}

func newAuthzFixture(t *testing.T) authzFixture {
	t.Helper()
	ctx := context.Background()

	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	sourceID, destinationID, jobID := seedData(t, server)

	other, err := server.app.Store.CreateAdmin(ctx, "second@example.com", "another-long-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	otherSession, _, err := server.app.Store.CreateSession(ctx, other.ID, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	return authzFixture{
		server: server, session: session, otherSession: otherSession,
		ids: map[string]string{
			"/mongo/":           sourceID,
			"/destinations/":    destinationID,
			"/configs/":         jobID,
			"/settings/admins/": other.ID,
		},
	}
}

func send(server *Server, method, path string, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var request *http.Request
	if reader != nil {
		request = httptest.NewRequest(method, path, reader)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		request = httptest.NewRequest(method, path, nil)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

var csrfInput = regexp.MustCompile(`name="csrf" value="[^"]*"`)

func sessionCookieFor(token string) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: token}
}

// --- authentication -------------------------------------------------------

// The public surface is short and deliberate. A route added without s.guard
// fails here rather than shipping open.
func TestOnlyTheIntendedRoutesAreReachableWithoutASession(t *testing.T) {
	public := map[string]bool{
		"GET /healthz": true, "GET /livez": true, "GET /readyz": true, "GET /metrics": true,
		"GET /static/": true,
		"GET /setup":   true, "POST /setup": true,
		"GET /login": true, "POST /login": true,
		"POST /logout": true,
	}

	seen := map[string]bool{}
	for _, r := range registeredRoutes(t) {
		key := r.method + " " + r.pattern
		seen[key] = true
		if !r.guarded && !public[key] {
			t.Errorf("%s is reachable without signing in", key)
		}
		if r.guarded && public[key] {
			t.Errorf("%s is guarded but listed as public; update this test", key)
		}
	}
	for key := range public {
		if !seen[key] {
			t.Errorf("%s is listed as public but no longer registered", key)
		}
	}
}

// Every guarded route, asked without a session or with one that is not real,
// sends the visitor to sign in and shows them nothing on the way.
func TestEveryGuardedRouteTurnsAwayAVisitorWithoutALiveSession(t *testing.T) {
	f := newAuthzFixture(t)
	before := databaseState(t, f.server)

	cookies := map[string][]*http.Cookie{
		"no cookie":       nil,
		"a forged cookie": {sessionCookieFor("forged-session-token")},
	}
	// Strings that are only in the seeded data, and must not reach someone
	// who is not signed in.
	secrets := []string{"orders-archive", "hunter2", "very-secret-key", "ops@example.com", "AKIA"}

	for _, r := range registeredRoutes(t) {
		if !r.guarded {
			continue
		}
		path := concretePath(r.pattern, f.ids)
		for name, jar := range cookies {
			t.Run(r.method+" "+r.pattern+"/"+name, func(t *testing.T) {
				body := ""
				if r.method == http.MethodPost {
					body = url.Values{"csrf": {"anything"}}.Encode()
				}
				response := send(f.server, r.method, path, body, jar...)

				if response.Code != http.StatusSeeOther {
					t.Fatalf("status %d, want a redirect to sign in", response.Code)
				}
				location, err := url.Parse(response.Header().Get("Location"))
				if err != nil || location.Path != "/login" {
					t.Fatalf("redirected to %q, want /login", response.Header().Get("Location"))
				}
				for _, secret := range secrets {
					if strings.Contains(response.Body.String(), secret) {
						t.Errorf("the response leaked %q", secret)
					}
				}
			})
		}
	}

	if after := databaseState(t, f.server); after != before {
		t.Error("requests without a session changed the database")
	}
}

// A cookie is only as good as the live, unexpired session row it names.
func TestSessionCookiesThatDoNotNameALiveSessionAreRefused(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		cookie func(t *testing.T, server *Server) string
	}{
		{"random bytes", func(*testing.T, *Server) string {
			return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
		}},
		{"a real token with one character changed", func(t *testing.T, server *Server) string {
			token := signIn(t, server)
			last := token[len(token)-1]
			replacement := byte('A')
			if last == 'A' {
				replacement = 'B'
			}
			return token[:len(token)-1] + string(replacement)
		}},
		{"a real token with something appended", func(t *testing.T, server *Server) string {
			return signIn(t, server) + "x"
		}},
		{"the hash the database stores instead of the token", func(t *testing.T, server *Server) string {
			signIn(t, server)
			var hash string
			if err := server.app.Store.DB().QueryRowContext(ctx,
				`SELECT token_hash FROM admin_sessions LIMIT 1`).Scan(&hash); err != nil {
				t.Fatal(err)
			}
			return hash
		}},
		{"an expired session", func(t *testing.T, server *Server) string {
			token := signIn(t, server)
			if _, err := server.app.Store.DB().ExecContext(ctx, `UPDATE admin_sessions SET expires_at = ?`,
				time.Now().Add(-time.Minute).UTC().Format("2006-01-02T15:04:05.000000Z07:00")); err != nil {
				t.Fatal(err)
			}
			return token
		}},
		{"a session whose administrator was removed behind its back", func(t *testing.T, server *Server) string {
			// DeleteAdmin takes the sessions with it; this is the row it would
			// leave if it ever stopped, and the guard must not depend on it.
			token := signIn(t, server)
			if _, err := server.app.Store.DB().ExecContext(ctx, `DELETE FROM admin_users`); err != nil {
				t.Fatal(err)
			}
			if _, err := server.app.Store.CreateAdmin(ctx, "new@example.com", "another-long-passphrase"); err != nil {
				t.Fatal(err)
			}
			return token
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			token := tc.cookie(t, server)

			response := get(t, server, "/configs", token)
			if response.Code != http.StatusSeeOther ||
				!strings.HasPrefix(response.Header().Get("Location"), "/login") {
				t.Fatalf("status %d to %q, want a redirect to sign in",
					response.Code, response.Header().Get("Location"))
			}
			cleared := false
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == sessionCookie && cookie.MaxAge < 0 {
					cleared = true
				}
			}
			if !cleared {
				t.Error("the dead session cookie was not cleared")
			}
		})
	}
}

// Session fixation: whatever session cookie the browser arrives with — one an
// attacker planted, or a real one — signing in must issue a new token.
func TestSigningInNeverAdoptsTheSessionTheBrowserArrivedWith(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	resetLoginThrottle(t, "ops@example.com")
	ctx := context.Background()

	planted := signIn(t, server)
	plantedSession, _, err := server.app.Store.SessionByToken(ctx, planted)
	if err != nil {
		t.Fatal(err)
	}

	for name, value := range map[string]string{"an invented value": "attacker-chosen-value", "a live session": planted} {
		t.Run(name, func(t *testing.T) {
			page := send(server, http.MethodGet, "/login", "")
			var csrf *http.Cookie
			for _, cookie := range page.Result().Cookies() {
				if cookie.Name == csrfCookie {
					csrf = cookie
				}
			}
			if csrf == nil {
				t.Fatal("the login page issued no CSRF cookie")
			}

			form := url.Values{"email": {"ops@example.com"}, "password": {"correct-horse-battery"}, "csrf": {csrf.Value}}
			response := send(server, http.MethodPost, "/login", form.Encode(),
				&http.Cookie{Name: csrfCookie, Value: csrf.Value}, sessionCookieFor(value))

			issued := sessionValue(response)
			if issued == "" {
				t.Fatalf("signing in did not issue a session (status %d)", response.Code)
			}
			if issued == value {
				t.Fatal("signing in kept the session token the browser arrived with")
			}
			fresh, _, err := server.app.Store.SessionByToken(ctx, issued)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.CSRFToken == plantedSession.CSRFToken {
				t.Error("the new session shares its CSRF token with an older one")
			}
		})
	}
}

// Session and CSRF tokens are 256 random bits each, and the database only
// ever holds a hash of the session token — a copy of the database file is not
// a set of live cookies.
func TestSessionTokensAreRandomAndStoredOnlyAsHashes(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	ctx := context.Background()

	seen := map[string]bool{}
	for range 5 {
		token := signIn(t, server)
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(raw) < 32 {
			t.Fatalf("session token %q is not 32 random bytes", token)
		}
		session, _, err := server.app.Store.SessionByToken(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if session.CSRFToken == token {
			t.Fatal("the CSRF token is the session token")
		}
		for _, value := range []string{token, session.CSRFToken} {
			if seen[value] {
				t.Fatalf("token %q was issued twice", value)
			}
			seen[value] = true
		}
		if strings.Contains(databaseState(t, server), token) {
			t.Fatal("the session token is stored in the database as-is")
		}
	}
}

// --- CSRF -----------------------------------------------------------------

// Every state-changing route refuses a request whose token is not the one
// bound to the session making it. The routes come from server.go, so a POST
// added later is covered without anyone remembering to add it here.
func TestEveryPostRouteRefusesATokenThatIsNotThisSessions(t *testing.T) {
	f := newAuthzFixture(t)
	mine := csrfFor(t, f.server, f.session)
	theirs := csrfFor(t, f.server, f.otherSession)

	cases := []struct {
		name   string
		query  string
		form   url.Values
		header string
	}{
		{"no token", "", url.Values{}, ""},
		{"an empty token", "", url.Values{"csrf": {""}}, ""},
		{"a forged token", "", url.Values{"csrf": {"forged"}}, ""},
		{"another administrator's token", "", url.Values{"csrf": {theirs}}, ""},
		{"this session's token, truncated", "", url.Values{"csrf": {mine[:len(mine)-1]}}, ""},
		{"this session's token, extended", "", url.Values{"csrf": {mine + "A"}}, ""},
		{"this session's token in upper case", "", url.Values{"csrf": {strings.ToUpper(mine)}}, ""},
		// The query string ends up in logs, history and Referer headers; a
		// token accepted from there would be one that leaks.
		{"this session's token in the URL rather than the body", "?csrf=" + url.QueryEscape(mine), url.Values{}, ""},
		{"a forged token in the header", "", url.Values{}, "forged"},
		{"another administrator's token in the header", "", url.Values{}, theirs},
	}

	before := databaseState(t, f.server)
	for _, r := range registeredRoutes(t) {
		if r.method != http.MethodPost || !r.guarded {
			continue
		}
		path := concretePath(r.pattern, f.ids)
		for _, tc := range cases {
			t.Run(r.pattern+"/"+tc.name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, path+tc.query, strings.NewReader(tc.form.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if tc.header != "" {
					request.Header.Set("X-CSRF-Token", tc.header)
				}
				request.AddCookie(sessionCookieFor(f.session))
				response := httptest.NewRecorder()
				f.server.Handler().ServeHTTP(response, request)

				if response.Code != http.StatusForbidden {
					t.Errorf("status %d, want 403", response.Code)
				}
				if response.Header().Get("Content-Disposition") != "" {
					t.Error("the refused request was answered with a download")
				}
			})
		}
	}

	if after := databaseState(t, f.server); after != before {
		t.Error("a POST with a bad CSRF token changed the database")
	}
	// Including the password change, which would have signed both out.
	for _, token := range []string{f.session, f.otherSession} {
		if _, _, err := f.server.app.Store.SessionByToken(context.Background(), token); err != nil {
			t.Error("a session was ended by a request with a bad CSRF token")
		}
	}
}

// The two forms that exist before a session — setup and sign-in — use a
// double-submit token: the cookie and the field must both be present and
// agree.
func TestPreSessionFormsNeedTheCookieAndTheFieldToAgree(t *testing.T) {
	cases := []struct {
		name   string
		cookie string
		field  string
	}{
		{"cookie only", "token-one", ""},
		{"field only", "", "token-one"},
		{"cookie and field disagree", "token-one", "token-two"},
		{"both empty", "", ""},
	}

	t.Run("setup", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				server := testServer(t)
				form := url.Values{
					"email": {"ops@example.com"}, "password": {"correct-horse-battery"},
					"confirm": {"correct-horse-battery"}, "csrf": {tc.field},
				}
				var jar []*http.Cookie
				if tc.cookie != "" {
					jar = append(jar, &http.Cookie{Name: csrfCookie, Value: tc.cookie})
				}
				response := send(server, http.MethodPost, "/setup", form.Encode(), jar...)
				if sessionValue(response) != "" {
					t.Error("setup signed someone in")
				}
				if needsSetup, _ := server.app.Store.NeedsSetup(context.Background()); !needsSetup {
					t.Error("setup created an administrator")
				}
			})
		}
	})

	t.Run("login", func(t *testing.T) {
		server := testServer(t)
		createAdmin(t, server)
		resetLoginThrottle(t, "ops@example.com")
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				form := url.Values{"email": {"ops@example.com"}, "password": {"correct-horse-battery"}, "csrf": {tc.field}}
				var jar []*http.Cookie
				if tc.cookie != "" {
					jar = append(jar, &http.Cookie{Name: csrfCookie, Value: tc.cookie})
				}
				response := send(server, http.MethodPost, "/login", form.Encode(), jar...)
				if sessionValue(response) != "" {
					t.Error("a sign-in without a matching token started a session")
				}
			})
		}
	})
}

// The pre-session token cookie is no use to script, and is not sent on
// cross-site posts. The session cookie from setup carries the same flags as
// the one from sign-in.
func TestPreSessionAndSetupCookiesAreHttpOnlyAndSameSite(t *testing.T) {
	server := testServer(t)

	var csrf *http.Cookie
	for _, cookie := range get(t, server, "/setup", "").Result().Cookies() {
		if cookie.Name == csrfCookie {
			csrf = cookie
		}
	}
	if csrf == nil {
		t.Fatal("the setup page issued no CSRF cookie")
	}
	if !csrf.HttpOnly || csrf.SameSite != http.SameSiteLaxMode || csrf.Path != "/" {
		t.Errorf("CSRF cookie flags: %+v", csrf)
	}

	response := postWithPreSessionCSRF(t, server, "/setup", url.Values{
		"email": {"ops@example.com"}, "password": {"correct-horse-battery"}, "confirm": {"correct-horse-battery"},
	})
	found := false
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name != sessionCookie {
			continue
		}
		found = true
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
			t.Errorf("session cookie flags after setup: %+v", cookie)
		}
	}
	if !found {
		t.Fatal("setup set no session cookie")
	}
}

// --- method confusion -----------------------------------------------------

// A link, an <img> or a prefetch can make a browser send GET or HEAD with its
// cookies attached. None of them may change anything — signing out included.
func TestGetAndHeadNeverReachAStateChangingHandler(t *testing.T) {
	f := newAuthzFixture(t)

	getPatterns := map[string]bool{}
	routes := registeredRoutes(t)
	for _, r := range routes {
		if r.method == http.MethodGet {
			getPatterns[r.pattern] = true
		}
	}

	before := databaseState(t, f.server)
	for _, r := range routes {
		if r.method != http.MethodPost {
			continue
		}
		path := concretePath(r.pattern, f.ids)
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+r.pattern, func(t *testing.T) {
				response := send(f.server, method, path, "", sessionCookieFor(f.session))
				// A path that is also a page (a form, a list) renders the
				// page;
				// one that is only an action has no GET at all, or falls to a
				// wildcard page that does not exist (/imports/{id} for
				// /imports/upload).
				if !getPatterns[r.pattern] && response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusNotFound {
					t.Errorf("status %d, want 405 or 404", response.Code)
				}
				if response.Header().Get("Content-Disposition") != "" {
					t.Error("a GET produced a download meant for a POST")
				}
			})
		}
	}

	if after := databaseState(t, f.server); after != before {
		t.Error("a GET or HEAD request changed the database")
	}
	if _, _, err := f.server.app.Store.SessionByToken(context.Background(), f.session); err != nil {
		t.Error("a GET to /logout ended the session")
	}
}

// Cross-origin script could set the X-CSRF-Token header only if a preflight
// were granted. Nothing in the panel grants one.
func TestNoResponseGrantsCrossOriginAccess(t *testing.T) {
	f := newAuthzFixture(t)

	for _, r := range registeredRoutes(t) {
		path := concretePath(r.pattern, f.ids)
		for _, method := range []string{http.MethodOptions, r.method} {
			request := httptest.NewRequest(method, path, nil)
			request.Header.Set("Origin", "https://evil.example")
			request.Header.Set("Access-Control-Request-Method", http.MethodPost)
			request.Header.Set("Access-Control-Request-Headers", "X-CSRF-Token")
			request.AddCookie(sessionCookieFor(f.session))
			response := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(response, request)

			for _, header := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Headers"} {
				if value := response.Header().Get(header); value != "" {
					t.Errorf("%s %s answered %s: %s", method, path, header, value)
				}
			}
		}
	}
}

// --- accounts and passwords -----------------------------------------------

// /setup disappears once an administrator exists — for a POST as much as for
// the GET, or anyone could append an administrator of their own.
func TestSetupCannotBeReplayedToAddAnAdministrator(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)

	response := send(server, http.MethodPost, "/setup", url.Values{
		"email": {"intruder@example.com"}, "password": {"correct-horse-battery"},
		"confirm": {"correct-horse-battery"}, "csrf": {"t"},
	}.Encode(), &http.Cookie{Name: csrfCookie, Value: "t"})

	if sessionValue(response) != "" {
		t.Error("a replayed setup signed someone in")
	}
	if _, err := server.app.Store.AdminByEmail(context.Background(), "intruder@example.com"); err == nil {
		t.Fatal("a replayed setup created an administrator")
	}
}

// The login page must not say which half of the pair was wrong, by its words
// or by how long it took to say them.
func TestSignInDoesNotRevealWhetherAnAccountExists(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	resetLoginThrottle(t, "ops@example.com", "nobody@example.com")

	render := func(email string) (int, string) {
		response := postWithPreSessionCSRF(t, server, "/login", url.Values{
			"email": {email}, "password": {"wrong-password-entirely"},
		})
		if sessionValue(response) != "" {
			t.Fatalf("%s: a failed sign-in set a session", email)
		}
		body := strings.ReplaceAll(response.Body.String(), email, "EMAIL")
		// The per-form token differs between the two requests; nothing else
		// should.
		return response.Code, csrfInput.ReplaceAllString(body, `name="csrf" value="TOKEN"`)
	}

	knownCode, known := render("ops@example.com")
	unknownCode, unknown := render("nobody@example.com")
	if knownCode != unknownCode {
		t.Errorf("status %d for a wrong password, %d for an unknown account", knownCode, unknownCode)
	}
	if !strings.Contains(known, html.EscapeString(store.ErrBadCredentials.Error())) {
		t.Error("the failure message is not the generic one")
	}
	if known != unknown {
		t.Error("the page for an unknown account differs from the page for a wrong password")
	}

	// Timing: an unknown account still pays for a bcrypt comparison. Measured
	// against the store directly, so the throttle is not involved; the bound
	// is loose enough to survive a busy machine and tight enough to catch a
	// lookup that returns early.
	ctx := context.Background()
	fastest := func(email string) time.Duration {
		best := time.Hour
		for range 3 {
			started := time.Now()
			_, _ = server.app.Store.Authenticate(ctx, email, "wrong-password-entirely")
			best = min(best, time.Since(started))
		}
		return best
	}
	wrongPassword, unknownAccount := fastest("ops@example.com"), fastest("nobody@example.com")
	if unknownAccount < wrongPassword/3 {
		t.Errorf("an unknown account is rejected in %v, a wrong password in %v", unknownAccount, wrongPassword)
	}
}

// The password rules hold on every path that sets one, not just the first.
func TestPasswordRulesHoldWhereverAPasswordIsChosen(t *testing.T) {
	short := strings.Repeat("a", store.MinPasswordLength-1)
	// bcrypt ignores everything past 72 bytes, so a longer one is weaker than
	// it looks.
	tooLong := strings.Repeat("é", 37)
	ctx := context.Background()

	t.Run("setup", func(t *testing.T) {
		for name, password := range map[string]string{"too short": short, "past bcrypt's limit": tooLong} {
			t.Run(name, func(t *testing.T) {
				server := testServer(t)
				postWithPreSessionCSRF(t, server, "/setup", url.Values{
					"email": {"ops@example.com"}, "password": {password}, "confirm": {password},
				})
				if needsSetup, _ := server.app.Store.NeedsSetup(ctx); !needsSetup {
					t.Error("setup accepted the password")
				}
			})
		}
	})

	t.Run("adding an administrator", func(t *testing.T) {
		server := testServer(t)
		createAdmin(t, server)
		session := signIn(t, server)
		post(t, server, "/settings/admins", url.Values{"email": {"x@example.com"}, "password": {tooLong}}, session)
		if _, err := server.app.Store.AdminByEmail(ctx, "x@example.com"); err == nil {
			t.Error("an administrator was added with a password past bcrypt's limit")
		}
	})

	t.Run("changing a password", func(t *testing.T) {
		for name, password := range map[string]string{
			"too short": short, "past bcrypt's limit": tooLong, "the same as before": "correct-horse-battery",
		} {
			t.Run(name, func(t *testing.T) {
				server := testServer(t)
				createAdmin(t, server)
				session := signIn(t, server)
				response := post(t, server, "/settings/password", url.Values{
					"currentPassword": {"correct-horse-battery"},
					"newPassword":     {password}, "confirmPassword": {password},
				}, session)
				assertRedirect(t, response, "/settings")
				if _, err := server.app.Store.Authenticate(ctx, "ops@example.com", "correct-horse-battery"); err != nil {
					t.Error("the password was changed")
				}
				if _, _, err := server.app.Store.SessionByToken(ctx, session); err != nil {
					t.Error("a refused change still signed the session out")
				}
			})
		}
	})
}

// The master key is the one download a hijacked session must not be able to
// take: it needs the password again, and the other administrator's password
// is not this one's.
func TestTheMasterKeyNeedsThisAdministratorsOwnPassword(t *testing.T) {
	f := newAuthzFixture(t)

	for name, password := range map[string]string{
		"no password":                      "",
		"a wrong password":                 "wrong-password-entirely",
		"another administrator's password": "another-long-passphrase",
	} {
		t.Run(name, func(t *testing.T) {
			response := post(t, f.server, "/settings/master-key", url.Values{"password": {password}}, f.session)
			assertRedirect(t, response, "/settings")
			if response.Header().Get("Content-Disposition") != "" {
				t.Fatal("the master key was downloaded")
			}
		})
	}
	assertNotAudited(t, f.server, store.AuditMasterKeyDownloaded)
}
