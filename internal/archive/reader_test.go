package archive

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/mongox"
)

// The whole point of the archive format is that what comes back out is what
// went in — same types, same values. Until now nothing ever read one back, so
// nothing checked.
func TestArchiveRoundTripsEveryBSONTypeThatMatters(t *testing.T) {
	for _, codec := range []string{config.CodecGzip, config.CodecZstd} {
		t.Run(codec, func(t *testing.T) {
			originals := sampleDocuments(t)

			spool := newSpool(t, codec)
			for _, doc := range originals {
				if err := spool.Add(mongoDoc(doc)); err != nil {
					t.Fatalf("adding a document: %v", err)
				}
			}
			result, err := spool.Finish("orders-00001.ndjson")
			if err != nil {
				t.Fatalf("finishing the spool: %v", err)
			}

			reader, err := NewDocumentReader(bytes.NewReader(readFile(t, result.Path)), codec)
			if err != nil {
				t.Fatalf("opening the archive: %v", err)
			}
			defer func() { _ = reader.Close() }()

			var got []bson.Raw
			for {
				doc, ok := reader.Next()
				if !ok {
					break
				}
				got = append(got, doc)
			}
			if err := reader.Err(); err != nil {
				t.Fatalf("reading the archive: %v", err)
			}
			if err := reader.Drain(); err != nil {
				t.Fatalf("draining: %v", err)
			}

			if len(got) != len(originals) {
				t.Fatalf("archive holds %d documents, wrote %d", len(got), len(originals))
			}
			for i := range got {
				if !bytes.Equal(got[i], originals[i]) {
					t.Errorf("document %d did not survive the round trip:\n  wrote %s\n  read  %s",
						i, originals[i].String(), got[i].String())
				}
			}

			// The digest the reader computes has to be the one the writer
			// recorded, or a restore cannot tell a good copy from a bad one.
			if !strings.EqualFold(reader.SHA256(), result.SHA256) {
				t.Errorf("reader hashed %s, spool recorded %s", reader.SHA256(), result.SHA256)
			}
			if reader.Count() != result.DocCount {
				t.Errorf("reader counted %d documents, spool recorded %d", reader.Count(), result.DocCount)
			}
		})
	}
}

// A corrupted archive has to be detectable, because that is the only thing
// standing between bit-rot and a restore that silently returns wrong data.
func TestCorruptedBytesChangeTheDigest(t *testing.T) {
	spool := newSpool(t, config.CodecGzip)
	for _, doc := range sampleDocuments(t) {
		if err := spool.Add(mongoDoc(doc)); err != nil {
			t.Fatalf("adding: %v", err)
		}
	}
	result, err := spool.Finish("orders-00001.ndjson")
	if err != nil {
		t.Fatalf("finishing: %v", err)
	}

	stored := readFile(t, result.Path)
	// Flip a bit deep in the compressed stream, past the header.
	stored[len(stored)/2] ^= 0x01

	reader, err := NewDocumentReader(bytes.NewReader(stored), config.CodecGzip)
	if err != nil {
		// gzip refusing to open a mangled stream is also a detection.
		return
	}
	defer func() { _ = reader.Close() }()
	for {
		if _, ok := reader.Next(); !ok {
			break
		}
	}
	_ = reader.Drain()

	if reader.Err() == nil && strings.EqualFold(reader.SHA256(), result.SHA256) {
		t.Fatal("a corrupted archive read back clean and hashed to the original digest")
	}
}

func TestUnknownCodecIsRefused(t *testing.T) {
	if _, err := NewDocumentReader(bytes.NewReader(nil), "brotli"); err == nil {
		t.Fatal("expected an unknown codec to be refused")
	}
}

func TestCodecForFileName(t *testing.T) {
	for name, want := range map[string]string{
		"orders-00001.ndjson.gz":  config.CodecGzip,
		"orders-00001.ndjson.zst": config.CodecZstd,
		"orders-00001.ndjson":     config.CodecGzip,
	} {
		if got := CodecForFileName(name); got != want {
			t.Errorf("%s: got codec %q, want %q", name, got, want)
		}
	}
}

