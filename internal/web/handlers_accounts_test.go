package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/store"
)

// These tests assert on what a handler does — status, where it sends the
// browser, what it stored, what it audited — and deliberately not on markup,
// which is free to change under them.

func TestSavingAMongoAccountStoresItAndAuditsIt(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := post(t, server, "/mongo", url.Values{
		"alias":   {"reporting"},
		"uri":     {"mongodb+srv://reader:pw@cluster0.example.net"},
		"appName": {"feather"},
		"enabled": {"on"},
	}, session)

	assertRedirect(t, response, "/mongo")
	if kind := flashKind(response); kind != "ok" {
		t.Errorf("flash kind %q, want ok", kind)
	}
	sources, err := server.app.Store.ListMongoSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Alias != "reporting" || !sources[0].Enabled {
		t.Fatalf("stored sources: %+v", sources)
	}
	assertAudited(t, server, store.AuditSourceSaved, "reporting")
}

func TestMongoAccountFormRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
	}{
		{"a connection string that is not one", url.Values{"alias": {"prod"}, "uri": {"postgres://x@y/z"}}},
		{"a missing alias", url.Values{"uri": {"mongodb://127.0.0.1:27017"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)

			response := post(t, server, "/mongo", tc.form, session)
			assertRedirect(t, response, "/mongo/new")
			if kind := flashKind(response); kind != "error" {
				t.Errorf("flash kind %q, want error", kind)
			}
			sources, err := server.app.Store.ListMongoSources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(sources) != 0 {
				t.Errorf("an invalid account was stored: %+v", sources)
			}
		})
	}
}

// A rejected edit returns to that account's own form, not to a blank new one.
func TestARejectedMongoEditReturnsToItsOwnForm(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	sourceID, _ := seedAccountsOnly(t, server)

	response := post(t, server, "/mongo/"+sourceID, url.Values{"alias": {"prod"}, "uri": {"http://nope"}}, session)
	assertRedirect(t, response, "/mongo/"+sourceID)

	stored, err := server.app.Store.MongoSourceByID(context.Background(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored.URI, "mongodb://") {
		t.Errorf("the rejected URI replaced the stored one: %q", stored.URI)
	}
}

func TestAMongoAccountInUseCannotBeDeleted(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	sourceID, _, _ := seedData(t, server)

	response := post(t, server, "/mongo/"+sourceID+"/delete", url.Values{}, session)
	assertRedirect(t, response, "/mongo")
	if kind := flashKind(response); kind != "error" {
		t.Errorf("flash kind %q, want error", kind)
	}
	if _, err := server.app.Store.MongoSourceByID(context.Background(), sourceID); err != nil {
		t.Fatalf("an account a config depends on was deleted: %v", err)
	}
	assertNotAudited(t, server, store.AuditSourceDeleted)
}

func TestAnUnusedMongoAccountIsDeletedAndAudited(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	sourceID, _ := seedAccountsOnly(t, server)

	response := post(t, server, "/mongo/"+sourceID+"/delete", url.Values{}, session)
	assertRedirect(t, response, "/mongo")
	if _, err := server.app.Store.MongoSourceByID(context.Background(), sourceID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the account is still there: %v", err)
	}
	assertAudited(t, server, store.AuditSourceDeleted, sourceID)
}

func TestSavingAnS3DestinationStoresItAndAuditsIt(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := post(t, server, "/destinations", url.Values{
		"alias":           {"minio"},
		"provider":        {"s3"},
		"region":          {"us-east-1"},
		"bucket":          {"archive"},
		"endpoint":        {"https://minio.internal:9000/"},
		"accessKeyId":     {"AKIA"},
		"secretAccessKey": {"s3cr3t"},
		"prefix":          {"/feather/"},
		"forcePathStyle":  {"on"},
		"enabled":         {"on"},
	}, session)

	assertRedirect(t, response, "/destinations")
	destinations, err := server.app.Store.ListDestinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 1 {
		t.Fatalf("stored %d destinations, want 1", len(destinations))
	}
	got := destinations[0]
	if got.Alias != "minio" || got.Bucket != "archive" || !got.ForcePathStyle {
		t.Errorf("stored destination: %+v", got)
	}
	// The form tidies the slashes an operator is likely to type.
	if got.Endpoint != "https://minio.internal:9000" {
		t.Errorf("endpoint %q kept its trailing slash", got.Endpoint)
	}
	if got.Prefix != "feather" {
		t.Errorf("prefix %q kept its surrounding slashes", got.Prefix)
	}
	assertAudited(t, server, store.AuditDestSaved, "minio")
}

func TestS3DestinationFormRejectsIncompleteAccounts(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
	}{
		{"no bucket", url.Values{"alias": {"cold"}, "provider": {"s3"}, "region": {"eu-west-1"}}},
		{"no region", url.Values{"alias": {"cold"}, "provider": {"s3"}, "bucket": {"b"}}},
		{"no alias", url.Values{"provider": {"s3"}, "region": {"eu-west-1"}, "bucket": {"b"}}},
		// A custom endpoint has no ambient credentials to fall back on.
		{"a custom endpoint without a secret", url.Values{
			"alias": {"minio"}, "provider": {"s3"}, "region": {"us-east-1"}, "bucket": {"b"},
			"endpoint": {"http://127.0.0.1:9000"}, "accessKeyId": {"AKIA"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)

			response := post(t, server, "/destinations", tc.form, session)
			assertRedirect(t, response, "/destinations/new")
			if kind := flashKind(response); kind != "error" {
				t.Errorf("flash kind %q, want error", kind)
			}
			destinations, err := server.app.Store.ListDestinations(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(destinations) != 0 {
				t.Errorf("an incomplete destination was stored: %+v", destinations)
			}
		})
	}
}

// On an edit a blank secret means "keep the stored one", so an operator can
// change the bucket without retyping the key.
func TestEditingADestinationWithABlankSecretKeepsTheStoredOne(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, destinationID := seedAccountsOnly(t, server)

	response := post(t, server, "/destinations/"+destinationID, url.Values{
		"alias": {"cold"}, "provider": {"s3"}, "region": {"ap-south-1"}, "bucket": {"archive-2"},
		"accessKeyId": {"AKIA"}, "enabled": {"on"},
	}, session)
	assertRedirect(t, response, "/destinations")

	stored, err := server.app.Store.DestinationByID(context.Background(), destinationID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Bucket != "archive-2" {
		t.Errorf("bucket %q, want archive-2", stored.Bucket)
	}
	if stored.SecretAccessKey != "very-secret-key" {
		t.Error("a blank secret on edit wiped the stored secret")
	}
}

func TestADestinationInUseCannotBeDeleted(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, destinationID, _ := seedData(t, server)

	response := post(t, server, "/destinations/"+destinationID+"/delete", url.Values{}, session)
	assertRedirect(t, response, "/destinations")
	if kind := flashKind(response); kind != "error" {
		t.Errorf("flash kind %q, want error", kind)
	}
	if _, err := server.app.Store.DestinationByID(context.Background(), destinationID); err != nil {
		t.Fatalf("a destination a config writes to was deleted: %v", err)
	}
	assertNotAudited(t, server, store.AuditDestDeleted)
}

func TestAnUnusedDestinationIsDeletedAndAudited(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, destinationID := seedAccountsOnly(t, server)

	response := post(t, server, "/destinations/"+destinationID+"/delete", url.Values{}, session)
	assertRedirect(t, response, "/destinations")
	if _, err := server.app.Store.DestinationByID(context.Background(), destinationID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the destination is still there: %v", err)
	}
	assertAudited(t, server, store.AuditDestDeleted, destinationID)
}

func TestUnknownAccountFormsAreNotFound(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	for _, path := range []string{
		"/mongo/00000000-0000-0000-0000-000000000000",
		"/destinations/00000000-0000-0000-0000-000000000000",
		"/configs/00000000-0000-0000-0000-000000000000",
	} {
		if code := get(t, server, path, session).Code; code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, code)
		}
	}
}

