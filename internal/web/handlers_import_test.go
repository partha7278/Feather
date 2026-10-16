package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unifers/feather/internal/store"
)

func TestImportPagesRender(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := firstSeededArchive(t, server)

	for _, path := range []string{
		"/imports", "/imports/new",
		"/imports/new?archive=" + artifact.ID.String(),
		"/imports/new?run=" + artifact.RunID.String(),
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

	if code := get(t, server, "/imports/new?archive=not-an-id", session).Code; code != http.StatusNotFound {
		t.Errorf("an unknown archive gave %d, want 404", code)
	}
	if code := get(t, server, "/imports/00000000-0000-0000-0000-000000000000", session).Code; code != http.StatusNotFound {
		t.Errorf("an unknown import gave %d, want 404", code)
	}
}

// Accepting every default must be safe: the same account and database, a new
// collection, and a mode that never overwrites.
func TestImportFormDefaultsAreSafe(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := firstSeededArchive(t, server)
	// The seeded destination only becomes readable once the registry knows it.
	if err := server.app.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	body := get(t, server, "/imports/new?archive="+artifact.ID.String(), session).Body.String()
	for _, want := range []string{
		`value="shop"`,
		`value="orders_restored_` + time.Now().Format("20060102") + `"`,
		`<option value="prod" selected`,
		`<option value="cold" selected`,
		`value="insert" checked`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("form is missing %s", want)
		}
	}
	if strings.Contains(body, `value="replace" checked`) {
		t.Error("replace is selected by default")
	}
}

func TestImportPostsRequireACSRFToken(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := firstSeededArchive(t, server)

	form := catalogueImportForm(artifact, "dryrun")
	form.Set(csrfField, "forged")
	request := httptest.NewRequest(http.MethodPost, "/imports", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", response.Code)
	}
	assertNoImports(t, server)
}

func TestAReplaceWithTheWrongPasswordWritesNothing(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := firstSeededArchive(t, server)

	form := catalogueImportForm(artifact, "import")
	form.Set("mode", "replace")
	form.Set("password", "not-my-password")
	response := post(t, server, "/imports", form, session)

	if !strings.Contains(response.Body.String(), "Password incorrect, nothing was imported") {
		t.Errorf("no password error shown: %s", truncateForLog(response.Body.String()))
	}
	assertNoImports(t, server)
	assertNoImportAudit(t, server)
}

func TestInvalidImportTargetsAreRefused(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := firstSeededArchive(t, server)

	for _, target := range []struct{ alias, db, coll, want string }{
		{"", "shop", "x", "Choose a MongoDB account"},
		{"prod", "", "x", "Database is required"},
		{"prod", "shop.x", "x", "cannot contain"},
		{"prod", "admin", "x", "MongoDB&#39;s own databases"},
		{"prod", strings.Repeat("d", 65), "x", "at most 64 bytes"},
		{"prod", "shop", "", "Collection is required"},
		{"prod", "shop", "a$b", "cannot contain $"},
		{"prod", "shop", "system.users", "system."},
		{"nobody", "shop", "x", "no MongoDB account called"},
	} {
		form := catalogueImportForm(artifact, "dryrun")
		form.Set("targetAlias", target.alias)
		form.Set("targetDatabase", target.db)
		form.Set("targetCollection", target.coll)
		body := post(t, server, "/imports", form, session).Body.String()
		if !strings.Contains(body, target.want) {
			t.Errorf("%s/%s/%s: expected %q in the response", target.alias, target.db, target.coll, target.want)
		}
	}
	assertNoImports(t, server)
}

// Importing into a collection an enabled config archives and deletes from is
// allowed, but only after the operator has ticked that they know.
func TestImportIntoADeletingConfigsCollectionNeedsAcknowledging(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)
	if err := server.app.Store.SetJobDeleteAfterArchive(context.Background(), jobID, true); err != nil {
		t.Fatal(err)
	}

	fields := map[string]string{"targetAlias": "prod", "targetDatabase": "shop", "targetCollection": "orders", "action": "dryrun"}
	response := postUpload(t, server, session, "orders.ndjson", []byte(`{"_id":1}`+"\n"), fields)
	body := response.Body.String()
	if !strings.Contains(body, "orders-archive") || !strings.Contains(body, `name="acknowledge"`) {
		t.Fatalf("no retention-loop warning: %s", truncateForLog(body))
	}
	assertNoImports(t, server)

	fields["acknowledge"] = "1"
	response = postUpload(t, server, session, "orders.ndjson", []byte(`{"_id":1}`+"\n"), fields)
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/imports/") {
		t.Fatalf("acknowledged import did not start: %d %s", response.Code, truncateForLog(response.Body.String()))
	}
	server.app.Drain(context.Background())
}