// --- helpers ---

func newSpool(t *testing.T, codec string) *Spool {
	t.Helper()
	spool, err := NewSpool(t.TempDir(), "test", config.Compression{Codec: codec})
	if err != nil {
		t.Fatalf("creating a spool: %v", err)
	}
	t.Cleanup(func() { _ = spool.Discard() })
	return spool
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func mongoDoc(raw bson.Raw) mongox.Document {
	id, _ := raw.LookupErr("_id")
	return mongox.Document{Raw: raw, ID: id}
}

// sampleDocuments covers the types the README promises survive a round trip:
// ObjectId, Date, Decimal128, and int32 as distinct from int64.
func sampleDocuments(t *testing.T) []bson.Raw {
	t.Helper()

	decimal, err := bson.ParseDecimal128("1234.5678")
	if err != nil {
		t.Fatalf("parsing a decimal: %v", err)
	}
	when := time.Date(2026, 3, 14, 15, 9, 26, 535000000, time.UTC)

	docs := []bson.D{
		{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "placedAt", Value: when},
			{Key: "total", Value: decimal},
			{Key: "quantity", Value: int32(7)},
			{Key: "customerId", Value: int64(9007199254740993)},
			{Key: "note", Value: "unicode: café — ☕"},
		},
		{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "nested", Value: bson.D{{Key: "tags", Value: bson.A{"a", int32(1), true}}}},
			{Key: "missing", Value: nil},
			{Key: "raw", Value: bson.Binary{Subtype: 0x00, Data: []byte{0xde, 0xad, 0xbe, 0xef}}},
		},
	}

	out := make([]bson.Raw, 0, len(docs))
	for _, doc := range docs {
		encoded, err := bson.Marshal(doc)
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		out = append(out, encoded)
	}
	return out
}

// The spool's contract is that a batch is bounded by disk, not by RAM. Holding
// each document's _id as it arrived quietly broke that: a bson.RawValue's bytes
// are a subslice of the whole document, and Go cannot free a backing array
// while any subslice of it is reachable, so retaining the id retained the
// document. At the default 256 MB batch across two concurrent jobs that is an
// out-of-memory kill on the VPS this is meant to run on.
//
// This asserts the heap stays proportional to the ids rather than to the
// documents they came from.
func TestSpoolingLargeDocumentsDoesNotRetainThem(t *testing.T) {
	const (
		documents   = 4000
		payloadSize = 8 << 10 // 8 KiB each: ~32 MB of documents in total
	)

	spool := newSpool(t, config.CodecGzip)
	payload := strings.Repeat("x", payloadSize)

	for range documents {
		encoded, err := bson.Marshal(bson.D{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "payload", Value: payload},
		})
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		raw := bson.Raw(encoded)
		id, err := raw.LookupErr("_id")
		if err != nil {
			t.Fatalf("looking up _id: %v", err)
		}
		if err := spool.Add(mongox.Document{Raw: raw, ID: id}); err != nil {
			t.Fatalf("adding: %v", err)
		}
	}

	if _, err := spool.Finish("large.ndjson.gz"); err != nil {
		t.Fatalf("finishing: %v", err)
	}

	// The ids are still held — the delete path needs them — so the spool is
	// kept alive across the measurement on purpose. What must not be held is
	// the documents they were carved out of.
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	retained := stats.HeapAlloc
	runtime.KeepAlive(spool)

	const written = documents * payloadSize
	// Generous: the ids themselves are ~12 bytes each, and the test binary and
	// compressor buffers account for a few MB. Retaining the documents would
	// put this above 32 MB, so the bound catches the regression without being
	// brittle about allocator behaviour.
	const bound = written / 4

	if retained > bound {
		t.Errorf("spooling %s of documents left %s on the heap (bound %s) — "+
			"the _id values are pinning the documents they came from",
			config.HumanBytes(written), config.HumanBytes(int64(retained)), config.HumanBytes(bound))
	}
	if got := len(spool.IDs()); got != documents {
		t.Errorf("spool holds %d ids, want %d", got, documents)
	}
}
