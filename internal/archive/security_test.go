package archive

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzip"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
)

// These tests treat an archive as what it is at restore time: bytes that came
// back from a bucket somebody else can write to, or a file an operator
// uploaded. Nothing in them may panic, buffer without limit, or read as a
// clean archive when it is not one.

// --- the spool file on local disk -------------------------------------------

// A spool holds documents that are about to be deleted from MongoDB. Another
// local user must not be able to read them while they sit on disk.
func TestSpoolFileIsNotReadableByOtherUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir := filepath.Join(t.TempDir(), "spool", "orders")
	spool, err := NewSpool(dir, "orders", config.Compression{Codec: config.CodecGzip})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Discard() })

	info, err := os.Stat(spool.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("spool file is %#o; nobody but the owner should read documents awaiting deletion", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("spool directory is %#o; it must not be open to every local user", perm)
	}
}

// The prefix comes from a config name. One that climbs out of the spool
// directory must be refused, not followed.
func TestSpoolPrefixCannotEscapeTheSpoolDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "spool")
	for _, prefix := range []string{"../escaped", "a/../../escaped", "/tmp/escaped"} {
		spool, err := NewSpool(dir, prefix, config.Compression{Codec: config.CodecGzip})
		if err == nil {
			_ = spool.Discard()
			if rel, relErr := filepath.Rel(dir, spool.path); relErr != nil || strings.HasPrefix(rel, "..") {
				t.Errorf("prefix %q put the spool file at %s, outside %s", prefix, spool.path, dir)
			}
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.Name() != "spool" {
			t.Errorf("a spool file was created outside the spool directory: %s", e.Name())
		}
	}
}

// Spool names are random and created exclusively, so a name cannot be guessed
// in advance and pre-planted as a symlink.
func TestSpoolNamesAreUnpredictable(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	for range 20 {
		spool, err := NewSpool(dir, "orders", config.Compression{Codec: config.CodecZstd})
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(spool.path)
		if seen[name] {
			t.Fatalf("spool name %s was handed out twice", name)
		}
		seen[name] = true
		if name == "orders.tmp" || name == "orders-.tmp" {
			t.Fatalf("spool name %s carries no random part", name)
		}
		_ = spool.Discard()
	}
}

// A spool abandoned before it was finished — the run failed — must not leave
// its documents behind on disk.
func TestDiscardingAnUnfinishedSpoolRemovesItsFile(t *testing.T) {
	spool, err := NewSpool(t.TempDir(), "orders", config.Compression{Codec: config.CodecGzip})
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Add(mongoDoc(mustRaw(t, bson.D{{Key: "_id", Value: 1}, {Key: "ssn", Value: "123-45-6789"}}))); err != nil {
		t.Fatal(err)
	}
	if err := spool.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool.path); !os.IsNotExist(err) {
		t.Errorf("the unfinished spool file is still on disk: %v", err)
	}
}

// The startup sweep deletes *.tmp. A symlink with that name must be removed as
// a link — the file it points at belongs to somebody else.
func TestCleanSpoolDirDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	spoolDir := filepath.Join(root, "spool")
	nested := filepath.Join(spoolDir, "orders")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}

	victimDir := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(victimDir, 0o750); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(victimDir, "important.tmp")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(nested, "planted.tmp")); err != nil {
		t.Fatal(err)
	}
	// A symlinked directory must not widen the sweep either.
	if err := os.Symlink(victimDir, filepath.Join(spoolDir, "linkdir")); err != nil {
		t.Fatal(err)
	}

	if _, err := CleanSpoolDir(spoolDir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "keep me" {
		t.Fatalf("the sweep reached through a symlink and removed %s (err %v)", victim, err)
	}
	if _, err := os.Lstat(filepath.Join(nested, "planted.tmp")); !os.IsNotExist(err) {
		t.Errorf("the planted link itself should have been removed: %v", err)
	}
}

// --- reading back hostile archives ------------------------------------------

// A line with no newline in it — a corrupt file or a decompression bomb of one
// repeated byte — is refused at maxLineBytes rather than buffered until the
// process is killed.
func TestAnArchiveWithNoNewlineIsRefusedAtTheLineLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("decompresses ~65 MB")
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	chunk := bytes.Repeat([]byte{'a'}, 1<<20)
	for range (maxLineBytes >> 20) + 1 {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if compressed.Len() > 1<<20 {
		t.Fatalf("fixture is %d bytes; expected a high-ratio bomb", compressed.Len())
	}

	reader, err := NewDocumentReader(bytes.NewReader(compressed.Bytes()), config.CodecGzip)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, ok := reader.Next(); ok {
		t.Fatal("a line longer than the limit decoded as a document")
	}
	if reader.Err() == nil {
		t.Fatal("an over-long line ended iteration without an error, as if the archive were empty")
	}
}

