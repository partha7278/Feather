package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/mongox"
	"github.com/unifers/feather/internal/store"
)

// The reason this package exists: an archive that has rotted since it was
// written must not restore silently. Verification at write time proves nothing
// about the object months later.
func TestACorruptedCopyIsReportedAsCorruptNotMissing(t *testing.T) {
	written := buildArchive(t, 25)
	damaged := append([]byte(nil), written.bytes...)
	damaged[len(damaged)/2] ^= 0x01

	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: damaged}}
	result := VerifyArtifact(t.Context(), discardLog(), registry, written.artifact, written.copies)

	if result.Intact() {
		t.Fatal("a corrupted copy verified as intact")
	}
	problems := result.Problems()
	if len(problems) != 1 {
		t.Fatalf("expected one problem, got %d", len(problems))
	}
	if problems[0].Status != CopyCorrupt {
		t.Errorf("status is %q, want %q", problems[0].Status, CopyCorrupt)
	}
}

func TestAMissingCopyIsReportedAsMissing(t *testing.T) {
	written := buildArchive(t, 5)
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", openErr: errors.New("NoSuchKey: the object does not exist")}}

	result := VerifyArtifact(t.Context(), discardLog(), registry, written.artifact, written.copies)
	if result.Intact() {
		t.Fatal("a missing copy verified as intact")
	}
	if got := result.Problems()[0].Status; got != CopyMissing {
		t.Errorf("status is %q, want %q", got, CopyMissing)
	}
}

// A destination that has since been removed from the config must never be
// counted as a pass — feather cannot see it, which is not the same as fine.
func TestAnUnknownDestinationIsSkippedNotPassed(t *testing.T) {
	written := buildArchive(t, 3)
	result := VerifyArtifact(t.Context(), discardLog(), fakeRegistry{}, written.artifact, written.copies)

	if result.Intact() {
		t.Fatal("an uncheckable copy counted as intact")
	}
	if got := result.Problems()[0].Status; got != CopySkipped {
		t.Errorf("status is %q, want %q", got, CopySkipped)
	}
}

// Several copies exist so they can fail independently. One good copy means the
// data is still there, and every copy is still checked so the operator learns
// which ones need repairing.
func TestOneGoodCopyMeansIntactAndEveryCopyIsStillChecked(t *testing.T) {
	written := buildArchive(t, 12)
	damaged := append([]byte(nil), written.bytes...)
	damaged[len(damaged)/2] ^= 0x01

	registry := fakeRegistry{
		"good": &fakeDestination{alias: "good", object: written.bytes},
		"bad":  &fakeDestination{alias: "bad", object: damaged},
	}
	copies := []store.ArtifactDestination{
		{Alias: "good", ObjectKey: "k"},
		{Alias: "bad", ObjectKey: "k"},
	}

	result := VerifyArtifact(t.Context(), discardLog(), registry, written.artifact, copies)
	if !result.Intact() {
		t.Fatal("a set with one good copy reported as not intact")
	}
	if len(result.Copies) != 2 {
		t.Fatalf("checked %d copies, want 2", len(result.Copies))
	}
	if len(result.Problems()) != 1 {
		t.Errorf("expected the damaged copy to be reported, got %d problems", len(result.Problems()))
	}
}

// A dry run must read and decode the whole archive — that is what makes it
// worth running before the real one — while writing nothing.
func TestDryRunReadsEveryDocumentAndWritesNothing(t *testing.T) {
	written := buildArchive(t, 40)
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: written.bytes}}

	result, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, written.copies,
		Options{DryRun: true})
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if result.DocumentsRead != 40 {
		t.Errorf("read %d documents, want 40", result.DocumentsRead)
	}
	if !result.DryRun {
		t.Error("result does not record that it was a dry run")
	}
}

// The digest is checked on the way past, so restoring from a rotted copy fails
// rather than quietly returning wrong data.
func TestRestoreRefusesAnArchiveThatDoesNotMatchTheCatalogue(t *testing.T) {
	written := buildArchive(t, 10)
	written.artifact.SHA256 = strings.Repeat("0", 64)
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: written.bytes}}

	_, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, written.copies,
		Options{DryRun: true})
	if err == nil {
		t.Fatal("expected a digest mismatch to fail the restore")
	}
	if !strings.Contains(err.Error(), "not the archive it claims to be") {
		t.Errorf("error does not explain the mismatch: %v", err)
	}
}