func TestAnUploadDryRunStartsRecordsAndAudits(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	body := []byte(`{"_id":1,"n":1}` + "\n" + `{"_id":2,"n":2}` + "\n")
	response := postUpload(t, server, session, "orders.ndjson", body, map[string]string{
		"targetAlias": "prod", "targetDatabase": "shop", "targetCollection": "orders_restored", "action": "dryrun",
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", response.Code, truncateForLog(response.Body.String()))
	}
	server.app.Drain(context.Background())

	imports, err := server.app.Store.Imports(context.Background(), 10)
	if err != nil || len(imports) != 1 {
		t.Fatalf("got %d imports (%v), want 1", len(imports), err)
	}
	imp := imports[0]
	if imp.Status != store.ImportDryRun || imp.DocsRead != 2 || !imp.DryRun || imp.SourceKind != store.ImportSourceUpload {
		t.Errorf("unexpected import: %+v", imp)
	}

	for _, action := range []string{store.AuditImportStarted, store.AuditImportFinished} {
		events, _, err := server.app.Store.AuditEvents(context.Background(), store.AuditQuery{Action: action})
		if err != nil || len(events) != 1 {
			t.Errorf("%s recorded %d times (%v), want once", action, len(events), err)
		}
	}
	assertSpoolEmpty(t, server)

	detail := get(t, server, "/imports/"+imp.ID.String(), session)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "orders.ndjson") {
		t.Errorf("detail page: %d", detail.Code)
	}
	if strings.Contains(detail.Body.String(), "Import for real") {
		t.Error("an upload dry run offered Import for real; the file is gone")
	}
}

func TestAnUnsupportedUploadIsRefused(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	response := postUpload(t, server, session, "orders.csv", []byte("a,b\n"), map[string]string{
		"targetAlias": "prod", "targetDatabase": "shop", "targetCollection": "x", "action": "dryrun",
	})
	if !strings.Contains(response.Body.String(), "not a file feather can import") {
		t.Errorf("no file type error: %s", truncateForLog(response.Body.String()))
	}
	assertNoImports(t, server)
	assertSpoolEmpty(t, server)
}

// A declared size over the cap is refused before anything is read; a body that
// runs over it without saying so is cut off by the reader. Neither leaves a
// row or a file.
func TestOversizedUploadsAreRejected(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	request := uploadRequest(t, server, session, "orders.ndjson", []byte(`{"_id":1}`+"\n"), map[string]string{
		"targetAlias": "prod", "targetDatabase": "shop", "targetCollection": "x", "action": "dryrun",
	})
	request.ContentLength = maxImportUploadBytes + 1
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/imports/new" {
		t.Errorf("declared oversize: status %d, location %q", response.Code, response.Header().Get("Location"))
	}

	big := bytes.Repeat([]byte(`{"_id":1}`+"\n"), 1000)
	request = uploadRequest(t, server, session, "orders.ndjson", big, map[string]string{
		"targetAlias": "prod", "targetDatabase": "shop", "targetCollection": "x", "action": "dryrun",
	})
	request.ContentLength = -1
	response = httptest.NewRecorder()
	server.limitUpload(1024, server.guard(server.handleImportUpload))(response, request)
	if response.Code == http.StatusSeeOther && strings.HasPrefix(response.Header().Get("Location"), "/imports/") &&
		response.Header().Get("Location") != "/imports/new" {
		t.Error("an over-cap body started an import")
	}

	assertNoImports(t, server)
	assertSpoolEmpty(t, server)
}

