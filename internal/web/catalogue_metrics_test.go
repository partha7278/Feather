package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/store"
)

// fakeDestination serves archive downloads from memory, or fails to.
type fakeDestination struct {
	alias   string
	objects map[string][]byte
	err     error
}

func (f *fakeDestination) Alias() string               { return f.alias }
func (f *fakeDestination) Info() dest.Info             { return dest.Info{Alias: f.alias, Provider: "fake"} }
func (f *fakeDestination) Check(context.Context) error { return f.err }
func (f *fakeDestination) Key(key string) string       { return key }
func (f *fakeDestination) Verify(context.Context, dest.StoredObject) error {
	return f.err
}
func (f *fakeDestination) Upload(context.Context, string, io.Reader, int64, dest.Metadata) (string, error) {
	return "", errors.New("read-only fake")
}
func (f *fakeDestination) Open(_ context.Context, key, _ string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, errors.New("no such object")
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// seededArtifact returns the one archive seedData catalogues.
func seededArtifact(t *testing.T, server *Server) store.ArtifactWithCopies {
	t.Helper()
	artifacts, _, err := server.app.Store.Artifacts(context.Background(), store.ArtifactQuery{ConfigName: "orders-archive"})
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("seeded artifacts: %d, %v", len(artifacts), err)
	}
	return artifacts[0]
}

const seededObjectKey = "archive/orders-archive-00001.ndjson.gz"

func TestDownloadingAnArchiveStreamsItAndAuditsIt(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := seededArtifact(t, server)

	payload := []byte("compressed-archive-bytes")
	server.app.Registry = dest.NewRegistryWith(map[string]dest.Destination{
		"cold": &fakeDestination{alias: "cold", objects: map[string][]byte{seededObjectKey: payload}},
	})

	response := get(t, server, "/backups/"+artifact.ID.String()+"/download", session)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, truncateForLog(response.Body.String()))
	}
	if !bytes.Equal(response.Body.Bytes(), payload) {
		t.Errorf("downloaded %q, want the stored bytes", response.Body.Bytes())
	}
	if ct := response.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content type %q", ct)
	}
	if cd := response.Header().Get("Content-Disposition"); cd != `attachment; filename="orders-archive-00001.ndjson.gz"` {
		t.Errorf("content disposition %q", cd)
	}
	if cc := response.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache control %q", cc)
	}
	assertAudited(t, server, store.AuditArchiveDownloaded, artifact.ID.String())
}

func TestAnUnreadableArchiveRedirectsWithAnError(t *testing.T) {
	cases := []struct {
		name     string
		registry *dest.Registry
	}{
		{"the destination is no longer configured", dest.NewRegistryWith(nil)},
		{"the destination cannot be read", dest.NewRegistryWith(map[string]dest.Destination{
			"cold": &fakeDestination{alias: "cold", err: errors.New("access denied")},
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)
			seedData(t, server)
			artifact := seededArtifact(t, server)
			server.app.Registry = tc.registry

			response := get(t, server, "/backups/"+artifact.ID.String()+"/download", session)
			assertRedirect(t, response, "/backups")
			if kind := flashKind(response); kind != "error" {
				t.Errorf("flash kind %q, want error", kind)
			}
			assertNotAudited(t, server, store.AuditArchiveDownloaded)
		})
	}
}

// One broken bucket must not make an archive unreachable while another copy
// is fine.
func TestADownloadFallsBackToAnotherCopy(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := seededArtifact(t, server)

	second := &store.ArtifactDestination{ArtifactID: artifact.ID, Alias: "warm", Provider: "s3",
		Bucket: "archive-2", ObjectKey: seededObjectKey, SizeBytes: 100}
	if err := server.app.Store.InsertArtifactDestination(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	server.app.Registry = dest.NewRegistryWith(map[string]dest.Destination{
		"cold": &fakeDestination{alias: "cold", err: errors.New("bucket gone")},
		"warm": &fakeDestination{alias: "warm", objects: map[string][]byte{seededObjectKey: []byte("from-warm")}},
	})

	response := get(t, server, "/backups/"+artifact.ID.String()+"/download", session)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if response.Body.String() != "from-warm" {
		t.Errorf("downloaded %q, want the surviving copy", response.Body.String())
	}
}

func TestCatalogueLookupsByBadOrUnknownIDsAreNotFound(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	for _, path := range []string{
		"/backups/not-a-uuid/download",
		"/backups/00000000-0000-0000-0000-000000000000/download",
		"/runs/not-a-uuid",
		"/runs/00000000-0000-0000-0000-000000000000",
	} {
		if code := get(t, server, path, session).Code; code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, code)
		}
	}
}

func TestARunDetailPageRenders(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)

	runs, err := server.app.Store.RecentRuns(context.Background(), "orders-archive", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("seeded runs: %v", err)
	}
	response := get(t, server, "/runs/"+runs[0].RunID.String(), session)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if ct := response.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
}

// Filters and page numbers come straight from the address bar, so nonsense in
// them must still produce a page.
func TestListPagesTolerateOddQueryStrings(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)

	for _, path := range []string{
		"/backups?page=-3", "/backups?page=abc", "/backups?page=999&q=orders&deleted=1",
		"/backups?run=not-a-uuid&config=orders-archive",
		"/runs?config=does-not-exist",
		"/audit?page=0", "/audit?page=abc", "/audit?page=50&actor=x&target=y&action=z",
	} {
		if code := get(t, server, path, session).Code; code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, code)
		}
	}
}