// Extended JSON nested past any depth MongoDB would accept is an error, not a
// stack overflow — which in Go is fatal and cannot be recovered.
func TestDeeplyNestedObjectIsRefused(t *testing.T) {
	const depth = 100_000
	line := `{"_id":1,"x":` + strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat(`}`, depth) + "}\n"

	reader, err := NewDocumentReader(strings.NewReader(line), CodecNone)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, ok := reader.Next(); ok {
		t.Errorf("a document nested %d objects deep was accepted", depth)
	}
	if reader.Err() == nil {
		t.Error("deep nesting stopped iteration without an error")
	}
}

// The parser's depth limit counts objects but not arrays, and the BSON writer
// recurses once per level. Ten million '[' — 20 MB, well inside maxLineBytes,
// and about 20 KB once gzipped — must come back as an error, not take the
// whole process down with an unrecoverable "goroutine stack exceeds limit".
//
// A fatal stack overflow cannot be caught in-process, so the parse runs in a
// child copy of this test binary and the parent judges how it exited.
func TestDeeplyNestedArrayDoesNotCrashTheProcess(t *testing.T) {
	const childEnv = "FEATHER_SECURITY_DEEP_ARRAY_CHILD"
	if os.Getenv(childEnv) == "1" {
		const depth = 10_000_000
		line := `{"_id":1,"x":` + strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth) + "}\n"
		reader, err := NewDocumentReader(strings.NewReader(line), CodecNone)
		if err != nil {
			return
		}
		reader.Next()
		_ = reader.Close()
		return
	}
	if testing.Short() {
		t.Skip("parses a 20 MB document in a child process")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDeeplyNestedArrayDoesNotCrashTheProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || bytes.Contains(out, []byte("stack overflow")) {
		head := out
		if len(head) > 400 {
			head = head[:400]
		}
		t.Fatalf("reading a deeply nested array crashed the process (%v):\n%s", err, head)
	}
}

// A zstd frame can ask for a window of gigabytes in a header of a few bytes.
// The decoder must refuse rather than allocate what the file asks for.
func TestZstdFrameDemandingAHugeWindowIsRefused(t *testing.T) {
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.LittleEndian, uint32(0xFD2FB528)) // magic
	frame.WriteByte(0x00)                                             // no single segment, no checksum, no FCS
	frame.WriteByte(byte(31-10) << 3)                                 // window log 31: 2 GiB
	frame.Write([]byte{0x09, 0x00, 0x00})                             // last raw block, 1 byte
	frame.WriteByte('{')

	reader, err := NewDocumentReader(bytes.NewReader(frame.Bytes()), config.CodecZstd)
	if err != nil {
		return // refused up front: fine
	}
	defer reader.Close()
	if _, ok := reader.Next(); ok {
		t.Fatal("a frame asking for a 2 GiB window decoded a document")
	}
	if reader.Err() == nil {
		t.Fatal("a frame asking for a 2 GiB window was read as an empty archive")
	}
}

// An archive cut short — an interrupted upload, a partial download — must
// never read as a shorter but complete one.
func TestTruncatedArchivesAreErrors(t *testing.T) {
	for _, codec := range []string{config.CodecGzip, config.CodecZstd} {
		t.Run(codec, func(t *testing.T) {
			whole := securityArchive(t, codec, 50)
			for _, cut := range truncationPoints(len(whole)) {
				reader, err := NewDocumentReader(bytes.NewReader(whole[:cut]), codec)
				if err != nil {
					continue // refused at the header: an error, which is the point
				}
				for {
					if _, ok := reader.Next(); !ok {
						break
					}
				}
				if reader.Err() == nil && reader.Drain() == nil {
					t.Errorf("an archive cut at %d of %d bytes read cleanly with %d documents",
						cut, len(whole), reader.Count())
				}
				_ = reader.Close()
			}
		})
	}
}

// Garbage appended after a valid stream is not silently ignored: either the
// decoder fails or the digest no longer matches the catalogue.
func TestTrailingBytesAfterTheStreamAreNoticed(t *testing.T) {
	for _, codec := range []string{config.CodecGzip, config.CodecZstd} {
		t.Run(codec, func(t *testing.T) {
			whole := securityArchive(t, codec, 5)
			clean := sha256Of(t, whole, codec)

			tampered := append(append([]byte(nil), whole...), []byte("\x00\x01trailing garbage")...)
			reader, err := NewDocumentReader(bytes.NewReader(tampered), codec)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			for {
				if _, ok := reader.Next(); !ok {
					break
				}
			}
			drainErr := reader.Drain()
			if reader.Err() == nil && drainErr == nil && reader.SHA256() == clean {
				t.Fatal("trailing bytes changed neither the outcome nor the digest")
			}
			if reader.SHA256() == clean {
				t.Error("the digest does not cover bytes after the end of the stream")
			}
		})
	}
}

