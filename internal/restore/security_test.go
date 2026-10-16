package restore

import (
	"bytes"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzip"

	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

// A restore reads bytes from a bucket that other credentials can write to. The
// catalogue is the only thing feather wrote itself, so everything the object
// says about itself is checked against it, and nothing in the object decides
// where its documents go.

// Somebody with write access to the bucket appends a second gzip member. gzip
// readers concatenate members, so the extra documents decode as if they had
// always been there — the digest is what has to catch it.
func TestDocumentsAppendedToAnArchiveAreDetected(t *testing.T) {
	written := buildArchive(t, 8)
	tampered := append(append([]byte(nil), written.bytes...),
		gzipMember(t, `{"_id":{"$oid":"65f0c0ffee0000000000abcd"},"role":"admin"}`+"\n")...)
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: tampered}}

	_, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, written.copies,
		Options{DryRun: true})
	if err == nil {
		t.Fatal("an archive with injected documents restored cleanly")
	}

	verdict := VerifyArtifact(t.Context(), discardLog(), registry, written.artifact, written.copies)
	if verdict.Intact() {
		t.Fatal("an archive with injected documents verified as intact")
	}
}

// Without a recorded digest the document count is the remaining check, and it
// must still notice the injection.
func TestInjectedDocumentsAreCaughtByCountWhenNoDigestIsRecorded(t *testing.T) {
	written := buildArchive(t, 8)
	written.artifact.SHA256 = ""
	tampered := append(append([]byte(nil), written.bytes...),
		gzipMember(t, `{"_id":1}`+"\n")...)
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: tampered}}

	_, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, written.copies,
		Options{DryRun: true})
	if err == nil {
		t.Fatal("nine documents restored from an archive the catalogue says holds eight")
	}
}

// The object is swapped for a perfectly valid archive of some other batch. It
// decodes, counts sensibly, and is still not the archive that was catalogued.
func TestASwappedButValidArchiveIsRefused(t *testing.T) {
	expected := buildArchive(t, 10)
	impostor := buildArchive(t, 10)
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: impostor.bytes}}

	_, err := Artifact(t.Context(), discardLog(), registry, nil, expected.artifact, expected.copies,
		Options{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "not the archive it claims to be") {
		t.Fatalf("a different archive in the same place restored: %v", err)
	}
}

// A copy cut short — a failed multipart upload, a lifecycle rule mid-transition
// — fails the restore wherever the cut falls, even when no digest is recorded.
func TestATruncatedCopyFailsTheRestore(t *testing.T) {
	written := buildArchive(t, 20)
	written.artifact.SHA256 = ""
	written.artifact.DocCount = 0

	for _, cut := range []int{1, 10, len(written.bytes) / 3, len(written.bytes) / 2, len(written.bytes) - 9, len(written.bytes) - 1} {
		registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: written.bytes[:cut]}}
		_, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, written.copies,
			Options{DryRun: true})
		if err == nil {
			t.Errorf("an archive cut at %d of %d bytes restored cleanly", cut, len(written.bytes))
		}
	}
}

// Uploaded files have no catalogue behind them, so the decoder's own checks are
// all that stand between a truncated upload and a partial import reported as
// complete.
func TestStreamRefusesATruncatedUpload(t *testing.T) {
	written := buildArchive(t, 20)
	for _, cut := range []int{len(written.bytes) / 2, len(written.bytes) - 4} {
		_, err := Stream(t.Context(), discardLog(), nil, bytes.NewReader(written.bytes[:cut]), config.CodecGzip,
			Options{DryRun: true})
		if err == nil {
			t.Errorf("a gzip upload cut at %d of %d bytes streamed cleanly", cut, len(written.bytes))
		}
	}
}

// The codec is read from the catalogue. A tampered or unknown one is an error,
// and a lie about the codec does not turn compressed bytes into documents.
func TestACatalogueCodecCannotBeUsedToSkipDecoding(t *testing.T) {
	written := buildArchive(t, 4)
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: written.bytes}}

	for _, codec := range []string{"../../bin/sh", "lz4", archive.CodecNone, config.CodecZstd} {
		artifact := written.artifact
		artifact.Codec = codec
		result, err := Artifact(t.Context(), discardLog(), registry, nil, artifact, written.copies,
			Options{DryRun: true})
		if err == nil {
			t.Errorf("codec %q read a gzip archive as %d documents", codec, result.DocumentsRead)
		}
	}
}

