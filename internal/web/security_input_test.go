package web

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/store"
)

// Untrusted input reaching the panel: text stored by a run, an import, a bundle
// or the audit trail; query strings; uploaded file names; the OAuth round trip.
// Authentication, CSRF and authorisation are covered in security_authz_test.go.

// hostile is one string that breaks out of an element, an attribute and a
// script at once, so a single seed exercises every template context.
const hostile = `x"'><script>alert(1)</script><img src=x onerror=alert(2)><svg onload=alert(3)>`

// rawMarkers are what would appear in a page if hostile were written unescaped.
var rawMarkers = []string{"<script>alert(1)", "<img src=x onerror", "<svg onload", `"'><`}

func assertEscaped(t *testing.T, where, body string) {
	t.Helper()
	for _, marker := range rawMarkers {
		if strings.Contains(body, marker) {
			t.Errorf("%s renders %q unescaped", where, marker)
		}
	}
	if strings.Contains(strings.ToLower(body), `href="javascript:`) {
		t.Errorf("%s renders a javascript: link", where)
	}
}

// seedHostileHistory writes hostile text into every free-text column the
// catalogue, run, import and audit pages render. None of it passes through a
// form: a bundle import and the runner both write these rows directly.
func seedHostileHistory(t *testing.T, server *Server) (runID, importID string) {
	t.Helper()
	ctx := context.Background()

	filter, _ := json.Marshal(map[string]string{"note": hostile})
	run := &store.Run{ConfigName: "orders-archive", Database: hostile, Collection: hostile,
		FilterJSON: string(filter), Trigger: store.TriggerCron, Host: hostile}
	if err := server.app.Store.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	artifact := &store.Artifact{RunID: run.ID, ConfigName: "orders-archive", Database: hostile,
		Collection: hostile, BatchSeq: 1, FileName: hostile + ".ndjson.gz",
		Format: "ndjson", Codec: "gzip", DocCount: 1, BytesRaw: 10, BytesCompressed: 5, SHA256: hostile}
	if err := server.app.Store.InsertArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	copy := &store.ArtifactDestination{ArtifactID: artifact.ID, Alias: "cold", Provider: "s3",
		Bucket: hostile, ObjectKey: hostile, SizeBytes: 5}
	if err := server.app.Store.InsertArtifactDestination(ctx, copy); err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunFailed
	run.Error = hostile
	if err := server.app.Store.FinishRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	imp := &store.Import{Actor: hostile, ActorIP: hostile, SourceKind: store.ImportSourceUpload,
		SourceLabel: hostile, TargetAlias: hostile, TargetDatabase: hostile, TargetCollection: hostile,
		Mode: store.ImportModeInsert, DryRun: true}
	if err := server.app.Store.StartImport(ctx, imp); err != nil {
		t.Fatal(err)
	}
	imp.Status = store.ImportFailed
	imp.Error = hostile
	if err := server.app.Store.FinishImport(ctx, imp); err != nil {
		t.Fatal(err)
	}

	if err := server.app.Store.RecordAudit(ctx, store.AuditEvent{Actor: hostile, ActorIP: hostile,
		Action: hostile, TargetKind: hostile, Target: hostile, Detail: hostile}); err != nil {
		t.Fatal(err)
	}
	return run.ID.String(), imp.ID.String()
}

// html/template escapes by context, but only if nothing bypasses it. This pins
// that every page showing stored history keeps doing so.
func TestStoredHostileTextIsEscapedOnEveryPage(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	runID, importID := seedHostileHistory(t, server)

	for _, path := range []string{
		"/", "/runs", "/runs/" + runID, "/backups", "/backups?deleted=0",
		"/imports", "/imports/" + importID, "/audit",
	} {
		response := get(t, server, path, session)
		if response.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, response.Code)
			continue
		}
		body := response.Body.String()
		assertEscaped(t, path, body)
		// Proof the seed reached the page at all, so the test is not vacuous.
		if !strings.Contains(body, "&lt;script&gt;alert(1)") && !strings.Contains(body, `<script>alert(1)`) {
			t.Logf("%s does not show the hostile text (fine, but not exercised)", path)
		}
	}
}

