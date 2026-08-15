package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/mongox"
)

// Spool builds one compressed archive file on local disk.
//
// Documents are encoded and compressed as they arrive and never all held in
// memory, so a batch is bounded by disk rather than RAM. The write path is
// documents -> Extended JSON line -> compressor -> (file + SHA-256 + byte
// counter), which means the recorded checksum covers exactly the bytes that get
// uploaded and can be compared against the stored object afterwards.
type Spool struct {
	path       string
	file       *os.File
	compressor io.WriteCloser
	counter    *countingWriter
	hasher     hash.Hash

	docCount int64
	rawBytes int64
	ids      []bson.RawValue
	minID    string
	maxID    string
	minTS    *time.Time
	maxTS    *time.Time

	codec  string
	closed bool
	result *Result
}

// Result is the finished description of a spooled file.
type Result struct {
	Path            string
	FileName        string
	DocCount        int64
	BytesRaw        int64
	BytesCompressed int64
	SHA256          string
	MinID           string
	MaxID           string
	MinTS           *time.Time
	MaxTS           *time.Time
}

// NewSpool creates a spool file in dir. The caller owns it until Discard.
func NewSpool(dir, namePrefix string, comp config.Compression) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating spool dir %s: %w", dir, err)
	}

	file, err := os.CreateTemp(dir, namePrefix+"-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("creating spool file in %s: %w", dir, err)
	}

	spool := &Spool{
		path:   file.Name(),
		file:   file,
		hasher: sha256.New(),
		codec:  comp.Codec,
	}
	spool.counter = &countingWriter{w: io.MultiWriter(file, spool.hasher)}

	switch comp.Codec {
	case config.CodecZstd:
		encoder, err := zstd.NewWriter(spool.counter, zstd.WithEncoderLevel(zstdLevel(comp.Level)))
		if err != nil {
			spool.abort()
			return nil, fmt.Errorf("starting zstd encoder: %w", err)
		}
		spool.compressor = encoder
	default:
		writer, err := gzip.NewWriterLevel(spool.counter, comp.Level)
		if err != nil {
			spool.abort()
			return nil, fmt.Errorf("starting gzip writer: %w", err)
		}
		spool.compressor = writer
	}

	return spool, nil
}

// Add encodes one document as a canonical Extended JSON line. Canonical form is
// deliberate: it preserves ObjectId, Date, Decimal128 and int32-vs-int64, so the
// archive restores through mongoimport as the same types it started as.
func (s *Spool) Add(doc mongox.Document) error {
	line, err := bson.MarshalExtJSON(doc.Raw, true, false)
	if err != nil {
		return fmt.Errorf("encoding document as Extended JSON: %w", err)
	}
	line = append(line, '\n')

	written, err := s.compressor.Write(line)
	if err != nil {
		return fmt.Errorf("writing to spool file: %w", err)
	}

	s.rawBytes += int64(written)
	s.docCount++
	// Copied, not retained. doc.ID is a bson.RawValue whose Value field is a
	// subslice of the document's own buffer, and Go cannot free a backing array
	// while any subslice of it is reachable. Appending the value as it arrives
	// therefore pins every document in the batch in memory for as long as the
	// id list lives — turning a spool bounded by disk into one bounded by RAM,
	// at up to batch.maxBytes per concurrent job. An _id is a few dozen bytes;
	// the document it came in with can be megabytes.
	s.ids = append(s.ids, copyRawValue(doc.ID))

	idText := formatID(doc.ID)
	if s.minID == "" {
		s.minID = idText
	}
	s.maxID = idText

	if doc.TS != nil {
		if s.minTS == nil || doc.TS.Before(*s.minTS) {
			ts := *doc.TS
			s.minTS = &ts
		}
		if s.maxTS == nil || doc.TS.After(*s.maxTS) {
			ts := *doc.TS
			s.maxTS = &ts
		}
	}
	return nil
}