// When every copy is unreadable the operator needs to see why each one failed,
// not a single generic message.
func TestUnreadableCopiesAreAllNamed(t *testing.T) {
	written := buildArchive(t, 2)
	registry := fakeRegistry{
		"one": &fakeDestination{alias: "one", openErr: errors.New("connection refused")},
		"two": &fakeDestination{alias: "two", openErr: errors.New("access denied")},
	}
	copies := []store.ArtifactDestination{{Alias: "one", ObjectKey: "k"}, {Alias: "two", ObjectKey: "k"}}

	_, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, copies, Options{DryRun: true})
	if err == nil {
		t.Fatal("expected a failure when no copy can be read")
	}
	for _, want := range []string{"one", "two", "connection refused", "access denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestPreferAliasPicksThatCopy(t *testing.T) {
	written := buildArchive(t, 6)
	registry := fakeRegistry{
		"first":  &fakeDestination{alias: "first", object: written.bytes},
		"second": &fakeDestination{alias: "second", object: written.bytes},
	}
	copies := []store.ArtifactDestination{{Alias: "first", ObjectKey: "k"}, {Alias: "second", ObjectKey: "k"}}

	result, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, copies,
		Options{DryRun: true, PreferAlias: "second"})
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if result.ReadFrom != "second" {
		t.Errorf("read from %q, want %q", result.ReadFrom, "second")
	}
}

// --- fakes and fixtures ---

type builtArchive struct {
	bytes    []byte
	artifact store.Artifact
	copies   []store.ArtifactDestination
}

func buildArchive(t *testing.T, docs int) builtArchive {
	t.Helper()

	spool, err := archive.NewSpool(t.TempDir(), "test", config.Compression{Codec: config.CodecGzip})
	if err != nil {
		t.Fatalf("creating a spool: %v", err)
	}
	for i := range docs {
		encoded, err := bson.Marshal(bson.D{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "n", Value: int32(i)},
			{Key: "padding", Value: strings.Repeat("x", 200)},
		})
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		id, _ := bson.Raw(encoded).LookupErr("_id")
		if err := spool.Add(mongox.Document{Raw: encoded, ID: id}); err != nil {
			t.Fatalf("adding: %v", err)
		}
	}
	result, err := spool.Finish("orders-00001.ndjson.gz")
	if err != nil {
		t.Fatalf("finishing: %v", err)
	}
	t.Cleanup(func() { _ = spool.Discard() })

	stored, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("reading the spool file: %v", err)
	}

	return builtArchive{
		bytes: stored,
		artifact: store.Artifact{
			FileName:        result.FileName,
			Codec:           config.CodecGzip,
			DocCount:        result.DocCount,
			BytesCompressed: result.BytesCompressed,
			SHA256:          result.SHA256,
			Database:        "shop",
			Collection:      "orders",
		},
		copies: []store.ArtifactDestination{{Alias: "cold", ObjectKey: "archive/orders-00001.ndjson.gz"}},
	}
}

type fakeRegistry map[string]dest.Destination

func (f fakeRegistry) Get(alias string) (dest.Destination, error) {
	target, ok := f[alias]
	if !ok {
		return nil, errors.New("no destination named " + alias)
	}
	return target, nil
}

// fakeDestination serves a fixed object, and verifies the way a real one does:
// by reading the bytes back and hashing them.
type fakeDestination struct {
	alias   string
	object  []byte
	openErr error
}

func (f *fakeDestination) Alias() string               { return f.alias }
func (f *fakeDestination) Info() dest.Info             { return dest.Info{Alias: f.alias} }
func (f *fakeDestination) Check(context.Context) error { return nil }
func (f *fakeDestination) Key(key string) string       { return key }

func (f *fakeDestination) Upload(context.Context, string, io.Reader, int64, dest.Metadata) (string, error) {
	return "handle", nil
}

func (f *fakeDestination) Open(context.Context, string, string) (io.ReadCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(bytes.NewReader(f.object)), nil
}

// Verify goes through the same helper the real destinations use, so these
// tests exercise the actual verifier rather than a stand-in for it.
func (f *fakeDestination) Verify(ctx context.Context, obj dest.StoredObject) error {
	return dest.VerifyByReading(ctx, f, obj)
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