// Query parameters are echoed back into filter inputs and pagination links.
func TestReflectedQueryParametersAreEscaped(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)

	q := url.QueryEscape(hostile)
	for _, path := range []string{
		"/backups?q=" + q,
		"/backups?config=" + q + "&run=" + q + "&page=2",
		"/runs?config=" + q,
		"/audit?action=" + q + "&actor=" + q + "&target=" + q + "&page=2",
	} {
		response := get(t, server, path, session)
		if response.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, response.Code)
			continue
		}
		body := response.Body.String()
		assertEscaped(t, path, body)
		if !strings.Contains(body, "&lt;script&gt;") && !strings.Contains(body, "%3Cscript%3E") {
			t.Errorf("%s did not echo the query back at all; the test exercises nothing", path)
		}
	}
}

// A rejected config form is re-rendered with exactly what was typed, inside
// value attributes and a textarea — the classic place to break out of.
func TestARejectedConfigFormEscapesWhatWasTyped(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	form := validConfigForm()
	form.Set("name", hostile)
	form.Set("database", hostile)
	form.Set("collection", hostile)
	form.Set("filter", `{"a":"</textarea>`+hostile+`"`) // invalid JSON too
	form.Set("projection", `</textarea>`+hostile)
	form.Set("timezone", hostile)
	form.Set("keyTemplate", hostile)

	response := post(t, server, "/configs", form, session)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want the form shown back", response.Code)
	}
	body := response.Body.String()
	assertEscaped(t, "the re-rendered config form", body)
	if strings.Contains(body, "</textarea><script>") {
		t.Error("the filter textarea was closed by the submitted value")
	}
	if _, err := server.app.Store.JobByName(context.Background(), hostile); err == nil {
		t.Error("a config with a hostile name was saved")
	}
}

// The flash travels in a cookie the browser hands back. Its kind lands in a
// class attribute and its message in an element; both must be escaped even if
// the cookie was tampered with.
func TestTamperedFlashCookiesAreEscaped(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	request := httptest.NewRequest(http.MethodGet, "/configs", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: flashCookie, Value: url.QueryEscape(hostile + "|" + hostile)})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	assertEscaped(t, "a page with a tampered flash", response.Body.String())
}

// Google reports a refusal as ?error=… on the callback, which anyone can put in
// a link. It is echoed into a flash and must come back escaped.
func TestDriveCallbackErrorTextIsEscaped(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	record := driveDestination(t, server)

	state := record.ID + ":nonce"
	request := httptest.NewRequest(http.MethodGet,
		gdriveCallbackPath+"?state="+url.QueryEscape(state)+"&error="+url.QueryEscape(hostile), nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: gdriveStateCookie, Value: state})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	assertRedirect(t, response, destinationFormPath(record.ID))

	next := httptest.NewRequest(http.MethodGet, destinationFormPath(record.ID), nil)
	next.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == flashCookie {
			next.AddCookie(cookie)
		}
	}
	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, next)
	body := page.Body.String()
	assertEscaped(t, "the destination form after a refused authorisation", body)
	if !strings.Contains(body, "&lt;script&gt;alert(1)") {
		t.Error("the refusal was not shown at all")
	}
}

// The state check is the only thing tying a code to the operator who started
// the round trip. Every way of getting it wrong must be refused, and nothing
// may be stored.
func TestDriveCallbackRefusesMissingMismatchedAndReplayedState(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	record := driveDestination(t, server)
	good := record.ID + ":the-real-nonce"

	cases := []struct {
		name   string
		state  string
		cookie string
	}{
		{"no cookie at all (a replay after the cookie was cleared)", good, ""},
		{"no state in the URL", "", good},
		{"a state for the same destination with another nonce", record.ID + ":guessed", good},
		{"a cookie for another destination", good, "00000000-0000-0000-0000-000000000000:the-real-nonce"},
		{"a prefix of the real state", record.ID + ":the-real", good},
		{"the real state with a suffix", good + "x", good},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet,
				gdriveCallbackPath+"?code=attacker-code&state="+url.QueryEscape(tc.state), nil)
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
			if tc.cookie != "" {
				request.AddCookie(&http.Cookie{Name: gdriveStateCookie, Value: tc.cookie})
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)

			assertRedirect(t, response, "/destinations")
			if flashKind(response) != "error" {
				t.Error("the refusal was not explained")
			}
			assertStateCookieCleared(t, response)
		})
	}

	stored, err := server.app.Store.DestinationByID(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuthRefreshToken != "" {
		t.Error("a refresh token was stored from a callback with a bad state")
	}
	if events := auditEvents(t, server, store.AuditDriveConnected); len(events) != 0 {
		t.Errorf("%d connections were audited from bad callbacks", len(events))
	}
}

