//go:build integration

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/store"
)

// An import from the catalogue into a collection that already holds half the
// documents adds only the missing half and leaves the rest as they are. A
// replace then puts every archived version back.
func TestImportAddsWhatIsMissingAndReplaceOverwrites(t *testing.T) {
	env := newEnv(t)
	const documents = 1000

	originals := env.seed(t, documents)
	job := env.config(t, "orders-import", false)
	outcome, err := env.app.Runner.Run(t.Context(), job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Half the documents are already in the target, one of them edited since.
	target := env.mongo.Database(testDatabase).Collection("orders_restored")
	var half []any
	var edited any
	for _, raw := range originals {
		if len(half) == documents/2 {
			break
		}
		half = append(half, raw)
	}
	if _, err := target.InsertMany(t.Context(), half); err != nil {
		t.Fatal(err)
	}
	edited = half[0].(bson.Raw).Lookup("_id")
	if _, err := target.UpdateOne(t.Context(), bson.D{{Key: "_id", Value: edited}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "note", Value: "edited"}}}}); err != nil {
		t.Fatal(err)
	}

	request := app.ImportRequest{
		Actor: "e2e", SourceKind: store.ImportSourceRun, SourceRef: outcome.RunID.String(),
		TargetAlias: "rig", TargetDatabase: testDatabase, TargetCollection: "orders_restored",
		Mode: store.ImportModeInsert,
	}
	imp := runImport(t, env.app, request)
	if imp.Status != store.ImportSuccess {
		t.Fatalf("insert import: %s %s", imp.Status, imp.Error)
	}
	if imp.DocsInserted != documents/2 || imp.DocsPresent != documents/2 {
		t.Errorf("inserted %d and found %d present, want %d each", imp.DocsInserted, imp.DocsPresent, documents/2)
	}
	if note := env.readAll(t, "orders_restored")[edited.(bson.RawValue).String()].Lookup("note").StringValue(); note != "edited" {
		t.Errorf("insert mode overwrote an existing document (note is %q)", note)
	}

	request.Mode = store.ImportModeReplace
	imp = runImport(t, env.app, request)
	if imp.Status != store.ImportSuccess {
		t.Fatalf("replace import: %s %s", imp.Status, imp.Error)
	}
	if imp.DocsReplaced != documents || imp.DocsInserted != 0 {
		t.Errorf("replaced %d and inserted %d, want %d and 0", imp.DocsReplaced, imp.DocsInserted, documents)
	}
	restored := env.readAll(t, "orders_restored")
	for id, original := range originals {
		if !bytes.Equal(restored[id], original) {
			t.Errorf("document %s is not the archived version after a replace", id)
		}
	}
}

// A plain NDJSON upload, in the relaxed form mongoexport writes, streams in.
func TestImportStreamsAPlainNDJSONUpload(t *testing.T) {
	env := newEnv(t)

	var body bytes.Buffer
	for i := range 50 {
		line, err := bson.MarshalExtJSON(bson.D{{Key: "_id", Value: int32(i)}, {Key: "n", Value: int32(i)}}, false, false)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	dir := filepath.Join(env.app.Env.SpoolDir, "imports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "upload-e2e.tmp")
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	imp := runImport(t, env.app, app.ImportRequest{
		Actor: "e2e", SourceKind: store.ImportSourceUpload,
		UploadPath: path, UploadName: "orders.ndjson", UploadCodec: archive.CodecNone,
		TargetAlias: "rig", TargetDatabase: testDatabase, TargetCollection: "orders_restored",
		Mode: store.ImportModeInsert,
	})
	if imp.Status != store.ImportSuccess || imp.DocsInserted != 50 {
		t.Fatalf("upload import: %s, inserted %d: %s", imp.Status, imp.DocsInserted, imp.Error)
	}
	if got := len(env.readAll(t, "orders_restored")); got != 50 {
		t.Errorf("collection holds %d documents, want 50", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the upload was not removed")
	}
}

func runImport(t *testing.T, a *app.App, request app.ImportRequest) store.Import {
	t.Helper()
	id, err := a.StartImport(t.Context(), request)
	if err != nil {
		t.Fatalf("starting the import: %v", err)
	}
	return waitForImport(t, a, id)
}

func waitForImport(t *testing.T, a *app.App, id uuid.UUID) store.Import {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		imp, err := a.Store.ImportByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if imp.FinishedAt != nil {
			// The slot is released just after the row closes.
			a.Drain(context.Background())
			return imp
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the import did not finish")
	return store.Import{}
}