func (s *Spool) DocCount() int64 { return s.docCount }
func (s *Spool) RawBytes() int64 { return s.rawBytes }
func (s *Spool) IsEmpty() bool   { return s.docCount == 0 }

// IDs are the _id values of every document in this batch — the exact set that
// may be deleted once all destinations confirm the upload.
func (s *Spool) IDs() []bson.RawValue { return s.ids }

// Finish flushes the compressor and returns the file's final description.
func (s *Spool) Finish(fileName string) (*Result, error) {
	if s.closed {
		return s.result, nil
	}
	s.closed = true

	if err := s.compressor.Close(); err != nil {
		return nil, fmt.Errorf("flushing compressor: %w", err)
	}
	// The file is about to be read back and uploaded; make sure the bytes are
	// actually on disk and not only in the page cache.
	if err := s.file.Sync(); err != nil {
		return nil, fmt.Errorf("syncing spool file: %w", err)
	}
	if err := s.file.Close(); err != nil {
		return nil, fmt.Errorf("closing spool file: %w", err)
	}

	s.result = &Result{
		Path:            s.path,
		FileName:        fileName,
		DocCount:        s.docCount,
		BytesRaw:        s.rawBytes,
		BytesCompressed: s.counter.n,
		SHA256:          hex.EncodeToString(s.hasher.Sum(nil)),
		MinID:           s.minID,
		MaxID:           s.maxID,
		MinTS:           s.minTS,
		MaxTS:           s.maxTS,
	}
	return s.result, nil
}

// Open reopens the finished file for upload. Each destination gets its own
// handle so uploads can run in parallel without sharing a file offset.
func (s *Spool) Open() (*os.File, error) {
	if !s.closed {
		return nil, fmt.Errorf("spool is still being written")
	}
	return os.Open(s.path)
}

// Discard removes the spool file. Safe to call more than once.
func (s *Spool) Discard() error {
	if !s.closed {
		s.abort()
		return nil
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Spool) abort() {
	if s.compressor != nil {
		_ = s.compressor.Close()
	}
	if s.file != nil {
		_ = s.file.Close()
	}
	_ = os.Remove(s.path)
}

// CleanSpoolDir removes leftover spool files from a previous process. They are
// worthless after a restart: the run that owned them is marked failed and will
// be redone from Mongo, which still holds the data.
func CleanSpoolDir(dir string) (int, error) {
	// Spool files live one level down, in a directory per config, so both
	// levels are swept.
	patterns := []string{
		filepath.Join(dir, "*.tmp"),
		filepath.Join(dir, "*", "*.tmp"),
	}

	removed := 0
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return removed, err
		}
		for _, path := range matches {
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// formatID renders an _id for the catalogue. ObjectIds get their familiar hex
// form; anything else falls back to Extended JSON so the value is unambiguous.
// copyRawValue detaches a RawValue from the buffer it was read out of.
//
// bsoncore is zero-copy by design: LookupErr hands back a slice into the
// document, which is exactly right for a value you use and drop. It is exactly
// wrong for one you keep, because keeping it keeps the whole document.
func copyRawValue(v bson.RawValue) bson.RawValue {
	if len(v.Value) == 0 {
		return v
	}
	value := make([]byte, len(v.Value))
	copy(value, v.Value)
	return bson.RawValue{Type: v.Type, Value: value}
}

func formatID(id bson.RawValue) string {
	if oid, ok := id.ObjectIDOK(); ok {
		return oid.Hex()
	}
	if str, ok := id.StringValueOK(); ok {
		return str
	}
	return id.String()
}

func zstdLevel(level int) zstd.EncoderLevel {
	switch level {
	case 1:
		return zstd.SpeedFastest
	case 3:
		return zstd.SpeedBetterCompression
	case 4:
		return zstd.SpeedBestCompression
	default:
		return zstd.SpeedDefault
	}
}