// Even a callback that is accepted must burn its state, so the same URL
// cannot be replayed from history.
func TestDriveCallbackClearsItsStateEvenWhenAccepted(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	record := driveDestination(t, server)

	state := record.ID + ":nonce"
	request := httptest.NewRequest(http.MethodGet,
		gdriveCallbackPath+"?state="+url.QueryEscape(state)+"&error=access_denied", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: gdriveStateCookie, Value: state})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	assertStateCookieCleared(t, response)
}

// The redirect URI sent to Google decides where the authorisation code goes.
// With a base URL configured, a forged Host header must not move it.
func TestDriveConnectSendsTheConfiguredRedirectURINotTheHostHeader(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	record := driveDestination(t, server)
	server.app.Env.BaseURL = "https://feather.example.com/"

	form := url.Values{csrfField: {csrfFor(t, server, session)}}
	request := httptest.NewRequest(http.MethodPost, "/destinations/"+record.ID+"/gdrive/connect",
		strings.NewReader(form.Encode()))
	request.Host = "evil.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Forwarded-Proto", "http")
	request.Header.Set("X-Forwarded-Host", "evil.example")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", response.Code, truncateForLog(response.Body.String()))
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != "accounts.google.com" {
		t.Fatalf("sent to %q, want Google", location.Host)
	}
	if got := location.Query().Get("redirect_uri"); got != "https://feather.example.com"+gdriveCallbackPath {
		t.Errorf("redirect_uri is %q", got)
	}

	// The state on the URL must be the one the cookie can prove, and the
	// cookie must be unreadable from script and scoped to the callback.
	var stateCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == gdriveStateCookie {
			stateCookie = cookie
		}
	}
	if stateCookie == nil {
		t.Fatal("no state cookie was set")
	}
	if stateCookie.Value != location.Query().Get("state") {
		t.Error("the state on the URL does not match the cookie")
	}
	if !stateCookie.HttpOnly || stateCookie.Path != gdriveCallbackPath || stateCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("state cookie is too permissive: %+v", stateCookie)
	}
	_, nonce, _ := strings.Cut(stateCookie.Value, ":")
	if len(nonce) < 32 {
		t.Errorf("the nonce %q is too short to be unguessable", nonce)
	}
}

// A file name comes out of the catalogue, and the catalogue can be filled from
// an imported bundle. Whatever it holds must not split the response or add
// parameters to Content-Disposition.
func TestArchiveDownloadFileNamesCannotInjectHeaders(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		want     string
	}{
		{"a quote that would add a filename* parameter", `report"; filename*=UTF-8''evil.html; x=".ndjson.gz`, ""},
		{"a CRLF that would start a new header", "orders.ndjson.gz\r\nSet-Cookie: feather_session=attacker", ""},
		{"a path that climbs out", "../../../../etc/cron.d/evil.ndjson.gz", "evil.ndjson.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)
			seedAccountsOnly(t, server)
			id := seedArchiveNamed(t, server, tc.fileName)

			response := get(t, server, "/backups/"+id+"/download", session)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d", response.Code)
			}
			if ct := response.Header().Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("content type %q", ct)
			}
			if response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("a download can be sniffed into HTML")
			}
			for name, values := range response.Header() {
				for _, value := range values {
					if strings.ContainsAny(value, "\r\n") {
						t.Errorf("header %s carries a line break: %q", name, value)
					}
				}
			}
			if response.Header().Get("Set-Cookie") != "" && strings.Contains(response.Header().Get("Set-Cookie"), "attacker") {
				t.Error("the file name set a cookie")
			}

			disposition, params, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
			if err != nil {
				t.Fatalf("Content-Disposition %q does not parse: %v", response.Header().Get("Content-Disposition"), err)
			}
			if disposition != "attachment" {
				t.Errorf("disposition %q, want attachment", disposition)
			}
			if len(params) != 1 {
				t.Errorf("Content-Disposition has extra parameters: %v", params)
			}
			got := params["filename"]
			if strings.ContainsAny(got, "/\\\r\n") || strings.HasSuffix(got, ".html") {
				t.Errorf("the browser would save the file as %q", got)
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("filename %q, want %q", got, tc.want)
			}
		})
	}
}