// "Go back where you came from" must not be steerable off-site by a forged
// Referer.
func TestRedirectBackOnlyFollowsASameSiteReferer(t *testing.T) {
	cases := []struct {
		name    string
		referer string
		want    string
	}{
		{"a same-site absolute referer", "http://example.com/configs?page=2", "/configs?page=2"},
		{"a path-only referer", "/destinations", "/destinations"},
		{"another host", "https://evil.example/phish", "/fallback"},
		{"no referer", "", "/fallback"},
		{"a relative path", "configs", "/fallback"},
	}
	server := testServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://example.com/x", nil)
			if tc.referer != "" {
				request.Header.Set("Referer", tc.referer)
			}
			response := httptest.NewRecorder()
			server.redirectBack(response, request, "/fallback")
			assertRedirect(t, response, tc.want)
		})
	}
}

// --- helpers shared by the *_test.go files added alongside this one --------

func assertRedirect(t *testing.T, response *httptest.ResponseRecorder, location string) {
	t.Helper()
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body: %s", response.Code, truncateForLog(response.Body.String()))
	}
	if got := response.Header().Get("Location"); got != location {
		t.Fatalf("redirected to %q, want %q", got, location)
	}
}

// flashKind reads the kind of the one-shot message a handler left for the next
// page — "ok", "error", "info" — without depending on its wording.
func flashKind(response *httptest.ResponseRecorder) string {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name != flashCookie || cookie.Value == "" {
			continue
		}
		decoded, err := url.QueryUnescape(cookie.Value)
		if err != nil {
			return ""
		}
		kind, _, _ := strings.Cut(decoded, "|")
		return kind
	}
	return ""
}

func auditEvents(t *testing.T, server *Server, action string) []store.AuditEvent {
	t.Helper()
	events, _, err := server.app.Store.AuditEvents(context.Background(), store.AuditQuery{Action: action})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func assertAudited(t *testing.T, server *Server, action, target string) {
	t.Helper()
	events := auditEvents(t, server, action)
	if len(events) != 1 {
		t.Fatalf("recorded %d %q events, want 1", len(events), action)
	}
	if events[0].Target != target {
		t.Errorf("%q event targets %q, want %q", action, events[0].Target, target)
	}
	if events[0].Actor != "ops@example.com" {
		t.Errorf("%q event attributed to %q", action, events[0].Actor)
	}
}

func assertNotAudited(t *testing.T, server *Server, action string) {
	t.Helper()
	if events := auditEvents(t, server, action); len(events) != 0 {
		t.Errorf("recorded %d %q events for a change that did not happen", len(events), action)
	}
}