func TestImportForRealIsOnlyForCatalogueDryRuns(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := firstSeededArchive(t, server)
	ctx := context.Background()

	upload := finishedImport(t, server, store.ImportSourceUpload, "", store.ImportModeInsert)
	response := post(t, server, "/imports/"+upload.ID.String()+"/run", url.Values{}, session)
	if response.Code != http.StatusSeeOther {
		t.Errorf("status %d", response.Code)
	}

	replace := finishedImport(t, server, store.ImportSourceArchive, artifact.ID.String(), store.ImportModeReplace)
	page := get(t, server, "/imports/"+replace.ID.String(), session).Body.String()
	if !strings.Contains(page, "Import for real") || !strings.Contains(page, `name="password"`) {
		t.Error("a catalogue replace dry run should offer Import for real with a password field")
	}
	post(t, server, "/imports/"+replace.ID.String()+"/run", url.Values{"password": {"wrong"}}, session)

	imports, err := server.app.Store.Imports(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(imports) != 2 {
		t.Errorf("%d imports exist, want the 2 seeded — nothing should have started", len(imports))
	}
	assertNoImportAudit(t, server)
}

func TestImportAuditActionsHaveLabels(t *testing.T) {
	for _, action := range []string{store.AuditImportStarted, store.AuditImportReplaceStarted, store.AuditImportFinished} {
		if auditLabel(action) == action {
			t.Errorf("%s has no readable label", action)
		}
	}
}

func TestEntryPointsLinkToTheImportForm(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedData(t, server)
	artifact := firstSeededArchive(t, server)

	backups := get(t, server, "/backups", session).Body.String()
	if !strings.Contains(backups, "/imports/new?archive="+artifact.ID.String()) {
		t.Error("the backups row does not link to an import")
	}
	run := get(t, server, "/runs/"+artifact.RunID.String(), session).Body.String()
	if !strings.Contains(run, "/imports/new?run="+artifact.RunID.String()) {
		t.Error("the run page does not offer to import the run")
	}
	if !strings.Contains(get(t, server, "/", session).Body.String(), `href="/imports"`) {
		t.Error("Imports is not in the navigation")
	}
}

// --- helpers ---

func firstSeededArchive(t *testing.T, server *Server) store.ArtifactWithCopies {
	t.Helper()
	found, _, err := server.app.Store.Artifacts(context.Background(), store.ArtifactQuery{Limit: 1})
	if err != nil || len(found) == 0 {
		t.Fatalf("no seeded archive: %v", err)
	}
	return found[0]
}

func catalogueImportForm(artifact store.ArtifactWithCopies, action string) url.Values {
	return url.Values{
		"sourceKind": {"archive"}, "sourceRef": {artifact.ID.String()}, "preferAlias": {"cold"},
		"targetAlias": {"prod"}, "targetDatabase": {"shop"}, "targetCollection": {"orders_restored"},
		"mode": {"insert"}, "action": {action},
	}
}

func uploadRequest(t *testing.T, server *Server, session, name string, content []byte, fields map[string]string) *http.Request {
	t.Helper()
	stored, _, err := server.app.Store.SessionByToken(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField(csrfField, stored.CSRFToken)
	for key, value := range fields {
		_ = writer.WriteField(key, value)
	}
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(content)
	_ = writer.Close()

	request := httptest.NewRequest(http.MethodPost, "/imports/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	return request
}

func postUpload(t *testing.T, server *Server, session, name string, content []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, uploadRequest(t, server, session, name, content, fields))
	return response
}

func finishedImport(t *testing.T, server *Server, kind, ref, mode string) *store.Import {
	t.Helper()
	imp := &store.Import{Actor: "ops@example.com", SourceKind: kind, SourceRef: ref, SourceLabel: "seeded",
		TargetAlias: "prod", TargetDatabase: "shop", TargetCollection: "orders_restored", Mode: mode, DryRun: true}
	if err := server.app.Store.StartImport(context.Background(), imp); err != nil {
		t.Fatal(err)
	}
	imp.Status = store.ImportDryRun
	if err := server.app.Store.FinishImport(context.Background(), imp); err != nil {
		t.Fatal(err)
	}
	return imp
}

func assertNoImports(t *testing.T, server *Server) {
	t.Helper()
	imports, err := server.app.Store.Imports(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(imports) != 0 {
		t.Errorf("%d import(s) were recorded, want none", len(imports))
	}
}

func assertNoImportAudit(t *testing.T, server *Server) {
	t.Helper()
	for _, action := range []string{store.AuditImportStarted, store.AuditImportReplaceStarted, store.AuditImportFinished} {
		events, _, err := server.app.Store.AuditEvents(context.Background(), store.AuditQuery{Action: action})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 0 {
			t.Errorf("%s was audited %d time(s), want none", action, len(events))
		}
	}
}

func assertSpoolEmpty(t *testing.T, server *Server) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(server.app.Env.SpoolDir, "imports"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d file(s) left in the import spool", len(entries))
	}
}
