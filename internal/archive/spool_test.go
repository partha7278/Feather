package archive

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/mongox"
)

// The spool's output is the archive. Everything downstream — the checksum the
// catalogue records, the bytes uploaded, the copy a restore reads back — is
// whatever this file produced, so these tests check the file itself rather than
// the counters describing it.

func TestSpoolRoundTripsDocumentsAndChecksums(t *testing.T) {
	spool := newTestSpool(t, config.Compression{Codec: config.CodecGzip, Level: 6})

	const count = 500
	want := make([]bson.M, 0, count)
	for i := range count {
		doc := bson.M{
			"_id":   bson.NewObjectID(),
			"seq":   int32(i),
			"body":  strings.Repeat("payload ", 20),
			"ratio": 1.5,
		}
		want = append(want, doc)
		if err := spool.Add(mustDocument(t, doc)); err != nil {
			t.Fatalf("adding document %d: %v", i, err)
		}
	}

	result, err := spool.Finish("orders-archive-00001.ndjson.gz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Discard() })

	if result.DocCount != count {
		t.Errorf("spool counted %d documents, wrote %d", result.DocCount, count)
	}

	// The recorded checksum has to cover exactly the bytes on disk. If it does
	// not, every verification downstream is comparing against a fiction.
	onDisk, err := os.Open(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = onDisk.Close() }()

	hasher := sha256.New()
	size, err := io.Copy(hasher, onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != result.SHA256 {
		t.Errorf("the file hashes to %s but the catalogue records %s", got, result.SHA256)
	}
	if size != result.BytesCompressed {
		t.Errorf("the file is %d bytes but the catalogue records %d", size, result.BytesCompressed)
	}

	// And the content has to be readable NDJSON that decodes back to the same
	// documents — that is the restore path, exercised here without a network.
	if _, err := onDisk.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	unzipped, err := gzip.NewReader(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unzipped.Close() }()

	lines := bufio.NewScanner(unzipped)
	lines.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for i := 0; lines.Scan(); i++ {
		if i >= len(want) {
			t.Fatalf("the archive holds more than the %d documents written", len(want))
		}
		var got bson.M
		if err := bson.UnmarshalExtJSON(lines.Bytes(), true, &got); err != nil {
			t.Fatalf("line %d is not canonical Extended JSON: %v", i, err)
		}
		// int32 must not have become a float on the way through, which is the
		// failure canonical mode exists to prevent.
		if got["seq"] != int32(i) {
			t.Fatalf("document %d came back with seq %#v (%T)", i, got["seq"], got["seq"])
		}
		if got["_id"] != want[i]["_id"] {
			t.Fatalf("document %d came back with _id %v", i, got["_id"])
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
}

// A batch is supposed to be bounded by disk, not RAM — the package comment says
// so. It was not: Add retained each document's _id as a bson.RawValue, whose
// Value field is a subslice of that document's buffer, so keeping the id kept
// the whole document. A batch of 256 MB of documents held 256 MB of heap.
//
// This test writes documents far larger than their ids and asserts the heap
// does not grow with the payload. It fails without copyRawValue.
func TestSpoolDoesNotRetainDocumentBodies(t *testing.T) {
	spool := newTestSpool(t, config.Compression{Codec: config.CodecGzip, Level: 1})
	t.Cleanup(func() { _ = spool.Discard() })

	const (
		docs        = 2000
		payloadSize = 32 << 10 // 64 MB of payload in total
	)
	payload := strings.Repeat("x", payloadSize)

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	for range docs {
		doc := mustDocument(t, bson.M{"_id": bson.NewObjectID(), "body": payload})
		if err := spool.Add(doc); err != nil {
			t.Fatal(err)
		}
	}

	// The ids are still live — they are what the delete step needs — so this
	// measures exactly what the spool is holding on to at the point where it
	// matters.
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	if len(spool.IDs()) != docs {
		t.Fatalf("the spool kept %d ids, not %d", len(spool.IDs()), docs)
	}

	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	written := int64(docs) * int64(payloadSize)

	// 2000 ObjectIDs are ~24 KB of value bytes plus slice overhead. Anything
	// close to the 64 MB of payload written means the documents are still
	// being held; a few MB of slack keeps the test from being flaky.
	const budget = 8 << 20
	if retained > budget {
		t.Errorf("the spool retained %d bytes of heap after writing %d bytes of documents; "+
			"it should be holding ids only (budget %d bytes)", retained, written, budget)
	}
}

func TestSpoolRecordsTheIDRangeInCursorOrder(t *testing.T) {
	spool := newTestSpool(t, config.Compression{Codec: config.CodecZstd, Level: 1})
	t.Cleanup(func() { _ = spool.Discard() })

	first := bson.NewObjectID()
	last := bson.NewObjectID()
	for _, id := range []bson.ObjectID{first, bson.NewObjectID(), last} {
		if err := spool.Add(mustDocument(t, bson.M{"_id": id})); err != nil {
			t.Fatal(err)
		}
	}

	result, err := spool.Finish("orders-archive-00001.ndjson.gz")
	if err != nil {
		t.Fatal(err)
	}

	// min and max are first-seen and last-seen, which is only the true range
	// because the cursor reads in _id order. Pinning it here means a change to
	// the read order fails a test rather than silently mislabelling archives.
	if result.MinID != first.Hex() {
		t.Errorf("min id is %s, want %s", result.MinID, first.Hex())
	}
	if result.MaxID != last.Hex() {
		t.Errorf("max id is %s, want %s", result.MaxID, last.Hex())
	}
}

func newTestSpool(t *testing.T, comp config.Compression) *Spool {
	t.Helper()
	spool, err := NewSpool(t.TempDir(), "test-archive", comp)
	if err != nil {
		t.Fatal(err)
	}
	return spool
}

func mustDocument(t *testing.T, doc bson.M) mongox.Document {
	t.Helper()

	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	id, err := bson.Raw(raw).LookupErr("_id")
	if err != nil {
		t.Fatal(err)
	}
	return mongox.Document{Raw: raw, ID: id}
}
