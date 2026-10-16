package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
)

// The runner decides when to cut a batch from DocCount and RawBytes, and skips
// an upload entirely when IsEmpty. Each has to track what Add actually wrote.
func TestSpoolCountsWhatItHasBeenGiven(t *testing.T) {
	spool := newTestSpool(t, config.Compression{Codec: config.CodecGzip, Level: 6})
	t.Cleanup(func() { _ = spool.Discard() })

	if !spool.IsEmpty() || spool.DocCount() != 0 || spool.RawBytes() != 0 {
		t.Fatalf("a new spool is not empty: empty=%v docs=%d raw=%d",
			spool.IsEmpty(), spool.DocCount(), spool.RawBytes())
	}

	var wantRaw int64
	for i := range 3 {
		doc := mustDocument(t, bson.M{"_id": bson.NewObjectID(), "seq": int32(i), "body": strings.Repeat("y", i*40)})
		line, err := bson.MarshalExtJSON(doc.Raw, true, false)
		if err != nil {
			t.Fatal(err)
		}
		wantRaw += int64(len(line)) + 1 // the newline

		if err := spool.Add(doc); err != nil {
			t.Fatal(err)
		}
		if spool.IsEmpty() {
			t.Fatalf("spool still reports empty after %d documents", i+1)
		}
		if got := spool.DocCount(); got != int64(i+1) {
			t.Errorf("after %d documents DocCount is %d", i+1, got)
		}
		if got := spool.RawBytes(); got != wantRaw {
			t.Errorf("after %d documents RawBytes is %d, want %d (the uncompressed lines)", i+1, got, wantRaw)
		}
	}

	result, err := spool.Finish("a.ndjson.gz")
	if err != nil {
		t.Fatal(err)
	}
	if result.DocCount != spool.DocCount() || result.BytesRaw != spool.RawBytes() {
		t.Errorf("result (%d docs, %d raw) disagrees with the spool (%d, %d)",
			result.DocCount, result.BytesRaw, spool.DocCount(), spool.RawBytes())
	}
}

// Uploading a half-written file would store an archive with no compressor
// trailer and a checksum that matches nothing.
func TestSpoolOpenRefusesAFileStillBeingWritten(t *testing.T) {
	spool := newTestSpool(t, config.Compression{Codec: config.CodecGzip})
	t.Cleanup(func() { _ = spool.Discard() })

	if err := spool.Add(mustDocument(t, bson.M{"_id": "x"})); err != nil {
		t.Fatal(err)
	}
	file, err := spool.Open()
	if err == nil {
		_ = file.Close()
		t.Fatal("an unfinished spool was opened for upload")
	}
	if !strings.Contains(err.Error(), "still being written") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
}

// Each destination gets its own handle, so parallel uploads do not share an
// offset. Every handle must read the whole file from the start.
func TestSpoolOpenGivesEachCallerTheWholeFile(t *testing.T) {
	spool := newTestSpool(t, config.Compression{Codec: config.CodecZstd, Level: 3})
	t.Cleanup(func() { _ = spool.Discard() })

	for i := range 50 {
		if err := spool.Add(mustDocument(t, bson.M{"_id": int32(i), "v": "payload"})); err != nil {
			t.Fatal(err)
		}
	}
	result, err := spool.Finish("a.ndjson.zst")
	if err != nil {
		t.Fatal(err)
	}

	first, err := spool.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := spool.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	// Advance one handle; the other must be unaffected.
	if _, err := io.CopyN(io.Discard, first, 10); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(second)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(all)
	if int64(len(all)) != result.BytesCompressed || hex.EncodeToString(sum[:]) != result.SHA256 {
		t.Errorf("an opened handle read %d bytes hashing to %x; the result says %d bytes, %s",
			len(all), sum, result.BytesCompressed, result.SHA256)
	}
}

