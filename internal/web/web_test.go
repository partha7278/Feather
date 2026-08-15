package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

// Templates are parsed at startup, but a page that references a field wrongly
// only fails when someone opens it. Every page is rendered here for that reason.
func TestTemplatesParse(t *testing.T) {
	set, err := loadTemplates()
	if err != nil {
		t.Fatal(err)
	}

	for _, page := range []string{
		"setup", "login", "dashboard", "mongo_list", "mongo_form",
		"destination_list", "destination_form", "config_list", "config_form",
		"backups", "runs", "run_detail", "settings",
	} {
		if _, ok := set.pages[page]; !ok {
			t.Errorf("no template for page %q", page)
		}
	}
}

// A fresh installation must lead to the setup form from wherever it is opened,
// and must not offer a login that cannot succeed.
func TestFreshInstallGoesToSetup(t *testing.T) {
	server := testServer(t)

	for _, path := range []string{"/", "/configs", "/backups", "/login"} {
		response := get(t, server, path, "")
		if response.Code != http.StatusSeeOther {
			t.Errorf("%s: status %d, want a redirect", path, response.Code)
		}
		if location := response.Header().Get("Location"); location != "/setup" {
			t.Errorf("%s: redirected to %q, want /setup", path, location)
		}
	}

	if code := get(t, server, "/setup", "").Code; code != http.StatusOK {
		t.Errorf("the setup page itself returned %d", code)
	}
	// Health checks must work before setup, or a deployment looks broken.
	if code := get(t, server, "/healthz", "").Code; code != http.StatusOK {
		t.Errorf("/healthz returned %d", code)
	}
}

func TestSetupCreatesAdminAndSignsIn(t *testing.T) {
	server := testServer(t)

	form := url.Values{
		"email":    {"ops@example.com"},
		"password": {"correct-horse-battery"},
		"confirm":  {"correct-horse-battery"},
	}
	response := postWithPreSessionCSRF(t, server, "/setup", form)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("setup returned %d, body: %s", response.Code, response.Body.String())
	}
	if session := sessionValue(response); session == "" {
		t.Fatal("setup did not sign the new administrator in")
	}

	needsSetup, err := server.app.Store.NeedsSetup(context.Background())
	if err != nil || needsSetup {
		t.Fatal("the administrator was not created")
	}
	// Once set up, /setup must stop existing.
	if location := get(t, server, "/setup", "").Header().Get("Location"); location != "/login" {
		t.Fatalf("/setup after setup redirected to %q", location)
	}
}

