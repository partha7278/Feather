package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

const exportPassphrase = "correct-horse-battery-staple"

// everything is what the form posts with every "Include" box left ticked.
var everything = []string{"mongoSources", "destinations", "configs", "settings", "runs"}

// The download is the whole installation leaving the machine, so it is checked
// for the two things that matter: that it is offered as a file, and that the
// credentials in it are sealed.
func TestExportDownloadsASealedBundle(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)

	response := post(t, server, "/settings/export", url.Values{
		"include":           everything,
		"passphrase":        {exportPassphrase},
		"passphraseConfirm": {exportPassphrase},
	}, session)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d, body: %s", response.Code, truncateForLog(response.Body.String()))
	}
	disposition := response.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, ".json") {
		t.Errorf("Content-Disposition is %q", disposition)
	}
	if cache := response.Header().Get("Cache-Control"); cache != "no-store" {
		t.Errorf("Cache-Control is %q", cache)
	}

	body := response.Body.String()
	for _, secret := range []string{"hunter2", "very-secret-key"} {
		if strings.Contains(body, secret) {
			t.Errorf("%q was downloaded in the clear", secret)
		}
	}
	if !strings.Contains(body, "orders-archive") {
		t.Error("the retention config is not in the bundle")
	}
}

// Exporting credentials without a passphrase would write a plaintext copy of
// every key feather holds.
func TestExportRefusesToWriteCredentialsWithoutAPassphrase(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)

	response := post(t, server, "/settings/export", url.Values{"include": everything}, session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want the form bounced back", response.Code)
	}

	mismatched := post(t, server, "/settings/export", url.Values{
		"include":           everything,
		"passphrase":        {exportPassphrase},
		"passphraseConfirm": {"something-else-entirely"},
	}, session)
	if mismatched.Code != http.StatusSeeOther {
		t.Fatalf("a mismatched confirmation returned %d", mismatched.Code)
	}

	// Without credentials, no passphrase is needed and the download proceeds.
	open := post(t, server, "/settings/export", url.Values{"include": everything, "withoutSecrets": {"true"}}, session)
	if open.Code != http.StatusOK {
		t.Fatalf("a credential-free export returned %d", open.Code)
	}
	if strings.Contains(open.Body.String(), "hunter2") {
		t.Error("a credential-free export carried a connection string")
	}
}

