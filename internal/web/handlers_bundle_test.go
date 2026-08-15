package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const exportPassphrase = "correct-horse-battery-staple"

// The download is the whole installation leaving the machine, so it is checked
// for the two things that matter: that it is offered as a file, and that the
// credentials in it are sealed.
func TestExportDownloadsASealedBundle(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)

	response := post(t, server, "/settings/export", url.Values{
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

	response := post(t, server, "/settings/export", url.Values{}, session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want the form bounced back", response.Code)
	}

	mismatched := post(t, server, "/settings/export", url.Values{
		"passphrase":        {exportPassphrase},
		"passphraseConfirm": {"something-else-entirely"},
	}, session)
	if mismatched.Code != http.StatusSeeOther {
		t.Fatalf("a mismatched confirmation returned %d", mismatched.Code)
	}

	// Without credentials, no passphrase is needed and the download proceeds.
	open := post(t, server, "/settings/export", url.Values{"withoutSecrets": {"true"}}, session)
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
		"passphrase":        {exportPassphrase},
		"passphraseConfirm": {exportPassphrase},
	}, fromSession).Body.Bytes()

	to := testServer(t)
	createAdmin(t, to)
	toSession := signIn(t, to)

	response := upload(t, to, "/settings/import", exported, map[string]string{
		"passphrase":     exportPassphrase,
		"importSettings": "true",
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
		map[string]string{"passphrase": "not-the-passphrase"}, otherSession)
	if refused.Code != http.StatusSeeOther {
		t.Fatalf("status %d", refused.Code)
	}
	if sources, err := other.app.Store.ListMongoSources(ctx); err != nil || len(sources) != 0 {
		t.Fatalf("%d accounts were written under a wrong passphrase (%v)", len(sources), err)
	}
}

func TestImportRejectsAFileThatIsNotABundle(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := upload(t, server, "/settings/import", []byte(`{"name":"orders-archive"}`), nil, session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d", response.Code)
	}
	if jobs, err := server.app.Store.ListJobs(context.Background()); err != nil || len(jobs) != 0 {
		t.Fatalf("%d configs were created from a file that is not a bundle (%v)", len(jobs), err)
	}
}

// upload posts a bundle the way the browser does, as multipart form data.
func upload(t *testing.T, server *Server, path string, file []byte, fields map[string]string, session string) *httptest.ResponseRecorder {
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
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
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