func TestFormatIDRendersEachKindOfID(t *testing.T) {
	oid := bson.NewObjectID()

	cases := []struct {
		name string
		id   any
		want string
	}{
		{"an ObjectId as its hex form", oid, oid.Hex()},
		{"a string as itself", "order-0042", "order-0042"},
		{"an empty string as itself", "", ""},
		{"an int32 as Extended JSON", int32(42), `{"$numberInt":"42"}`},
		{"an int64 as Extended JSON", int64(1) << 40, `{"$numberLong":"1099511627776"}`},
		{"a subdocument as Extended JSON", bson.D{{Key: "tenant", Value: "a"}}, `{"tenant": "a"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := mustDocument(t, bson.M{"_id": tc.id}).ID
			if got := formatID(id); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Integer ids must not read back as a different number in the catalogue.
func TestSpoolRecordsNonObjectIDRangeUnambiguously(t *testing.T) {
	spool := newTestSpool(t, config.Compression{Codec: config.CodecGzip})
	t.Cleanup(func() { _ = spool.Discard() })

	for _, id := range []int64{7, 8, 9} {
		if err := spool.Add(mustDocument(t, bson.M{"_id": id})); err != nil {
			t.Fatal(err)
		}
	}
	result, err := spool.Finish("a.ndjson.gz")
	if err != nil {
		t.Fatal(err)
	}
	if result.MinID != `{"$numberLong":"7"}` || result.MaxID != `{"$numberLong":"9"}` {
		t.Errorf("range %s..%s, want the Extended JSON for 7 and 9", result.MinID, result.MaxID)
	}
}

func TestZstdLevelMapsTheConfiguredLevel(t *testing.T) {
	cases := []struct {
		level int
		want  zstd.EncoderLevel
	}{
		{1, zstd.SpeedFastest},
		{2, zstd.SpeedDefault},
		{3, zstd.SpeedBetterCompression},
		{4, zstd.SpeedBestCompression},
		// Anything else, including gzip's own range, falls back to the default
		// rather than failing a run over a level zstd does not have.
		{0, zstd.SpeedDefault},
		{-1, zstd.SpeedDefault},
		{9, zstd.SpeedDefault},
	}
	for _, tc := range cases {
		if got := zstdLevel(tc.level); got != tc.want {
			t.Errorf("level %d: got %v, want %v", tc.level, got, tc.want)
		}
	}
}

// Every zstd level the config accepts must produce an archive that reads back.
func TestEveryZstdLevelProducesAReadableArchive(t *testing.T) {
	for _, level := range []int{0, 1, 2, 3, 4} {
		spool := newTestSpool(t, config.Compression{Codec: config.CodecZstd, Level: level})
		if err := spool.Add(mustDocument(t, bson.M{"_id": "only", "level": int32(level)})); err != nil {
			t.Fatal(err)
		}
		result, err := spool.Finish("a.ndjson.zst")
		if err != nil {
			t.Fatal(err)
		}

		reader, err := NewDocumentReader(bytes.NewReader(readFile(t, result.Path)), config.CodecZstd)
		if err != nil {
			t.Fatal(err)
		}
		doc, ok := reader.Next()
		if !ok {
			t.Fatalf("level %d: no document read back: %v", level, reader.Err())
		}
		if got := doc.Lookup("level").Int32(); got != int32(level) {
			t.Errorf("level %d: read back %d", level, got)
		}
		_ = reader.Close()
		_ = spool.Discard()
	}
}

// CompressedBytes says how much of the stored file a restore has consumed. Once
// the reader is drained it must be the whole file — the same count the spool
// recorded and the digest covers.
func TestReaderCompressedBytesCoversTheWholeFileOnceDrained(t *testing.T) {
	for _, codec := range []string{config.CodecGzip, config.CodecZstd} {
		t.Run(codec, func(t *testing.T) {
			spool := newSpool(t, codec)
			for _, raw := range sampleDocuments(t) {
				if err := spool.Add(mongoDoc(raw)); err != nil {
					t.Fatal(err)
				}
			}
			result, err := spool.Finish("a.ndjson")
			if err != nil {
				t.Fatal(err)
			}

			reader, err := NewDocumentReader(bytes.NewReader(readFile(t, result.Path)), codec)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()

			for {
				if _, ok := reader.Next(); !ok {
					break
				}
			}
			if err := reader.Err(); err != nil {
				t.Fatal(err)
			}
			if got := reader.CompressedBytes(); got != result.BytesCompressed {
				t.Errorf("reader consumed %d bytes, the stored file is %d", got, result.BytesCompressed)
			}
			if reader.SHA256() != result.SHA256 {
				t.Errorf("drained digest %s, recorded %s", reader.SHA256(), result.SHA256)
			}
		})
	}
}