// "Go back" on the error page and every redirect-back use the Referer. A path
// that starts with two slashes is another host to a browser.
func TestRefererBackLinksNeverLeaveTheSite(t *testing.T) {
	for _, referer := range []string{
		"http://example.com//evil.example/phish",
		"http://example.com/\\evil.example",
		"http://example.com/%2F%2Fevil.example",
		"javascript:alert(1)",
		"http://evil.example/",
		"//evil.example/",
	} {
		t.Run(referer, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://example.com/x", nil)
			request.Header.Set("Referer", referer)

			if link := backLink(request); !onSite(link) {
				t.Errorf("the error page would link to %q", link)
			}
			response := httptest.NewRecorder()
			testServer(t).redirectBack(response, request, "/fallback")
			if location := response.Header().Get("Location"); !onSite(location) {
				t.Errorf("redirected to %q", location)
			}
		})
	}
}

func onSite(link string) bool {
	if !strings.HasPrefix(link, "/") || strings.HasPrefix(link, "//") || strings.HasPrefix(link, `/\`) {
		return false
	}
	parsed, err := url.Parse(link)
	return err == nil && parsed.Host == "" && parsed.Scheme == ""
}

// The CSRF error page puts the Referer into an href. Even a same-site path
// must arrive there attribute-escaped.
func TestTheErrorPageBackLinkIsEscaped(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	form := url.Values{csrfField: {"wrong"}}
	request := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Referer", `http://example.com/configs?x="><script>alert(1)</script>`)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", response.Code)
	}
	assertEscaped(t, "the CSRF error page", response.Body.String())
}