func TestSettingsOffersExportAndImport(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	page := get(t, server, "/settings", session).Body.String()
	for _, want := range []string{
		`action="/settings/export"`,
		`action="/settings/import"`,
		`enctype="multipart/form-data"`,
		`name="bundle"`,
		`name="withoutSecrets"`,
		`name="overwrite"`,
		`name="include" value="mongoSources"`,
		`name="include" value="settings"`,
		`name="include" value="runs"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page is missing %s", want)
		}
	}
}

// Export from one installation, upload into another, through the panel.
func TestImportThroughTheUpload(t *testing.T) {
	ctx := context.Background()

	from := testServer(t)
	createAdmin(t, from)
	fromSession := signIn(t, from)
	seedData(t, from)

	exported := post(t, from, "/settings/export", url.Values{
		"include":           everything,
		"passphrase":        {exportPassphrase},
		"passphraseConfirm": {exportPassphrase},
	}, fromSession).Body.Bytes()

	to := testServer(t)
	createAdmin(t, to)
	toSession := signIn(t, to)

	response := upload(t, to, "/settings/import", exported, url.Values{
		"include":    everything,
		"passphrase": {exportPassphrase},
	}, toSession)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body: %s", response.Code, truncateForLog(response.Body.String()))
	}

	sources, err := to.app.Store.ListMongoSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].URI != "mongodb://user:hunter2@127.0.0.1:27017" {
		t.Fatalf("the MongoDB account did not arrive: %+v", sources)
	}
	record, err := to.app.Store.JobByName(ctx, "orders-archive")
	if err != nil {
		t.Fatalf("the retention config did not arrive: %v", err)
	}
	if record.Config.Source.Collection != "orders" {
		t.Errorf("the config arrived as %+v", record.Config.Source)
	}

	// A wrong passphrase must be named, and must change nothing.
	other := testServer(t)
	createAdmin(t, other)
	otherSession := signIn(t, other)
	refused := upload(t, other, "/settings/import", exported,
		url.Values{"include": everything, "passphrase": {"not-the-passphrase"}}, otherSession)
	if refused.Code != http.StatusSeeOther {
		t.Fatalf("status %d", refused.Code)
	}
	if sources, err := other.app.Store.ListMongoSources(ctx); err != nil || len(sources) != 0 {
		t.Fatalf("%d accounts were written under a wrong passphrase (%v)", len(sources), err)
	}
}

// Picking only configs leaves the credentials behind, so neither side asks for
// a passphrase, and the accounts stay where they are.
func TestExportAndImportOnlyThePartsChosen(t *testing.T) {
	ctx := context.Background()

	from := testServer(t)
	createAdmin(t, from)
	seedData(t, from)

	exported := post(t, from, "/settings/export", url.Values{"include": {"configs"}}, signIn(t, from))
	if exported.Code != http.StatusOK {
		t.Fatalf("a configs-only export returned %d", exported.Code)
	}
	body := exported.Body.String()
	if !strings.Contains(body, "orders-archive") {
		t.Error("the retention config is not in the bundle")
	}
	if strings.Contains(body, "hunter2") || strings.Contains(body, `"secrets"`) {
		t.Error("a configs-only export carried credentials")
	}

	full := post(t, from, "/settings/export", url.Values{
		"include":           everything,
		"passphrase":        {exportPassphrase},
		"passphraseConfirm": {exportPassphrase},
	}, signIn(t, from)).Body.Bytes()

	to := testServer(t)
	createAdmin(t, to)
	response := upload(t, to, "/settings/import", full, url.Values{"include": {"configs"}}, signIn(t, to))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d", response.Code)
	}
	if kind := flashKind(response); kind == "error" {
		t.Fatalf("a configs-only import of a sealed bundle asked for the passphrase")
	}
	if sources, err := to.app.Store.ListMongoSources(ctx); err != nil || len(sources) != 0 {
		t.Errorf("%d accounts were imported though they were not chosen (%v)", len(sources), err)
	}

	// The config names an account and a destination; once those exist here, a
	// configs-only import lands it.
	seedAccountsOnly(t, to)
	upload(t, to, "/settings/import", full, url.Values{"include": {"configs"}}, signIn(t, to))
	if _, err := to.app.Store.JobByName(ctx, "orders-archive"); err != nil {
		t.Errorf("the retention config did not arrive: %v", err)
	}
}

// The execution log carries over as it was: same run, same file, same record
// of where that file went — and a second import adds nothing.
func TestExecutionLogsCarryOver(t *testing.T) {
	ctx := context.Background()

	from := testServer(t)
	createAdmin(t, from)
	seedData(t, from)
	runs, err := from.app.Store.RecentRuns(ctx, "", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("seeded %d runs (%v), want 1", len(runs), err)
	}
	original := runs[0]

	exported := post(t, from, "/settings/export", url.Values{"include": {"runs"}}, signIn(t, from))
	if exported.Code != http.StatusOK {
		t.Fatalf("an execution-log export returned %d", exported.Code)
	}
	body := exported.Body.Bytes()
	if !strings.Contains(string(body), "orders-archive-00001.ndjson.gz") {
		t.Error("the archived file is not in the execution log")
	}

	to := testServer(t)
	createAdmin(t, to)
	session := signIn(t, to)
	response := upload(t, to, "/settings/import", body, url.Values{"include": {"runs"}}, session)
	if kind := flashKind(response); kind == "error" {
		t.Fatalf("the import failed (status %d)", response.Code)
	}

	run, err := to.app.Store.Run(ctx, original.RunID)
	if err != nil {
		t.Fatalf("the run did not arrive: %v", err)
	}
	if run.Status != original.Status || run.DocsArchived != original.DocsArchived ||
		!run.StartedAt.Equal(original.StartedAt) {
		t.Errorf("the run arrived as %+v, want %+v", run, original)
	}
	files, err := to.app.Store.ArtifactsForRun(ctx, original.RunID)
	if err != nil || len(files) != 1 {
		t.Fatalf("%d files arrived with the run (%v), want 1", len(files), err)
	}
	if len(files[0].Copies) != 1 || files[0].Copies[0].Alias != "cold" ||
		files[0].Copies[0].ObjectKey != "archive/orders-archive-00001.ndjson.gz" {
		t.Errorf("the file's copies arrived as %+v", files[0].Copies)
	}

	again := upload(t, to, "/settings/import", body, url.Values{"include": {"runs"}}, session)
	if kind := flashKind(again); kind != "info" {
		t.Errorf("a repeat import flashed %q, want info (nothing imported)", kind)
	}
	if runs, err := to.app.Store.RecentRuns(ctx, "", 10); err != nil || len(runs) != 1 {
		t.Errorf("%d runs after a repeat import (%v), want 1", len(runs), err)
	}
}

func TestExportAndImportRefuseAnEmptyChoice(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)

	exported := post(t, server, "/settings/export", url.Values{"withoutSecrets": {"true"}}, session)
	if exported.Code != http.StatusSeeOther || flashKind(exported) != "error" {
		t.Fatalf("an export of nothing returned %d", exported.Code)
	}

	bundle := post(t, server, "/settings/export", url.Values{"include": {"configs"}}, session).Body.Bytes()
	imported := upload(t, server, "/settings/import", bundle, nil, session)
	if imported.Code != http.StatusSeeOther || flashKind(imported) != "error" {
		t.Fatalf("an import of nothing returned %d", imported.Code)
	}
}

func TestImportRejectsAFileThatIsNotABundle(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := upload(t, server, "/settings/import", []byte(`{"name":"orders-archive"}`), url.Values{"include": everything}, session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d", response.Code)
	}
	if jobs, err := server.app.Store.ListJobs(context.Background()); err != nil || len(jobs) != 0 {
		t.Fatalf("%d configs were created from a file that is not a bundle (%v)", len(jobs), err)
	}
}

// upload posts a bundle the way the browser does, as multipart form data.
func upload(t *testing.T, server *Server, path string, file []byte, fields url.Values, session string) *httptest.ResponseRecorder {
	t.Helper()

	stored, _, err := server.app.Store.SessionByToken(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField(csrfField, stored.CSRFToken); err != nil {
		t.Fatal(err)
	}
	for name, values := range fields {
		for _, value := range values {
			if err := writer.WriteField(name, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	part, err := writer.CreateFormFile("bundle", "feather.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, path, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

// The master key is the other half of every backup, so it can be downloaded —
// but only after the password is typed again, and never cached.
func TestMasterKeyDownloadNeedsThePassword(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	wrong := post(t, server, "/settings/master-key", url.Values{"password": {"not-the-password"}}, session)
	if wrong.Code != http.StatusSeeOther {
		t.Fatalf("a wrong password returned %d, want the form bounced back", wrong.Code)
	}

	response := post(t, server, "/settings/master-key", url.Values{"password": {"correct-horse-battery"}}, session)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, body: %s", response.Code, truncateForLog(response.Body.String()))
	}
	if disposition := response.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment;") {
		t.Errorf("Content-Disposition is %q", disposition)
	}
	if cache := response.Header().Get("Cache-Control"); cache != "no-store" {
		t.Errorf("Cache-Control is %q", cache)
	}

	onDisk, err := os.ReadFile(server.app.Env.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(response.Body.String()), strings.TrimSpace(string(onDisk)); got != want {
		t.Error("the downloaded key is not the one this feather uses")
	}
}