func TestSetupRejectsMismatchedPasswords(t *testing.T) {
	server := testServer(t)

	response := postWithPreSessionCSRF(t, server, "/setup", url.Values{
		"email":    {"ops@example.com"},
		"password": {"correct-horse-battery"},
		"confirm":  {"different-passphrase"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want the form re-rendered", response.Code)
	}
	if !strings.Contains(response.Body.String(), "do not match") {
		t.Fatal("the form did not explain what was wrong")
	}

	needsSetup, _ := server.app.Store.NeedsSetup(context.Background())
	if !needsSetup {
		t.Fatal("an administrator was created despite the mismatch")
	}
}

func TestSignedOutRequestsRedirectToLogin(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)

	for _, path := range []string{"/", "/configs", "/backups", "/runs", "/settings", "/mongo"} {
		response := get(t, server, path, "")
		if response.Code != http.StatusSeeOther {
			t.Errorf("%s: status %d, want a redirect", path, response.Code)
		}
		if location := response.Header().Get("Location"); !strings.HasPrefix(location, "/login") {
			t.Errorf("%s: redirected to %q", path, location)
		}
	}
}

// Every page is rendered with a real session, which is the only way template
// errors surface.
func TestEveryPageRendersForASignedInAdmin(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	seedData(t, server)

	for _, path := range []string{
		"/", "/mongo", "/mongo/new", "/destinations", "/destinations/new",
		"/configs", "/configs/new", "/backups", "/runs", "/settings",
	} {
		response := get(t, server, path, session)
		if response.Code != http.StatusOK {
			t.Errorf("%s: status %d, body: %s", path, response.Code, truncateForLog(response.Body.String()))
			continue
		}
		if !strings.Contains(response.Body.String(), "</html>") {
			t.Errorf("%s: page did not render fully", path)
		}
	}
}

// The edit forms take an ID, so they need their own pass over real records.
func TestEditFormsRender(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	sourceID, destinationID, jobID := seedData(t, server)

	for _, path := range []string{
		"/mongo/" + sourceID,
		"/destinations/" + destinationID,
		"/configs/" + jobID,
	} {
		response := get(t, server, path, session)
		if response.Code != http.StatusOK {
			t.Errorf("%s: status %d, body: %s", path, response.Code, truncateForLog(response.Body.String()))
		}
	}
}

// A stored connection string must never be written back into a form field.
func TestEditFormDoesNotLeakStoredSecrets(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	sourceID, destinationID, _ := seedData(t, server)

	mongoPage := get(t, server, "/mongo/"+sourceID, session).Body.String()
	if strings.Contains(mongoPage, "hunter2") {
		t.Error("the MongoDB password was rendered into the edit form")
	}
	destinationPage := get(t, server, "/destinations/"+destinationID, session).Body.String()
	if strings.Contains(destinationPage, "very-secret-key") {
		t.Error("the destination secret was rendered into the edit form")
	}
}

// Without this, a page on another site could delete a config by posting to it.
func TestPostsRequireACSRFToken(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)

	request := httptest.NewRequest(http.MethodPost, "/configs/"+jobID+"/delete",
		strings.NewReader(url.Values{"csrf": {"forged"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if _, err := server.app.Store.JobByID(context.Background(), jobID); err != nil {
		t.Fatalf("the config was removed by a request with a bad CSRF token: %v", err)
	}
}

// Toggling deletion is the action that decides whether archiving destroys data,
// so it gets its own test.
func TestDeletionToggle(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)

	post(t, server, "/configs/"+jobID+"/deletion", url.Values{"deleteAfterArchive": {"true"}}, session)

	record, err := server.app.Store.JobByID(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Config.Retention.DeleteAfterArchive {
		t.Fatal("deletion was not enabled")
	}

	post(t, server, "/configs/"+jobID+"/deletion", url.Values{"deleteAfterArchive": {"false"}}, session)

	record, err = server.app.Store.JobByID(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Config.Retention.DeleteAfterArchive {
		t.Fatal("deletion was not disabled")
	}
}

func TestCreatingAConfigThroughTheForm(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	form := url.Values{
		"name":               {"orders-archive"},
		"enabled":            {"on"},
		"mongoSource":        {"prod"},
		"database":           {"shop"},
		"collection":         {"orders"},
		"filter":             {`{"status":"completed"}`},
		"destinations":       {"cold"},
		"keyTemplate":        {"archive/{db}/{collection}/{config}-{runId}-{seq}{ext}"},
		"codec":              {"gzip"},
		"codecLevel":         {"6"},
		"batchMaxDocs":       {"10000"},
		"batchMaxBytes":      {"8MB"},
		"fetchSize":          {"1000"},
		"scheduleKind":       {"every"},
		"every":              {"1d"},
		"at":                 {"02:30"},
		"timezone":           {"Asia/Kolkata"},
		"maxRuntime":         {"6h"},
		"deleteAfterArchive": {"on"},
		"verifyBeforeDelete": {"on"},
		"deleteBatchSize":    {"5000"},
		"readPreference":     {"primary"},
	}
	response := post(t, server, "/configs", form, session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body: %s", response.Code, truncateForLog(response.Body.String()))
	}

	record, err := server.app.Store.JobByName(context.Background(), "orders-archive")
	if err != nil {
		t.Fatalf("the config was not created: %v", err)
	}
	if !record.Config.Retention.DeleteAfterArchive {
		t.Error("the deletion flag was not saved")
	}
	if record.Config.Output.KeyTemplate != "archive/{db}/{collection}/{config}-{runId}-{seq}{ext}" {
		t.Errorf("the file name template was not saved: %q", record.Config.Output.KeyTemplate)
	}
}

// A typo in the file naming template would otherwise be written into object
// keys literally and only be noticed much later.
func TestConfigFormRejectsAnUnknownFileNameVariable(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	form := url.Values{
		"name": {"typo"}, "mongoSource": {"prod"}, "database": {"shop"}, "collection": {"orders"},
		"filter": {"{}"}, "destinations": {"cold"},
		"keyTemplate": {"archive/{colletion}/{runId}{ext}"},
		"codec":       {"gzip"}, "codecLevel": {"6"}, "batchMaxBytes": {"8MB"},
		"scheduleKind": {"every"}, "every": {"1d"}, "timezone": {"UTC"}, "maxRuntime": {"6h"},
		"readPreference": {"primary"},
	}
	post(t, server, "/configs", form, session)

	if _, err := server.app.Store.JobByName(context.Background(), "typo"); err == nil {
		t.Fatal("a config with an unknown template variable was saved")
	}
}

// --- helpers ---------------------------------------------------------------

func testServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	env := &config.Env{
		DataDir:           dir,
		DBPath:            filepath.Join(dir, "feather.db"),
		MasterKeyPath:     filepath.Join(dir, "master.key"),
		SpoolDir:          filepath.Join(dir, "spool"),
		HTTPAddr:          "127.0.0.1:0",
		MaxConcurrentJobs: 1,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	application, err := app.Open(ctx, env, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close(ctx) })

	server, err := New(application, log, false)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func createAdmin(t *testing.T, server *Server) {
	t.Helper()
	if _, err := server.app.Store.CreateAdmin(context.Background(), "ops@example.com", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
}

func signIn(t *testing.T, server *Server) string {
	t.Helper()

	token, _, err := server.app.Store.CreateSession(context.Background(), adminID(t, server), "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func adminID(t *testing.T, server *Server) string {
	t.Helper()

	admins, err := server.app.Store.ListAdmins(context.Background())
	if err != nil || len(admins) == 0 {
		t.Fatalf("no administrator: %v", err)
	}
	return admins[0].ID
}

func seedAccountsOnly(t *testing.T, server *Server) (string, string) {
	t.Helper()
	ctx := context.Background()

	source := store.MongoSourceRecord{Alias: "prod", URI: "mongodb://user:hunter2@127.0.0.1:27017", Enabled: true}
	if err := server.app.Store.SaveMongoSource(ctx, &source); err != nil {
		t.Fatal(err)
	}
	destination := store.DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "cold", Provider: config.ProviderS3, Region: "ap-south-1", Bucket: "archive",
		AccessKeyID: "AKIA", SecretAccessKey: "very-secret-key",
	}}
	if err := server.app.Store.SaveDestination(ctx, &destination); err != nil {
		t.Fatal(err)
	}
	return source.ID, destination.ID
}

func seedData(t *testing.T, server *Server) (sourceID, destinationID, jobID string) {
	t.Helper()
	ctx := context.Background()

	sourceID, destinationID = seedAccountsOnly(t, server)

	enabled := true
	record := store.JobRecord{Config: &config.JobConfig{
		Name:    "orders-archive",
		Enabled: &enabled,
		Source: config.Source{
			MongoSource: "prod", Database: "shop", Collection: "orders",
			Filter: []byte(`{"status":"completed"}`),
		},
		Output:       config.Output{KeyTemplate: "archive/{config}-{runId}-{seq}{ext}"},
		Destinations: []string{"cold"},
		Schedule:     config.Schedule{Every: "1d", At: "02:30", Timezone: "Asia/Kolkata"},
	}}
	if err := server.app.Store.SaveJob(ctx, &record); err != nil {
		t.Fatal(err)
	}

	// One archived file, so the catalogue pages have something to render.
	run := &store.Run{ConfigName: "orders-archive", Database: "shop", Collection: "orders",
		FilterJSON: `{"status":"completed"}`, Trigger: store.TriggerCron, Host: "test"}
	if err := server.app.Store.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	artifact := &store.Artifact{RunID: run.ID, ConfigName: "orders-archive", Database: "shop",
		Collection: "orders", BatchSeq: 1, FileName: "orders-archive-00001.ndjson.gz",
		Format: "ndjson", Codec: "gzip", DocCount: 100, BytesRaw: 1000, BytesCompressed: 100, SHA256: "abc"}
	if err := server.app.Store.InsertArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	copy := &store.ArtifactDestination{ArtifactID: artifact.ID, Alias: "cold", Provider: "s3",
		Bucket: "archive", ObjectKey: "archive/orders-archive-00001.ndjson.gz", SizeBytes: 100}
	if err := server.app.Store.InsertArtifactDestination(ctx, copy); err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunSuccess
	if err := server.app.Store.FinishRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	return sourceID, destinationID, record.ID
}

func get(t *testing.T, server *Server, path, session string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	if session != "" {
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func post(t *testing.T, server *Server, path string, form url.Values, session string) *httptest.ResponseRecorder {
	t.Helper()

	stored, _, err := server.app.Store.SessionByToken(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	form.Set(csrfField, stored.CSRFToken)

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

// postWithPreSessionCSRF covers the two forms that exist before a session does.
func postWithPreSessionCSRF(t *testing.T, server *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	page := get(t, server, path, "")
	token := ""
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookie {
			token = cookie.Value
		}
	}
	if token == "" {
		t.Fatalf("%s did not issue a CSRF cookie", path)
	}
	form.Set(csrfField, token)

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func sessionValue(response *httptest.ResponseRecorder) string {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}

func truncateForLog(body string) string {
	if len(body) > 400 {
		return body[:400] + "…"
	}
	return body
}

// The Drive half of the destination form posts entirely different fields from
// the S3 half, and an OAuth account is saved before it can be authorised.
func TestAddingAGoogleDriveDestinationThroughTheForm(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	form := url.Values{
		"alias":             {"drive"},
		"provider":          {"gdrive"},
		"driveFolderId":     {"https://drive.google.com/drive/folders/1AbCfolder"},
		"oauthClientId":     {"id.apps.googleusercontent.com"},
		"oauthClientSecret": {"client-secret"},
		"prefix":            {"feather/"},
		"enabled":           {"on"},
	}
	response := post(t, server, "/destinations", form, session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body: %s", response.Code, truncateForLog(response.Body.String()))
	}

	ctx := context.Background()
	destinations, err := server.app.Store.ListDestinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 1 {
		t.Fatalf("%d destinations stored, want 1", len(destinations))
	}
	stored := destinations[0]

	if stored.Provider != config.ProviderGDrive {
		t.Errorf("provider is %q", stored.Provider)
	}
	// A folder URL is what an operator has in front of them; the ID is what the
	// API wants.
	if stored.DriveFolderID != "1AbCfolder" {
		t.Errorf("the folder was stored as %q", stored.DriveFolderID)
	}
	if stored.Prefix != "feather" {
		t.Errorf("the prefix was stored as %q", stored.Prefix)
	}
	if stored.Ready() {
		t.Error("a destination that has not been authorised reports itself ready")
	}

	// Editing it must not render the secret back into the browser.
	page := get(t, server, "/destinations/"+stored.ID, session).Body.String()
	if strings.Contains(page, "client-secret") {
		t.Error("the OAuth client secret was rendered into the edit form")
	}
	if !strings.Contains(page, "Connect Google Drive") {
		t.Error("the edit form does not offer to connect the account")
	}
	if !strings.Contains(page, gdriveCallbackPath) {
		t.Error("the edit form does not show the redirect URI to register")
	}
}

// Connecting sends the operator to Google; there is nothing to send them to
// until the account has an OAuth client.
func TestConnectingDriveNeedsAnOAuthClient(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	ctx := context.Background()

	record := store.DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "drive", Provider: config.ProviderGDrive, DriveFolderID: "1AbCfolder",
		ServiceAccountJSON: `{"type":"service_account","private_key":"very-secret-key"}`,
	}}
	if err := server.app.Store.SaveDestination(ctx, &record); err != nil {
		t.Fatal(err)
	}

	response := post(t, server, "/destinations/"+record.ID+"/gdrive/connect", url.Values{}, session)
	if location := response.Header().Get("Location"); strings.Contains(location, "accounts.google.com") {
		t.Fatal("a service account destination was sent through the browser sign-in")
	}

	page := get(t, server, "/destinations/"+record.ID, session).Body.String()
	if strings.Contains(page, "very-secret-key") {
		t.Error("the service account key was rendered into the edit form")
	}
}

// The callback must not accept a code that arrives without the state feather
// itself set on the way out.
func TestDriveCallbackRejectsAnUnsolicitedCode(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := get(t, server, gdriveCallbackPath+"?code=stolen&state=made-up", session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/destinations" {
		t.Errorf("redirected to %q", location)
	}
}

func TestDriveFolderIDAcceptsAnIDOrAURL(t *testing.T) {
	cases := map[string]string{
		"1AbCfolder": "1AbCfolder",
		"https://drive.google.com/drive/folders/1AbCfolder":            "1AbCfolder",
		"https://drive.google.com/drive/u/0/folders/1AbCfolder?usp=sh": "1AbCfolder",
		"https://drive.google.com/open?id=1AbCfolder":                  "1AbCfolder",
		"": "",
	}
	for input, want := range cases {
		if got := driveFolderID(input); got != want {
			t.Errorf("%q resolved to %q, want %q", input, got, want)
		}
	}
}