// Where documents go is the caller's decision. An artifact whose catalogue row
// names another database — admin, say — does not supply a target of its own.
func TestArchiveMetadataNeverChoosesTheTargetCollection(t *testing.T) {
	written := buildArchive(t, 3)
	written.artifact.Database = "admin"
	written.artifact.Collection = "system.users"
	registry := fakeRegistry{"cold": &fakeDestination{alias: "cold", object: written.bytes}}

	_, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, written.copies, Options{})
	if err == nil || !strings.Contains(err.Error(), "no target collection") {
		t.Fatalf("a restore with no target given did not stop: %v", err)
	}
	if _, err := Stream(t.Context(), discardLog(), nil, bytes.NewReader(written.bytes), config.CodecGzip, Options{}); err == nil {
		t.Fatal("a stream with no target given did not stop")
	}
}

// Asking for a particular destination must not quietly fall back to another
// one — that would read from an account the operator did not choose.
func TestPreferAliasDoesNotFallBackToAnotherDestination(t *testing.T) {
	written := buildArchive(t, 2)
	registry := fakeRegistry{
		"cold":  &fakeDestination{alias: "cold", object: written.bytes},
		"rogue": &fakeDestination{alias: "rogue", object: written.bytes},
	}

	_, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact, written.copies,
		Options{DryRun: true, PreferAlias: "rogue"})
	if err == nil {
		t.Fatal("a preferred destination the archive has no copy on was read from anyway")
	}
}

// A copy whose destination is not configured is never resolved by some other
// route; the copy list is all a restore may read from.
func TestOnlyCataloguedCopiesAreRead(t *testing.T) {
	written := buildArchive(t, 2)
	registry := fakeRegistry{"other": &fakeDestination{alias: "other", object: written.bytes}}

	_, err := Artifact(t.Context(), discardLog(), registry, nil, written.artifact,
		[]store.ArtifactDestination{{Alias: "cold", ObjectKey: "../../other/key"}}, Options{DryRun: true})
	if err == nil {
		t.Fatal("a restore read from a destination the catalogue does not list")
	}
}

// Hostile document content stops the stream with an error rather than reaching
// the target: nesting past MongoDB's limits, and lines that are not documents.
func TestStreamStopsAtHostileDocuments(t *testing.T) {
	deep := `{"_id":1,"x":` + strings.Repeat(`{"a":`, 5000) + "1" + strings.Repeat(`}`, 5000) + "}\n"
	for name, body := range map[string]string{
		"deep nesting": deep,
		"bare array":   "[1,2,3]\n",
		"bare string":  `"just a string"` + "\n",
		"unclosed":     `{"_id":1,"x":` + "\n",
		"bad oid":      `{"_id":{"$oid":"not-hex"}}` + "\n",
		"bad date":     `{"_id":1,"d":{"$date":"yesterday"}}` + "\n",
		"nul in key":   "{\"_id\":1,\"a\x00b\":1}\n",
	} {
		result, err := Stream(t.Context(), discardLog(), nil, strings.NewReader(body), archive.CodecNone,
			Options{DryRun: true})
		if err == nil {
			t.Errorf("%s: streamed cleanly as %d documents", name, result.DocumentsRead)
		}
	}
}

// Two documents on one line: the parser stops after the first value and the
// rest of the line is dropped without a word, so an import reports success
// while content in the file never reached the target — and a reviewer reading
// the file sees data the restore silently ignores.
func TestTrailingContentOnALineIsNotSilentlyDropped(t *testing.T) {
	for _, body := range []string{
		`{"_id":1}{"_id":2}` + "\n",
		`{"_id":1} trailing garbage` + "\n",
	} {
		result, err := Stream(t.Context(), discardLog(), nil, strings.NewReader(body), archive.CodecNone,
			Options{DryRun: true})
		if err == nil && result.DocumentsRead < 2 {
			t.Errorf("%q streamed cleanly as %d document(s); the rest of the line vanished", body, result.DocumentsRead)
		}
	}
}

func gzipMember(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
