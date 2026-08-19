package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/mongox"
	"github.com/unifers/feather/internal/secrets"
	"github.com/unifers/feather/internal/store"
)

// The rig runs the real pipeline against fake edges: an in-memory collection
// and in-memory destinations, with a real SQLite catalogue and real spool files
// on disk. Everything between the two edges — batching, compression, hashing,
// the upload fan-out, the verify gate, the delete decision, the catalogue
// writes — is the production code.

type rig struct {
	runner       *Runner
	store        *store.Store
	job          *config.JobConfig
	source       *fakeSource
	destinations map[string]*fakeDestination
}

func newRig(t *testing.T, documents int) *rig {
	t.Helper()

	key, err := secrets.GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.Resolve(key, "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "feather.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	destinations := map[string]*fakeDestination{
		"primary": newFakeDestination("primary"),
		"backup":  newFakeDestination("backup"),
	}
	byAlias := map[string]dest.Destination{}
	for alias, destination := range destinations {
		byAlias[alias] = destination
	}

	env := &config.Env{SpoolDir: t.TempDir()}
	source := newFakeSource(documents)

	runner := New(env, nil, st, dest.NewRegistryWith(byAlias), discardLog())
	runner.sourceFor = func(context.Context, *config.JobConfig) (mongox.Source, error) {
		return source, nil
	}
	// Retries would turn every failure case into a multi-second test without
	// changing what is being asserted.
	runner.retry.Attempts = 1

	verify := true
	job := &config.JobConfig{
		Name: "orders-archive",
		Source: config.Source{
			MongoSource: "primary", Database: "shop", Collection: "orders",
			Filter: json.RawMessage(`{"placedAt":{"$lt":{"$date":"2026-01-01T00:00:00Z"}}}`),
		},
		Destinations: []string{"primary", "backup"},
		Output:       config.Output{KeyTemplate: "archive/{db}/{collection}/{config}-{runId}-{seq}{ext}"},
		Compression:  config.Compression{Codec: config.CodecGzip},
		Schedule:     config.Schedule{Every: "24h"},
		Retention:    config.Retention{VerifyBeforeDelete: &verify},
	}
	if err := job.Prepare(); err != nil {
		t.Fatalf("preparing the test config: %v", err)
	}

	return &rig{runner: runner, store: st, job: job, source: source, destinations: destinations}
}

// --- a collection, in memory ---

type fakeSource struct {
	docs []mongox.Document

	mu          sync.Mutex
	deleted     int64
	deleteCalls int
	offeredIDs  []bson.RawValue
}

func newFakeSource(count int) *fakeSource {
	docs := make([]mongox.Document, 0, count)
	for i := range count {
		encoded, err := bson.Marshal(bson.D{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "seq", Value: int32(i)},
			{Key: "payload", Value: strings.Repeat("x", 300)},
		})
		if err != nil {
			panic(err)
		}
		raw := bson.Raw(encoded)
		id, err := raw.LookupErr("_id")
		if err != nil {
			panic(err)
		}
		docs = append(docs, mongox.Document{Raw: raw, ID: id})
	}
	return &fakeSource{docs: docs}
}

func (f *fakeSource) MaxID(context.Context, bson.D) (bson.RawValue, bool, error) {
	if len(f.docs) == 0 {
		return bson.RawValue{}, false, nil
	}
	return f.docs[len(f.docs)-1].ID, true, nil
}

func (f *fakeSource) Count(context.Context, bson.D) (int64, error) {
	return int64(len(f.docs)), nil
}

func (f *fakeSource) OpenCursor(context.Context, bson.D, bson.D, int32, string) (mongox.Stream, error) {
	return &fakeStream{docs: f.docs}, nil
}

func (f *fakeSource) DeleteByIDs(_ context.Context, _ *slog.Logger, _ bson.D, ids []bson.RawValue, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	f.offeredIDs = append(f.offeredIDs, ids...)
	f.deleted += int64(len(ids))
	return int64(len(ids)), nil
}

type fakeStream struct {
	docs []mongox.Document
	at   int
}

func (f *fakeStream) Next(context.Context) (mongox.Document, bool, error) {
	if f.at >= len(f.docs) {
		return mongox.Document{}, false, nil
	}
	doc := f.docs[f.at]
	f.at++
	return doc, true, nil
}

func (f *fakeStream) Close(context.Context) error { return nil }

// --- a destination, in memory ---

type fakeDestination struct {
	alias string

	// Failure switches, set by a test before the run.
	uploadErr error
	verifyErr error
	corrupt   bool // store bytes that are not what was sent
	vanish    bool // lose the object between upload and verify

	mu       sync.Mutex
	objects  map[string][]byte
	uploaded int
	verified int
}

func newFakeDestination(alias string) *fakeDestination {
	return &fakeDestination{alias: alias, objects: map[string][]byte{}}
}

func (f *fakeDestination) Alias() string { return f.alias }
func (f *fakeDestination) Info() dest.Info {
	return dest.Info{Alias: f.alias, Bucket: f.alias + "-bucket"}
}
func (f *fakeDestination) Check(context.Context) error { return nil }
func (f *fakeDestination) Key(key string) string       { return key }

func (f *fakeDestination) Upload(_ context.Context, key string, body io.Reader, _ int64, _ dest.Metadata) (string, error) {
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	stored, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	if f.corrupt && len(stored) > 0 {
		stored = append([]byte(nil), stored...)
		stored[len(stored)/2] ^= 0x01
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.vanish {
		f.objects[key] = stored
	}
	f.uploaded++
	return "handle-" + key, nil
}

func (f *fakeDestination) Open(_ context.Context, key, _ string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s is not in %s", key, f.alias)
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

// Verify goes through the same helper the real destinations use, so these tests
// exercise the production verifier rather than a stand-in for it.
func (f *fakeDestination) Verify(ctx context.Context, obj dest.StoredObject) error {
	if f.verifyErr != nil {
		return f.verifyErr
	}
	f.mu.Lock()
	f.verified++
	f.mu.Unlock()
	return dest.VerifyByReading(ctx, f, obj)
}

func (f *fakeDestination) stored() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.objects))
	for key, body := range f.objects {
		out[key] = body
	}
	return out
}

func (f *fakeDestination) uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploaded
}

func (f *fakeDestination) verifies() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verified
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var _ = errors.New
