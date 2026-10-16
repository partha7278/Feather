package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

// One import at a time, and a refused upload must not leave its file behind.
func TestASecondImportIsRefusedWhileOneIsRunning(t *testing.T) {
	a := testApp(t)
	path := writeUpload(t, a, `{"_id":1}`+"\n")

	a.mu.Lock()
	a.importing = true
	a.mu.Unlock()

	_, err := a.StartImport(context.Background(), uploadRequest(path))
	if !errors.Is(err, ErrImportBusy) {
		t.Fatalf("got %v, want ErrImportBusy", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("the refused upload was left in the spool directory")
	}
	if imports, _ := a.Store.Imports(context.Background(), 10); len(imports) != 0 {
		t.Errorf("a refused import wrote %d rows", len(imports))
	}
}

// A dry run of an upload reads everything, writes no documents, records its
// counts and digest, audits its end, removes the file, and frees the slot.
func TestAnUploadDryRunRecordsItsCountsAndCleansUp(t *testing.T) {
	a := testApp(t)
	path := writeUpload(t, a, `{"_id":1,"n":1}`+"\n"+`{"_id":2,"n":2}`+"\n"+`{"_id":3,"n":3}`+"\n")

	id, err := a.StartImport(context.Background(), uploadRequest(path))
	if err != nil {
		t.Fatal(err)
	}
	imp := waitForImport(t, a, id)

	if imp.Status != store.ImportDryRun || imp.Error != "" {
		t.Fatalf("status %s, error %q", imp.Status, imp.Error)
	}
	if imp.DocsRead != 3 || imp.DocsInserted != 3 || imp.FilesDone != 1 || imp.SHA256 == "" {
		t.Errorf("counts not recorded: %+v", imp)
	}
	if imp.BytesRead != imp.BytesExpected || imp.BytesExpected == 0 {
		t.Errorf("read %d of %d bytes", imp.BytesRead, imp.BytesExpected)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("the upload was not removed after the import")
	}

	// The slot is released after the row is closed; Drain is how a caller
	// waits for that.
	a.Drain(context.Background())
	if a.ImportRunning() {
		t.Error("the import slot was not released")
	}

	events, _, err := a.Store.AuditEvents(context.Background(), store.AuditQuery{Action: store.AuditImportFinished})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Actor != "ops@example.com" || events[0].ActorIP != "10.0.0.9" {
		t.Errorf("finish not audited with the starter's identity: %+v", events)
	}
}

func TestAnInvalidUploadFailsWithTheLineNumber(t *testing.T) {
	a := testApp(t)
	path := writeUpload(t, a, `{"_id":1}`+"\nnot json\n")

	id, err := a.StartImport(context.Background(), uploadRequest(path))
	if err != nil {
		t.Fatal(err)
	}
	imp := waitForImport(t, a, id)
	if imp.Status != store.ImportFailed || imp.DocsRead != 1 {
		t.Errorf("status %s after reading %d", imp.Status, imp.DocsRead)
	}
	if imp.Error == "" {
		t.Error("the failure has no message")
	}
}

func TestAnUnknownModeIsRefused(t *testing.T) {
	a := testApp(t)
	request := uploadRequest(writeUpload(t, a, "{}\n"))
	request.Mode = "merge"
	if _, err := a.StartImport(context.Background(), request); err == nil {
		t.Fatal("expected an unknown mode to be refused")
	}
	if a.ImportRunning() {
		t.Error("a refused import kept the slot")
	}
}

func TestHousekeepingClosesImportsLeftRunning(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	imp := &store.Import{Actor: "a", SourceKind: store.ImportSourceUpload, TargetAlias: "p",
		TargetDatabase: "d", TargetCollection: "c", Mode: store.ImportModeInsert}
	if err := a.Store.StartImport(ctx, imp); err != nil {
		t.Fatal(err)
	}

	a.Housekeeping(ctx)

	got, err := a.Store.ImportByID(ctx, imp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ImportFailed || got.Error != "interrupted by a restart" {
		t.Errorf("left-over import not closed: %+v", got)
	}
}

// --- helpers ---

func testApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	env := &config.Env{
		DataDir:           dir,
		DBPath:            filepath.Join(dir, "feather.db"),
		MasterKeyPath:     filepath.Join(dir, "master.key"),
		SpoolDir:          filepath.Join(dir, "spool"),
		MaxConcurrentJobs: 1,
	}
	a, err := Open(context.Background(), env, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

func writeUpload(t *testing.T, a *App, body string) string {
	t.Helper()
	dir := filepath.Join(a.Env.SpoolDir, "imports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "upload-test.tmp")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func uploadRequest(path string) ImportRequest {
	return ImportRequest{
		Actor: "ops@example.com", ActorIP: "10.0.0.9",
		SourceKind: store.ImportSourceUpload, UploadPath: path, UploadName: "orders.ndjson",
		UploadCodec: archive.CodecNone,
		TargetAlias: "prod", TargetDatabase: "shop", TargetCollection: "orders_restored",
		Mode: store.ImportModeInsert, DryRun: true,
	}
}

func waitForImport(t *testing.T, a *App, id uuid.UUID) store.Import {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		imp, err := a.Store.ImportByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if imp.FinishedAt != nil {
			return imp
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the import did not finish")
	return store.Import{}
}
