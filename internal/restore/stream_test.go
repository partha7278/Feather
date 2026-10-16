package restore

import (
	"bytes"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/config"
)

// Stream is what an uploaded file goes through. A dry run has to read and hash
// all of it, so the digest and size it reports are the file's own.
func TestStreamDryRunCountsAndHashesTheWholeFile(t *testing.T) {
	written := buildArchive(t, 30)

	result, err := Stream(t.Context(), discardLog(), nil, bytes.NewReader(written.bytes), config.CodecGzip,
		Options{DryRun: true})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if result.DocumentsRead != 30 || result.Inserted != 30 {
		t.Errorf("read %d and would insert %d, want 30 and 30", result.DocumentsRead, result.Inserted)
	}
	if !strings.EqualFold(result.SHA256, written.artifact.SHA256) {
		t.Errorf("hashed %s, the archive is %s", result.SHA256, written.artifact.SHA256)
	}
	if result.BytesRead != int64(len(written.bytes)) {
		t.Errorf("read %d bytes, the archive is %d", result.BytesRead, len(written.bytes))
	}
	if !result.DryRun {
		t.Error("result does not record that it was a dry run")
	}
}

// The progress callback is what keeps a long import's page moving.
func TestStreamReportsProgressAfterEveryBatch(t *testing.T) {
	written := buildArchive(t, 25)

	var seen []int64
	_, err := Stream(t.Context(), discardLog(), nil, bytes.NewReader(written.bytes), config.CodecGzip,
		Options{DryRun: true, BatchSize: 10, Progress: func(r Result) { seen = append(seen, r.DocumentsRead) }})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{10, 20, 25}
	if len(seen) != len(want) {
		t.Fatalf("progress called %d times (%v), want %d", len(seen), seen, len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("call %d saw %d documents read, want %d", i, seen[i], want[i])
		}
	}
}

func TestStreamReadsPlainNDJSON(t *testing.T) {
	body := `{"_id":1,"name":"a"}` + "\n" + `{"_id":2,"name":"b"}` + "\n"
	result, err := Stream(t.Context(), discardLog(), nil, strings.NewReader(body), archive.CodecNone,
		Options{DryRun: true, Mode: ModeReplace})
	if err != nil {
		t.Fatal(err)
	}
	if result.DocumentsRead != 2 || result.Inserted != 2 || result.Replaced != 0 {
		t.Errorf("got read %d, inserted %d, replaced %d", result.DocumentsRead, result.Inserted, result.Replaced)
	}
}

// A bad line is reported by number, and what was read before it is still
// counted.
func TestStreamStopsAtAnInvalidLine(t *testing.T) {
	body := "{\"_id\":1}\n{\"_id\":2}\nnot json\n{\"_id\":4}\n"
	result, err := Stream(t.Context(), discardLog(), nil, strings.NewReader(body), archive.CodecNone,
		Options{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "document 3") {
		t.Fatalf("expected the third line to be named, got %v", err)
	}
	if result.DocumentsRead != 2 {
		t.Errorf("read %d documents before the bad line, want 2", result.DocumentsRead)
	}
}

func TestStreamNeedsACollectionUnlessDryRun(t *testing.T) {
	if _, err := Stream(t.Context(), discardLog(), nil, strings.NewReader(""), archive.CodecNone, Options{}); err == nil {
		t.Fatal("expected a real stream with no target to be refused")
	}
}

// Artifact now goes through Stream; the digest it reports has to be the one
// it checked.
func TestArtifactReportsTheDigestItChecked(t *testing.T) {
	written := buildArchive(t, 8)
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: written.bytes}}

	result, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, written.copies,
		Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 != written.artifact.SHA256 || result.BytesRead != written.artifact.BytesCompressed {
		t.Errorf("reported %s / %d bytes, want %s / %d", result.SHA256, result.BytesRead,
			written.artifact.SHA256, written.artifact.BytesCompressed)
	}
	if result.Artifact.FileName != written.artifact.FileName || result.ReadFrom != "cold" {
		t.Errorf("result lost its artifact or source: %+v", result)
	}
}
