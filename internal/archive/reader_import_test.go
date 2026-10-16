package archive

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
)

// The reader parses in relaxed mode so that files exported by other tools can
// be imported. That is only acceptable if every archive feather has ever
// written — canonical Extended JSON — still decodes to exactly the BSON it was
// written from. This is the line that decision rests on: if it ever fails,
// relaxed parsing has to become an upload-only option.
func TestCanonicalArchivesParseIdentically(t *testing.T) {
	decimal, err := bson.ParseDecimal128("-0.000000001")
	if err != nil {
		t.Fatal(err)
	}
	docs := []bson.D{{
		{Key: "_id", Value: bson.NewObjectID()},
		{Key: "date", Value: time.Date(2026, 3, 14, 15, 9, 26, 535000000, time.UTC)},
		{Key: "beforeEpoch", Value: time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Key: "decimal", Value: decimal},
		{Key: "int32", Value: int32(7)},
		// The values relaxed JSON would otherwise read as a narrower type.
		{Key: "smallInt64", Value: int64(5)},
		{Key: "bigInt64", Value: int64(9007199254740993)},
		{Key: "wholeDouble", Value: 1.0},
		{Key: "negativeZero", Value: math.Copysign(0, -1)},
		{Key: "nan", Value: math.NaN()},
		{Key: "infinity", Value: math.Inf(-1)},
		{Key: "uuid", Value: bson.Binary{Subtype: 0x04, Data: []byte("0123456789abcdef")}},
		{Key: "regex", Value: bson.Regex{Pattern: "^a.*", Options: "i"}},
		{Key: "timestamp", Value: bson.Timestamp{T: 1700000000, I: 3}},
		{Key: "minKey", Value: bson.MinKey{}},
		{Key: "maxKey", Value: bson.MaxKey{}},
		{Key: "code", Value: bson.JavaScript("function () {}")},
		{Key: "array", Value: bson.A{int32(1), int64(2), 3.0, "four", nil}},
	}}

	spool := newSpool(t, config.CodecGzip)
	var originals []bson.Raw
	for _, doc := range docs {
		encoded, err := bson.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		originals = append(originals, encoded)
		if err := spool.Add(mongoDoc(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	for _, doc := range sampleDocuments(t) {
		originals = append(originals, doc)
		if err := spool.Add(mongoDoc(doc)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := spool.Finish("types.ndjson.gz")
	if err != nil {
		t.Fatal(err)
	}

	reader, err := NewDocumentReader(bytes.NewReader(readFile(t, result.Path)), config.CodecGzip)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	for i, want := range originals {
		got, ok := reader.Next()
		if !ok {
			t.Fatalf("archive ended after %d documents: %v", i, reader.Err())
		}
		if !bytes.Equal(got, want) {
			t.Errorf("document %d changed:\n  wrote %s\n  read  %s", i, want.String(), got.String())
		}
	}
	if _, ok := reader.Next(); ok {
		t.Error("the archive held more documents than were written")
	}
}

// What mongoexport writes by default, and what an operator types by hand.
func TestRelaxedExtendedJSONIsAccepted(t *testing.T) {
	line := `{"_id":{"$oid":"5f1b2c3d4e5f6a7b8c9d0e1f"},"n":1,"big":5000000000,"price":2.5,"at":{"$date":"2024-01-02T03:04:05Z"}}` + "\n"

	reader, err := NewDocumentReader(strings.NewReader(line), CodecNone)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	doc, ok := reader.Next()
	if !ok {
		t.Fatalf("a relaxed line did not parse: %v", reader.Err())
	}
	if got := doc.Lookup("n").Type; got != bson.TypeInt32 {
		t.Errorf("n decoded as %v, want int32", got)
	}
	if got := doc.Lookup("big").Type; got != bson.TypeInt64 {
		t.Errorf("big decoded as %v, want int64", got)
	}
	if got := doc.Lookup("at").Type; got != bson.TypeDateTime {
		t.Errorf("at decoded as %v, want a date", got)
	}
}

// Plain NDJSON has no compression, but it is hashed and counted like any other
// archive so an upload's digest can still be recorded.
func TestPlainNDJSONReadsWithCodecNone(t *testing.T) {
	body := "{\"a\":1}\n\n{\"a\":2}\n{\"a\":3}"
	reader, err := NewDocumentReader(strings.NewReader(body), CodecNone)
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
	if err := reader.Drain(); err != nil {
		t.Fatal(err)
	}
	if reader.Count() != 3 {
		t.Errorf("read %d documents, want 3 (blank lines skipped)", reader.Count())
	}
	if reader.CompressedBytes() != int64(len(body)) {
		t.Errorf("counted %d bytes, want %d", reader.CompressedBytes(), len(body))
	}
}

func TestCodecForUpload(t *testing.T) {
	for name, want := range map[string]string{
		"orders.ndjson":        CodecNone,
		"orders.jsonl":         CodecNone,
		"orders.json":          CodecNone,
		"orders.ndjson.gz":     config.CodecGzip,
		"ORDERS.NDJSON.GZ":     config.CodecGzip,
		"orders.ndjson.zst":    config.CodecZstd,
		"orders.ndjson.zstd":   config.CodecZstd,
		"export.jsonl.gz":      config.CodecGzip,
		"feather-00001.ndjson": CodecNone,
	} {
		got, err := CodecForUpload(name)
		if err != nil || got != want {
			t.Errorf("%s: got %q (%v), want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"orders.csv", "orders.bson", "orders", "orders.tar.gz.bak"} {
		if _, err := CodecForUpload(name); err == nil {
			t.Errorf("%s: expected to be refused", name)
		}
	}
}
