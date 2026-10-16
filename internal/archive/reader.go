package archive

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
)

// This is the other half of spool.go: reading back what it wrote.
//
// Until now nothing in feather ever read an archive again. The restore
// procedure was seven lines of README prose that no test had executed, which is
// not a defensible position for a tool whose job is deleting the original.

// maxLineBytes bounds one document read back from an archive. Documents cannot
// exceed MongoDB's 16 MB BSON limit, and Extended JSON is more verbose than
// BSON, so 64 MB is generous while still refusing to buffer a corrupt file
// with no newline in it into memory without limit.
const maxLineBytes = 64 << 20

// maxNestingDepth bounds how deeply objects and arrays may nest in one line.
// The Extended JSON parser caps objects at 200 but not arrays, and both it and
// the BSON writer recurse once per level, so ten million '[' is a fatal,
// unrecoverable stack overflow. MongoDB itself refuses documents nested more
// than 100 deep, so 200 rejects nothing a real archive can contain.
const maxNestingDepth = 200

// CodecNone is an uncompressed archive: plain NDJSON. feather never writes one
// — it is not a codec a retention config may choose — but an operator importing
// a file exported by hand often has exactly that.
const CodecNone = "none"

// Decompress wraps r with the decoder for a codec. The returned closer must be
// closed, and closing it does not close r.
func Decompress(r io.Reader, codec string) (io.ReadCloser, error) {
	switch codec {
	case CodecNone:
		return io.NopCloser(r), nil
	case config.CodecZstd:
		decoder, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("opening the zstd stream: %w", err)
		}
		return decoder.IOReadCloser(), nil
	case config.CodecGzip, "":
		// Empty means an archive written before the codec was recorded, and
		// gzip was the only option then.
		reader, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("opening the gzip stream: %w", err)
		}
		return reader, nil
	default:
		return nil, fmt.Errorf("unknown compression codec %q", codec)
	}
}

// CodecForFileName guesses the codec from an archive's name, for a file that
// arrived without its catalogue row.
func CodecForFileName(name string) string {
	switch {
	case strings.HasSuffix(name, ".zst"), strings.HasSuffix(name, ".zstd"):
		return config.CodecZstd
	default:
		return config.CodecGzip
	}
}

// CodecForUpload picks the codec for a file an operator uploaded, from its
// name. Unlike CodecForFileName it does not assume gzip: an uploaded file has
// no catalogue row behind it, and guessing wrong turns a plain NDJSON export
// into a confusing "not a gzip stream" error, so anything unrecognised is
// refused with the list of what is accepted.
func CodecForUpload(name string) (string, error) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zst"), strings.HasSuffix(lower, ".zstd"):
		return config.CodecZstd, nil
	case strings.HasSuffix(lower, ".gz"):
		return config.CodecGzip, nil
	case strings.HasSuffix(lower, ".ndjson"), strings.HasSuffix(lower, ".jsonl"), strings.HasSuffix(lower, ".json"):
		return CodecNone, nil
	default:
		return "", fmt.Errorf("%s is not a file feather can import — use .ndjson, .jsonl or .json, optionally compressed as .gz or .zst", name)
	}
}

// DocumentReader streams documents back out of an archive, decompressing and
// decoding as it goes, and hashing the compressed bytes on the way past.
//
// Hashing here is the point: it means a restore verifies the copy it is
// restoring from, rather than trusting that a verification months ago still
// holds. Bit-rot, a lifecycle rule, and a wrong-key overwrite all look like a
// perfectly readable file until you compare the digest.
type DocumentReader struct {
	source     io.Reader
	hasher     hash.Hash
	counter    *countingReader
	decompress io.ReadCloser
	lines      *bufio.Scanner

	count int64
	err   error
}