func TestAuditTrailFiltersByActionActorAndTarget(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)

	post(t, server, "/configs/"+jobID+"/enabled", url.Values{"enabled": {""}}, session)
	post(t, server, "/settings", url.Values{"maxConcurrent": {"2"}}, session)

	ctx := context.Background()
	cases := []struct {
		name  string
		query store.AuditQuery
		want  int64
	}{
		{"everything", store.AuditQuery{}, 2},
		{"by action", store.AuditQuery{Action: store.AuditSettingsSaved}, 1},
		{"by actor", store.AuditQuery{Actor: "ops@example.com"}, 2},
		{"by an actor who did nothing", store.AuditQuery{Actor: "someone@example.com"}, 0},
		{"by target", store.AuditQuery{Target: jobID}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, total, err := server.app.Store.AuditEvents(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if total != tc.want {
				t.Errorf("total %d, want %d", total, tc.want)
			}
		})
	}
}

func TestMetricsExposeConfigHealth(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	seedData(t, server)

	response := get(t, server, "/metrics", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if ct := response.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type %q is not the Prometheus text format", ct)
	}
	if cc := response.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache control %q", cc)
	}

	body := response.Body.String()
	for _, want := range []string{
		"feather_configs_total 1\n",
		"feather_configs_enabled 1\n",
		"feather_destinations_total 1\n",
		"feather_mongo_sources_total 1\n",
		"feather_archives_total 1\n",
		`feather_config_enabled{config="orders-archive"} 1`,
		`feather_config_deletes_source{config="orders-archive"} 0`,
		`feather_config_parked{config="orders-archive"} 0`,
		`feather_config_consecutive_failures{config="orders-archive"} 0`,
		`feather_config_running{config="orders-archive"} 0`,
		`feather_config_last_success_age_seconds{config="orders-archive"} `,
		"# TYPE feather_archives_total counter",
		"# TYPE feather_config_enabled gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics are missing %q", want)
		}
	}
}

// The consecutive-failures gauge is what an alert rule watches: a config
// failing every night looks fine everywhere except the runs page.
func TestMetricsCountFailuresSinceTheLastSuccess(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	seedData(t, server)
	ctx := context.Background()

	run := &store.Run{ConfigName: "orders-archive", Database: "shop", Collection: "orders",
		FilterJSON: `{}`, Trigger: store.TriggerCron, Host: "test"}
	if err := server.app.Store.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunFailed
	if err := server.app.Store.FinishRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	body := get(t, server, "/metrics", "").Body.String()
	if !strings.Contains(body, `feather_config_consecutive_failures{config="orders-archive"} 1`) {
		t.Errorf("a failure after the last success was not counted:\n%s", body)
	}
	if !strings.Contains(body, "feather_runs_failed_total 1\n") {
		t.Error("the failed run is not in the failure counter")
	}
}

func TestMetricsWorkOnAFreshInstall(t *testing.T) {
	server := testServer(t)

	response := get(t, server, "/metrics", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d — the setup gate should not cover /metrics", response.Code)
	}
	if strings.Contains(response.Body.String(), "feather_config_last_success_age_seconds{") {
		t.Error("an install with no configs reported a success age")
	}
}

func TestHealthEndpoints(t *testing.T) {
	server := testServer(t)

	for _, path := range []string{"/healthz", "/livez", "/readyz"} {
		response := get(t, server, path, "")
		if response.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, response.Code)
		}
		if cc := response.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: cache control %q", path, cc)
		}
		if ct := response.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("%s: content type %q", path, ct)
		}
	}

	ready := get(t, server, "/readyz", "").Body.String()
	for _, check := range []string{"database: ok", "master_key: ok"} {
		if !strings.Contains(ready, check) {
			t.Errorf("/readyz does not report %q:\n%s", check, ready)
		}
	}
}

func TestReadinessFailsWhenTheDatabaseIsGone(t *testing.T) {
	server := testServer(t)
	server.app.Store.Close()

	response := get(t, server, "/readyz", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", response.Code)
	}
	if !strings.Contains(response.Body.String(), "database: fail") {
		t.Errorf("the failing check is not named:\n%s", response.Body)
	}
}

func TestStaticAssetsAreServedWithoutASession(t *testing.T) {
	server := testServer(t) // fresh install: the setup gate must not cover assets

	cases := []struct {
		path        string
		contentType string
	}{
		{"/static/app.css", "text/css"},
		{"/static/app.js", "text/javascript"},
		{"/static/logo.svg", "image/svg+xml"},
	}
	for _, tc := range cases {
		response := get(t, server, tc.path, "")
		if response.Code != http.StatusOK {
			t.Errorf("%s: status %d", tc.path, response.Code)
			continue
		}
		if ct := response.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
			t.Errorf("%s: content type %q, want %s", tc.path, ct, tc.contentType)
		}
		if cc := response.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
			t.Errorf("%s: cache control %q", tc.path, cc)
		}
		if response.Body.Len() == 0 {
			t.Errorf("%s: empty body", tc.path)
		}
	}

	if code := get(t, server, "/static/does-not-exist.css", "").Code; code != http.StatusNotFound {
		t.Errorf("a missing asset answered %d, want 404", code)
	}
}

// Oversized form posts are cut off rather than buffered in full.
func TestOversizedFormPostsAreRefused(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	form := url.Values{"csrf": {csrfFor(t, server, session)}, "instanceName": {strings.Repeat("x", maxFormBytes+1)}}
	request := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code == http.StatusSeeOther || response.Code == http.StatusOK {
		t.Fatalf("an oversized post was accepted with %d", response.Code)
	}
	name, err := server.app.Store.Meta(context.Background(), "instance_name", "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Error("an oversized post was stored")
	}
}

// A panic in one handler is answered with a 500 rather than killing the
// process that also runs the archives.
func TestAPanickingHandlerIsRecovered(t *testing.T) {
	server := testServer(t)
	handler := server.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", response.Code)
	}
}
