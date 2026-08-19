package archive

import (
	"bufio"
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

// Decompress wraps r with the decoder for a codec. The returned closer must be
// closed, and closing it does not close r.
func Decompress(r io.Reader, codec string) (io.ReadCloser, error) {
	switch codec {
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
		var doc bson.Raw
		if err := bson.UnmarshalExtJSON(line, true, &doc); err != nil {
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

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