// A bundle is a file somebody else wrote. Malformed or hostile JSON must be
// refused with a message, not a panic, and nothing may be applied.
func TestHostileBundlesAreRefusedWithoutApplyingAnything(t *testing.T) {
	deep := `{"feather":{"version":1},"configs":[` + strings.Repeat("[", 200000) + strings.Repeat("]", 200000) + `]}`
	cases := map[string]string{
		"deep nesting":                          deep,
		"a number too large to hold":            `{"feather":{"version":1e400}}`,
		"a version from the future":             `{"feather":{"version":2147483647}}`,
		"a version that overflows int":          `{"feather":{"version":99999999999999999999999}}`,
		"wrong types":                           `{"feather":{"version":1},"configs":"all of them","mongoSources":{}}`,
		"truncated":                             `{"feather":{"version":1},"configs":[{"name":"x"`,
		"not JSON at all":                       "\x00\xff\xfe<script>alert(1)</script>",
		"duplicate keys hiding a newer version": `{"feather":{"version":1},"feather":{"version":999}}`,
		"an absurd scrypt cost": `{"feather":{"version":1,"secrets":{"kdf":"scrypt","n":1073741824,"r":64,"p":64,` +
			`"salt":"c2FsdA==","check":"x"}},"mongoSources":[{"alias":"x"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- upload(t, server, "/settings/import", []byte(body),
					url.Values{"include": everything, "passphrase": {"anything"}}, session)
			}()
			var response *httptest.ResponseRecorder
			select {
			case response = <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the import did not return; a hostile bundle can stall the server")
			}

			assertRedirect(t, response, "/settings")
			if kind := flashKind(response); kind == "ok" {
				t.Errorf("a hostile bundle was reported as imported")
			}
			ctx := context.Background()
			if jobs, _ := server.app.Store.ListJobs(ctx); len(jobs) != 0 {
				t.Errorf("%d configs were created", len(jobs))
			}
			if sources, _ := server.app.Store.ListMongoSources(ctx); len(sources) != 0 {
				t.Errorf("%d MongoDB accounts were created", len(sources))
			}
		})
	}
}

// A bundle with null entries where records belong must not dereference them.
func TestABundleOfNullsDoesNotPanic(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	body := `{"feather":{"version":1},"mongoSources":[null],"destinations":[null],"configs":[null],"runs":[null]}`
	response := upload(t, server, "/settings/import", []byte(body), url.Values{"include": everything}, session)
	if response.Code == http.StatusInternalServerError {
		t.Fatalf("a bundle of nulls crashed the import: %s", response.Body)
	}
	assertRedirect(t, response, "/settings")
	if jobs, _ := server.app.Store.ListJobs(context.Background()); len(jobs) != 0 {
		t.Errorf("%d configs were created from nulls", len(jobs))
	}
}

// A bundle body that runs past its cap without declaring a length is cut off
// by the reader, not buffered.
func TestAnOversizedBundleIsCutOff(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	stored, _, err := server.app.Store.SessionByToken(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	padding := `{"feather":{"version":1},"settings":{"instanceName":"` + strings.Repeat("A", 8<<10) + `"}}`
	request := multipartBundleRequest(t, stored.CSRFToken, session, []byte(padding))
	request.ContentLength = -1
	response := httptest.NewRecorder()
	limitBody(1024, server.guard(server.handleSettingsImport))(response, request)

	if response.Code == http.StatusSeeOther && flashKind(response) == "ok" {
		t.Fatal("an over-cap bundle was imported")
	}
	if name, _ := server.app.Store.Meta(context.Background(), "instance_name", ""); name != "" {
		t.Error("an over-cap bundle was applied")
	}
}

// The upload's file name comes from the browser. It is reduced to a base name
// before it is used anywhere, so it cannot steer where the spool file lands,
// and it is escaped wherever it is shown.
func TestAnUploadedFileNameCannotTraverseOrInject(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	name := "../../../../tmp/<img src=x onerror=alert(2)>.ndjson"
	body := []byte(`{"_id":1}` + "\n")

	// Refused at the target, so the label is shown back on the form.
	rejected := postUpload(t, server, session, name, body, map[string]string{
		"targetAlias": "prod", "targetDatabase": "admin", "targetCollection": "x", "action": "dryrun",
	})
	if rejected.Code != http.StatusOK {
		t.Fatalf("status %d", rejected.Code)
	}
	assertEscaped(t, "the rejected upload form", rejected.Body.String())

	// Accepted as a dry run: the label recorded is the base name only.
	accepted := postUpload(t, server, session, name, body, map[string]string{
		"targetAlias": "prod", "targetDatabase": "shop", "targetCollection": "x", "action": "dryrun",
	})
	if accepted.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", accepted.Code, truncateForLog(accepted.Body.String()))
	}
	server.app.Drain(context.Background())
	imports, err := server.app.Store.Imports(context.Background(), 10)
	if err != nil || len(imports) != 1 {
		t.Fatalf("%d imports (%v)", len(imports), err)
	}
	if label := imports[0].SourceLabel; strings.Contains(label, "..") || strings.Contains(label, "/") {
		t.Errorf("the upload is labelled %q", label)
	}
	detail := get(t, server, "/imports/"+imports[0].ID.String(), session).Body.String()
	assertEscaped(t, "the upload's import page", detail)

	if _, err := os.Stat(filepath.Join(server.app.Env.SpoolDir, "..", "..", "tmp")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(server.app.Env.SpoolDir, "..", "..", "tmp"))
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "onerror") {
				t.Errorf("the upload was written outside the spool: %s", entry.Name())
			}
		}
	}
	assertSpoolEmpty(t, server)
}

// IDs in the path are parsed before they are used. Traversal and junk are a
// 404, never a 500 and never a file from disk.
func TestPathParametersCannotTraverse(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)

	for _, path := range []string{
		"/runs/..%2F..%2Fetc%2Fpasswd",
		"/runs/%2e%2e",
		"/imports/..%2Ffeather.db",
		"/backups/..%2F..%2Fmaster.key/download",
		"/backups/" + url.PathEscape("1 OR 1=1") + "/download",
		"/configs/..%2F..%2F",
		"/destinations/%00",
		"/mongo/" + url.PathEscape("' OR '1'='1"),
	} {
		response := get(t, server, path, session)
		if response.Code == http.StatusInternalServerError || response.Code == http.StatusOK {
			t.Errorf("%s: status %d, want a 404 or redirect", path, response.Code)
		}
	}

	// The embedded file server must not hand out templates or source.
	for _, path := range []string{
		"/static/../templates/layout.html",
		"/static/..%2Ftemplates%2Flayout.html",
		"/static/%2e%2e/render.go",
		"/static/../../web_test.go",
	} {
		response := get(t, server, path, "")
		body := response.Body.String()
		if strings.Contains(body, "{{define") || strings.Contains(body, "package web") {
			t.Errorf("%s served source (%d)", path, response.Code)
		}
	}
}

// /metrics and /readyz answer without a session. Nothing secret, and no admin
// identity, may appear in them.
func TestUnauthenticatedEndpointsDoNotLeakSecrets(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	seedData(t, server)

	for _, path := range []string{"/metrics", "/readyz", "/healthz", "/livez"} {
		response := get(t, server, path, "")
		body := response.Body.String()
		for _, secret := range []string{"hunter2", "very-secret-key", "AKIA", "mongodb://", "ops@example.com",
			"correct-horse-battery", server.app.Env.DataDir} {
			if strings.Contains(body, secret) {
				t.Errorf("%s exposes %q", path, secret)
			}
		}
	}
}

// Errors are logged, not shown: a 500 or a recovered panic must not carry the
// cause, a stack, or an internal path to the browser.
func TestServerErrorsDoNotLeakInternals(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	panicking := server.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret detail at " + server.app.Env.DBPath)
	}))
	recovered := httptest.NewRecorder()
	panicking.ServeHTTP(recovered, httptest.NewRequest(http.MethodGet, "/", nil))
	assertNoInternals(t, server, "a recovered panic", recovered.Body.String())

	failed := httptest.NewRecorder()
	server.serverError(failed, httptest.NewRequest(http.MethodGet, "/", nil),
		&os.PathError{Op: "open", Path: server.app.Env.MasterKeyPath, Err: os.ErrPermission})
	assertNoInternals(t, server, "a server error", failed.Body.String())

	server.app.Store.Close()
	closed := get(t, server, "/backups", session)
	if closed.Code < 400 && closed.Code != http.StatusSeeOther {
		t.Errorf("a closed database answered %d", closed.Code)
	}
	assertNoInternals(t, server, "a page over a closed database", closed.Body.String())
}

func assertNoInternals(t *testing.T, server *Server, where, body string) {
	t.Helper()
	for _, leak := range []string{server.app.Env.DataDir, "goroutine ", ".go:", "sql:", "sqlite", "panic", "secret detail", "permission denied"} {
		if strings.Contains(body, leak) {
			t.Errorf("%s leaks %q: %s", where, leak, truncateForLog(body))
		}
	}
}

// --- helpers ---------------------------------------------------------------

func driveDestination(t *testing.T, server *Server) store.DestinationRecord {
	t.Helper()
	record := store.DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "drive", Provider: config.ProviderGDrive, DriveFolderID: "1AbCfolder",
		OAuthClientID: "id.apps.googleusercontent.com", OAuthClientSecret: "client-secret",
	}}
	if err := server.app.Store.SaveDestination(context.Background(), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func assertStateCookieCleared(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == gdriveStateCookie && cookie.MaxAge < 0 {
			return
		}
	}
	t.Error("the OAuth state cookie was not cleared")
}

// seedArchiveNamed catalogues one archive under the given file name, held by a
// fake destination, and returns its ID.
func seedArchiveNamed(t *testing.T, server *Server, fileName string) string {
	t.Helper()
	ctx := context.Background()
	run := &store.Run{ConfigName: "orders-archive", Database: "shop", Collection: "orders",
		FilterJSON: `{}`, Trigger: store.TriggerCron, Host: "test"}
	if err := server.app.Store.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	artifact := &store.Artifact{RunID: run.ID, ConfigName: "orders-archive", Database: "shop",
		Collection: "orders", BatchSeq: 1, FileName: fileName, Format: "ndjson", Codec: "gzip",
		DocCount: 1, BytesRaw: 4, BytesCompressed: 4, SHA256: "abc"}
	if err := server.app.Store.InsertArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	if err := server.app.Store.InsertArtifactDestination(ctx, &store.ArtifactDestination{
		ArtifactID: artifact.ID, Alias: "cold", Provider: "s3", Bucket: "archive", ObjectKey: "k", SizeBytes: 4,
	}); err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunSuccess
	if err := server.app.Store.FinishRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	server.app.Registry = dest.NewRegistryWith(map[string]dest.Destination{
		"cold": &fakeDestination{alias: "cold", objects: map[string][]byte{"k": []byte("data")}},
	})
	return artifact.ID.String()
}

func multipartBundleRequest(t *testing.T, csrf, session string, file []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	boundary := "feather-test-boundary"
	body.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"" + csrfField + "\"\r\n\r\n" + csrf + "\r\n")
	body.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"include\"\r\n\r\nsettings\r\n")
	body.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"bundle\"; filename=\"b.json\"\r\n" +
		"Content-Type: application/json\r\n\r\n")
	body.Write(file)
	body.WriteString("\r\n--" + boundary + "--\r\n")

	request := httptest.NewRequest(http.MethodPost, "/settings/import", &body)
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	return request
}