// The codec comes from a catalogue row or a file name. A made-up one is
// refused, never mapped to "read it raw".
func TestHostileCodecNamesAreRefused(t *testing.T) {
	for _, codec := range []string{"../../bin/sh", "GZIP", "zstd ", "none\x00gzip", "lz4"} {
		if _, err := Decompress(strings.NewReader("x"), codec); err == nil {
			t.Errorf("codec %q was accepted", codec)
		}
	}
	for _, name := range []string{"orders.gz.exe", "orders", "../../etc/passwd", "orders.tar"} {
		if _, err := CodecForUpload(name); err == nil {
			t.Errorf("upload %q was given a codec", name)
		}
	}
}

// FuzzDocumentReader holds the line that untrusted bytes, under any codec,
// produce documents or an error — never a panic or a hang. The seed corpus
// runs under plain `go test`.
func FuzzDocumentReader(f *testing.F) {
	codecs := []string{config.CodecGzip, config.CodecZstd, CodecNone}

	for i, codec := range codecs {
		whole := seedArchive(f, codec)
		f.Add(whole, uint8(i))
		f.Add(whole[:len(whole)/2], uint8(i))
		flipped := append([]byte(nil), whole...)
		flipped[len(flipped)/3] ^= 0xff
		f.Add(flipped, uint8(i))
	}
	f.Add([]byte(`{"_id":{"$oid":"zz"}}`+"\n"), uint8(2))
	f.Add([]byte(`{"$numberDecimal":"1e999999999"}`+"\n"), uint8(2))
	f.Add([]byte(`{"_id":1,"d":{"$date":{"$numberLong":"-99999999999999999999"}}}`+"\n"), uint8(2))
	f.Add([]byte(`{"_id":1,"b":{"$binary":{"base64":"!!!","subType":"ff"}}}`+"\n"), uint8(2))
	f.Add([]byte("\n\n\n\r\n \t\n"), uint8(2))
	f.Add([]byte{0x1f, 0x8b, 0x08, 0x00}, uint8(0))
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd}, uint8(1))

	f.Fuzz(func(t *testing.T, data []byte, which uint8) {
		codec := codecs[int(which)%len(codecs)]
		reader, err := NewDocumentReader(bytes.NewReader(data), codec)
		if err != nil {
			return
		}
		defer reader.Close()
		for range 10_000 {
			doc, ok := reader.Next()
			if !ok {
				break
			}
			if err := doc.Validate(); err != nil {
				t.Fatalf("decoded an invalid BSON document: %v", err)
			}
		}
		_ = reader.Drain()
		if reader.CompressedBytes() > int64(len(data)) {
			t.Fatalf("counted %d bytes of a %d-byte input", reader.CompressedBytes(), len(data))
		}
	})
}

// --- helpers ----------------------------------------------------------------

func mustRaw(t testing.TB, doc bson.D) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func securityArchive(t testing.TB, codec string, docs int) []byte {
	t.Helper()
	spool, err := NewSpool(t.TempDir(), "security", config.Compression{Codec: codec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spool.Discard() }()
	for i := range docs {
		raw := mustRaw(t, bson.D{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "n", Value: int32(i)},
			{Key: "pad", Value: strings.Repeat("p", 100)},
		})
		if err := spool.Add(mongoDoc(raw)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := spool.Finish("security")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func seedArchive(f *testing.F, codec string) []byte {
	if codec != CodecNone {
		return securityArchive(f, codec, 3)
	}
	return []byte(`{"_id":{"$oid":"65f0c0ffee0000000000abcd"},"n":{"$numberInt":"1"}}` + "\n" + `{"_id":2,"s":"x"}` + "\n")
}

// truncationPoints samples cut positions: every byte of the header and
// trailer, where the interesting failures are, and a spread through the body.
func truncationPoints(n int) []int {
	var out []int
	// From 1: a zero-byte zstd input is zero frames, which the decoder reads as
	// an empty stream; the catalogue's size and digest catch that case.
	for i := 1; i < n; i++ {
		if i < 32 || i > n-32 || i%17 == 0 {
			out = append(out, i)
		}
	}
	return out
}

func sha256Of(t *testing.T, data []byte, codec string) string {
	t.Helper()
	reader, err := NewDocumentReader(bytes.NewReader(data), codec)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.Drain(); err != nil {
		t.Fatal(err)
	}
	return reader.SHA256()
}