// NewDocumentReader reads an archive from r, which must be the compressed bytes
// exactly as they were stored.
func NewDocumentReader(r io.Reader, codec string) (*DocumentReader, error) {
	hasher := sha256.New()
	counter := &countingReader{r: io.TeeReader(r, hasher)}

	decompress, err := Decompress(counter, codec)
	if err != nil {
		return nil, err
	}

	lines := bufio.NewScanner(decompress)
	lines.Buffer(make([]byte, 0, 256<<10), maxLineBytes)

	return &DocumentReader{
		source: r, hasher: hasher, counter: counter,
		decompress: decompress, lines: lines,
	}, nil
}

// Next decodes the next document, returning false at the end of the archive.
func (d *DocumentReader) Next() (bson.Raw, bool) {
	if d.err != nil {
		return nil, false
	}
	for d.lines.Scan() {
		line := d.lines.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		// Parsed in relaxed mode, which also accepts canonical Extended JSON.
		// feather writes canonical, and relaxed parsing reads every canonical
		// value back to the identical BSON — TestCanonicalArchivesParseIdentically
		// holds that line — while also accepting the relaxed form mongoexport
		// and most hand-made exports use ({"n": 1}, {"$date": "2024-…"}).
		//
		// The line is scanned first: the parser stops after the first value and
		// would silently drop anything after it, and it does not bound array
		// nesting at all.
		if err := checkLineShape(line); err != nil {
			d.err = fmt.Errorf("document %d is not valid Extended JSON: %w", d.count+1, err)
			return nil, false
		}
		var doc bson.Raw
		if err := bson.UnmarshalExtJSON(line, false, &doc); err != nil {
			d.err = fmt.Errorf("document %d is not valid Extended JSON: %w", d.count+1, err)
			return nil, false
		}
		d.count++
		return doc, true
	}
	if err := d.lines.Err(); err != nil {
		d.err = fmt.Errorf("reading the archive: %w", err)
	}
	return nil, false
}

// NextLine returns the next record as the raw line it was archived as, without
// decoding it — how a PostgreSQL archive, whose lines are row_to_json rather
// than Extended JSON, is read back. The slice is the caller's to keep.
func (d *DocumentReader) NextLine() ([]byte, bool) {
	if d.err != nil {
		return nil, false
	}
	for d.lines.Scan() {
		line := bytes.TrimSpace(d.lines.Bytes())
		if len(line) == 0 {
			continue
		}
		d.count++
		return append([]byte(nil), line...), true
	}
	if err := d.lines.Err(); err != nil {
		d.err = fmt.Errorf("reading the archive: %w", err)
	}
	return nil, false
}

// Count is how many documents have been read so far.
func (d *DocumentReader) Count() int64 { return d.count }

// CompressedBytes is how many bytes of the stored file have been consumed.
func (d *DocumentReader) CompressedBytes() int64 { return d.counter.n }

// SHA256 is the digest of the compressed bytes read so far. It is only the
// archive's digest once the reader has been driven to the end.
func (d *DocumentReader) SHA256() string { return hex.EncodeToString(d.hasher.Sum(nil)) }

// Err reports why iteration stopped, if it was not the end of the archive.
func (d *DocumentReader) Err() error { return d.err }

// Close releases the decompressor. It does not close the underlying reader.
func (d *DocumentReader) Close() error { return d.decompress.Close() }

// Drain reads whatever is left of the compressed stream without decoding it, so
// the digest covers the whole file even when the caller stopped early.
func (d *DocumentReader) Drain() error {
	_, err := io.Copy(io.Discard, d.counter)
	return err
}

// checkLineShape refuses a line whose objects and arrays nest deeper than
// maxNestingDepth, or that has anything but whitespace after its first
// complete top-level value. It only tracks brackets outside strings; the
// parser still judges everything else.
func checkLineShape(line []byte) error {
	depth := 0
	inString, escaped, closed := false, false, false
	for i, c := range line {
		if closed {
			if !isJSONSpace(c) {
				return fmt.Errorf("unexpected content after the document at byte %d", i)
			}
			continue
		}
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxNestingDepth {
				return fmt.Errorf("nested more than %d levels deep", maxNestingDepth)
			}
		case '}', ']':
			depth--
			if depth <= 0 {
				// Unbalanced closers are left for the parser to report.
				closed = depth == 0
			}
		}
	}
	return nil
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
