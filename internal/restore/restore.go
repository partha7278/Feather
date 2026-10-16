// Package restore reads archives back: to check they are still what feather
// says they are, and to put their documents back into MongoDB.
//
// It is the half of the product that was missing. feather deleted documents on
// the strength of a verification performed once, at write time, and then never
// looked at the object again — so bit-rot, a lifecycle rule expiring the
// archive, an accidental bucket delete or a wrong-key overwrite were all
// undetectable until someone needed the data and found it gone. And the restore
// procedure was seven lines of README prose that no test had ever executed.
//
// Both operations read the whole object and hash it. A restore that did not
// check the digest would be trusting a claim made months ago; a verification
// that did not read the bytes would be trusting metadata feather itself wrote.
package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/retry"
	"github.com/unifers/feather/internal/store"
)

// CopyStatus is what a verification found for one stored copy.
type CopyStatus string

const (
	// CopyOK means the bytes were read and hashed to what the catalogue says.
	CopyOK CopyStatus = "ok"
	// CopyCorrupt means the copy was readable but is not the archive.
	CopyCorrupt CopyStatus = "corrupt"
	// CopyMissing means it could not be read at all.
	CopyMissing CopyStatus = "missing"
	// CopySkipped means feather could not check it — an unknown destination
	// alias, usually because the account was removed after the archive was
	// written. Never treated as a pass.
	CopySkipped CopyStatus = "skipped"
)

// CopyResult is one copy's verdict.
type CopyResult struct {
	Alias     string
	ObjectKey string
	Location  string
	Status    CopyStatus
	Bytes     int64
	SHA256    string
	Err       error
}

// OK reports whether this copy is intact.
func (c CopyResult) OK() bool { return c.Status == CopyOK }

// ArtifactResult is the verdict on one archive file across all its copies.
type ArtifactResult struct {
	Artifact store.Artifact
	Copies   []CopyResult
}

// Intact reports whether at least one copy was read back and matched. That is
// the question that matters after a deletion: not "is every copy fine", but
// "is the data still somewhere".
func (a ArtifactResult) Intact() bool {
	for _, copy := range a.Copies {
		if copy.OK() {
			return true
		}
	}
	return false
}

// Problems lists the copies that are not intact.
func (a ArtifactResult) Problems() []CopyResult {
	var out []CopyResult
	for _, copy := range a.Copies {
		if !copy.OK() {
			out = append(out, copy)
		}
	}
	return out
}

// Registry is the subset of dest.Registry this package needs, so a test can
// supply its own destinations.
type Registry interface {
	Get(alias string) (dest.Destination, error)
}

// VerifyArtifact re-reads every stored copy of one archive and compares it to
// the catalogue.
//
// Every copy is checked even after one has passed. An operator running a scrub
// wants to know that the second and third copies have rotted, not just that the
// first is fine — the whole reason for writing several is that they fail
// independently.
func VerifyArtifact(ctx context.Context, log *slog.Logger, registry Registry,
	artifact store.Artifact, copies []store.ArtifactDestination) ArtifactResult {

	result := ArtifactResult{Artifact: artifact}
	for _, copy := range copies {
		result.Copies = append(result.Copies, verifyCopy(ctx, log, registry, artifact, copy))
	}
	return result
}

func verifyCopy(ctx context.Context, log *slog.Logger, registry Registry,
	artifact store.Artifact, copy store.ArtifactDestination) CopyResult {

	out := CopyResult{Alias: copy.Alias, ObjectKey: copy.ObjectKey, Location: copy.LocationURL}

	target, err := registry.Get(copy.Alias)
	if err != nil {
		out.Status = CopySkipped
		out.Err = fmt.Errorf("destination %q is not configured any more, so this copy cannot be checked: %w", copy.Alias, err)
		return out
	}

	err = retry.Do(ctx, log, retry.DefaultPolicy, "verify "+copy.Alias, func(ctx context.Context) error {
		return target.Verify(ctx, dest.StoredObject{
			Key:    copy.ObjectKey,
			Handle: copy.ETag,
			Size:   artifact.BytesCompressed,
			SHA256: artifact.SHA256,
		})
	})
	if err == nil {
		out.Status = CopyOK
		out.Bytes = artifact.BytesCompressed
		out.SHA256 = artifact.SHA256
		return out
	}

	// "It is not there" and "it is there but wrong" call for different
	// responses — one is a lifecycle rule or a deletion, the other is
	// corruption — so they are reported apart.
	out.Err = err
	out.Status = CopyCorrupt
	if isMissing(err) {
		out.Status = CopyMissing
	}
	return out
}

func isMissing(err error) bool {
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"nosuchkey", "not found", "notfound", "404", "no such file"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// Mode decides what happens to a document whose _id is already in the target
// collection.
type Mode string

const (
	// ModeInsert leaves the existing document alone and counts it as already
	// present. It is the zero value, so every caller that does not ask for
	// anything else gets the restore that cannot overwrite.
	ModeInsert Mode = ""
	// ModeReplace overwrites the existing document with the archived one,
	// matched by _id. A document with no _id is inserted.
	ModeReplace Mode = "replace"
)

// Options configures a restore.
type Options struct {
	// DryRun reads and decodes the archive, and reports what it would insert,
	// without writing anything.
	DryRun bool

	// BatchSize is how many documents are inserted per round trip.
	BatchSize int

	// PreferAlias names the destination to read from. Empty means the first
	// copy that can be read.
	PreferAlias string

	// Mode is insert-only unless set otherwise.
	Mode Mode

	// Progress, when set, is called after every batch with the counts so far.
	// It runs on the restoring goroutine, so it should be quick — the caller
	// that writes progress to the database throttles itself.
	Progress func(Result)
}

// Result summarises a restore.
type Result struct {
	Artifact       store.Artifact
	ReadFrom       string
	DocumentsRead  int64
	Inserted       int64
	AlreadyPresent int64
	// Replaced counts documents that were already there and were overwritten.
	// Always 0 outside ModeReplace.
	Replaced int64
	// BytesRead and SHA256 describe the stored bytes as they were read —
	// compressed, exactly as the archive sits in the bucket or on disk.
	BytesRead int64
	SHA256    string
	DryRun    bool
	Duration  time.Duration
}

const defaultRestoreBatch = 1000

// Artifact restores one archive file into a collection.
//
// The digest is checked as the archive is read, and a mismatch fails the
// restore after the inserts that already happened — which is the right way
// round. Insertion is unordered and duplicate keys are tolerated, so restoring
// twice, or restoring over documents that were never deleted, converges instead
// of failing halfway.
//
// The caller resolves the target collection, which keeps this package free of
// any opinion about how MongoDB accounts are looked up and lets a test pass a
// collection from a throwaway database.
func Artifact(ctx context.Context, log *slog.Logger, registry Registry, into *mongo.Collection,
	artifact store.Artifact, copies []store.ArtifactDestination, opts Options) (Result, error) {

	if into == nil && !opts.DryRun {
		return Result{Artifact: artifact, DryRun: opts.DryRun}, errors.New("no target collection given")
	}
	return restoreArtifact(ctx, registry, artifact, copies, opts, func(r io.Reader) (Result, error) {
		return Stream(ctx, log, into, r, codecFor(artifact), opts)
	})
}

// TableArtifact restores one archive of a PostgreSQL table, with the same
// copy selection and the same digest and count checks as Artifact.
func TableArtifact(ctx context.Context, log *slog.Logger, registry Registry, into TableWriter,
	artifact store.Artifact, copies []store.ArtifactDestination, opts Options) (Result, error) {

	if into == nil && !opts.DryRun {
		return Result{Artifact: artifact, DryRun: opts.DryRun}, errors.New("no target table given")
	}
	return restoreArtifact(ctx, registry, artifact, copies, opts, func(r io.Reader) (Result, error) {
		return StreamTable(ctx, log, into, r, codecFor(artifact), opts)
	})
}

// restoreArtifact is what Artifact and TableArtifact share: find a readable
// copy, stream it into the target, and then hold what was read to what the
// catalogue says was written.
func restoreArtifact(ctx context.Context, registry Registry, artifact store.Artifact,
	copies []store.ArtifactDestination, opts Options, stream func(io.Reader) (Result, error)) (Result, error) {

	started := time.Now()
	result := Result{Artifact: artifact, DryRun: opts.DryRun}

	source, target, err := openCopy(ctx, registry, artifact, copies, opts.PreferAlias)
	if err != nil {
		return result, err
	}
	defer func() { _ = source.Close() }()

	streamed, err := stream(source)
	streamed.Artifact, streamed.ReadFrom = artifact, target
	result = streamed
	if err != nil {
		return result, err
	}

	// Checked last, and it still fails the restore. Documents inserted before
	// the mismatch was detectable stay — they are the operator's data, and
	// removing them again on the way out would be worse than leaving them.
	if artifact.SHA256 != "" && !strings.EqualFold(result.SHA256, artifact.SHA256) {
		return result, fmt.Errorf(
			"the archive read from %s hashes to %s but the catalogue records %s — this copy is not the archive it claims to be; "+
				"%d documents were read and %d inserted before this was detectable",
			target, result.SHA256, artifact.SHA256, result.DocumentsRead, result.Inserted)
	}
	if artifact.DocCount > 0 && result.DocumentsRead != artifact.DocCount {
		return result, fmt.Errorf("the archive holds %d documents but the catalogue records %d",
			result.DocumentsRead, artifact.DocCount)
	}

	result.Duration = time.Since(started)
	return result, nil
}

// Stream restores one archive from r, which must be the stored bytes exactly
// as they were written — compressed with codec, or plain NDJSON for
// archive.CodecNone. It is the part of Artifact that does not care where the
// bytes came from, so an uploaded file goes through the same decoding, batching
// and duplicate handling as a catalogued one.
//
// It checks nothing against a catalogue: there may not be one. The digest and
// size of what was read come back in the Result for the caller to compare, or
// simply to record.
func Stream(ctx context.Context, log *slog.Logger, into *mongo.Collection, r io.Reader,
	codec string, opts Options) (result Result, err error) {

	started := time.Now()
	result = Result{DryRun: opts.DryRun}

	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultRestoreBatch
	}
	if into == nil && !opts.DryRun {
		return result, errors.New("no target collection given")
	}

	reader, err := archive.NewDocumentReader(r, codec)
	if err != nil {
		return result, err
	}
	defer func() { _ = reader.Close() }()

	// Kept current on every return, so a caller recording a failure still
	// sees how far the restore got and what it had hashed.
	snapshot := func() {
		result.BytesRead = reader.CompressedBytes()
		result.SHA256 = reader.SHA256()
		result.Duration = time.Since(started)
	}
	defer snapshot()

	batch := make([]any, 0, opts.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var err error
		if opts.Mode == ModeReplace {
			var inserted, replaced, duplicates int64
			inserted, replaced, duplicates, err = replaceBatch(ctx, log, into, batch, opts.DryRun)
			result.Inserted += inserted
			result.Replaced += replaced
			result.AlreadyPresent += duplicates
		} else {
			var inserted, duplicates int64
			inserted, duplicates, err = insertBatch(ctx, log, into, batch, opts.DryRun)
			result.Inserted += inserted
			result.AlreadyPresent += duplicates
		}
		batch = batch[:0]
		if opts.Progress != nil {
			snapshot()
			opts.Progress(result)
		}
		return err
	}

	for {
		doc, ok := reader.Next()
		if !ok {
			break
		}
		result.DocumentsRead++
		batch = append(batch, doc)
		if len(batch) >= opts.BatchSize {
			if err := flush(); err != nil {
				return result, err
			}
		}
	}
	if err := reader.Err(); err != nil {
		return result, err
	}
	if err := flush(); err != nil {
		return result, err
	}

	// Read whatever is left of the compressed stream so the digest covers the
	// whole file even if the last line ended before the stream did.
	if err := reader.Drain(); err != nil {
		return result, fmt.Errorf("reading the rest of the archive: %w", err)
	}
	return result, nil
}

// TableWriter is where StreamTable puts rows: a PostgreSQL table, in practice
// a *pgsrc.Writer. It is an interface so this package needs no database driver
// to be tested.
type TableWriter interface {
	WriteRows(ctx context.Context, lines [][]byte, replace, dryRun bool) (inserted, replaced, present int64, err error)
}

// StreamTable is Stream for a PostgreSQL archive: each line is a row as
// row_to_json wrote it, handed to the table writer undecoded.
func StreamTable(ctx context.Context, log *slog.Logger, into TableWriter, r io.Reader,
	codec string, opts Options) (result Result, err error) {

	started := time.Now()
	result = Result{DryRun: opts.DryRun}

	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultRestoreBatch
	}
	if into == nil && !opts.DryRun {
		return result, errors.New("no target table given")
	}

	reader, err := archive.NewDocumentReader(r, codec)
	if err != nil {
		return result, err
	}
	defer func() { _ = reader.Close() }()

	snapshot := func() {
		result.BytesRead = reader.CompressedBytes()
		result.SHA256 = reader.SHA256()
		result.Duration = time.Since(started)
	}
	defer snapshot()

	batch := make([][]byte, 0, opts.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var (
			inserted, replaced, present int64
			err                         error
		)
		if into != nil {
			inserted, replaced, present, err = into.WriteRows(ctx, batch, opts.Mode == ModeReplace, opts.DryRun)
		}
		result.Inserted += inserted
		result.Replaced += replaced
		result.AlreadyPresent += present
		if present > 0 {
			log.Debug("rows already present were left alone", "count", present)
		}
		batch = batch[:0]
		if opts.Progress != nil {
			snapshot()
			opts.Progress(result)
		}
		return err
	}

	for {
		line, ok := reader.NextLine()
		if !ok {
			break
		}
		result.DocumentsRead++
		batch = append(batch, line)
		if len(batch) >= opts.BatchSize {
			if err := flush(); err != nil {
				return result, err
			}
		}
	}
	if err := reader.Err(); err != nil {
		return result, err
	}
	if err := flush(); err != nil {
		return result, err
	}
	if err := reader.Drain(); err != nil {
		return result, fmt.Errorf("reading the rest of the archive: %w", err)
	}
	return result, nil
}

// openCopy finds a readable copy, trying each in turn.
func openCopy(ctx context.Context, registry Registry, artifact store.Artifact,
	copies []store.ArtifactDestination, prefer string) (io.ReadCloser, string, error) {

	if len(copies) == 0 {
		return nil, "", fmt.Errorf("archive %s has no recorded copies", artifact.FileName)
	}

	ordered := make([]store.ArtifactDestination, 0, len(copies))
	if prefer != "" {
		for _, copy := range copies {
			if copy.Alias == prefer {
				ordered = append(ordered, copy)
			}
		}
		if len(ordered) == 0 {
			return nil, "", fmt.Errorf("archive %s has no copy on destination %q", artifact.FileName, prefer)
		}
	} else {
		ordered = copies
	}

	var failures []string
	for _, copy := range ordered {
		target, err := registry.Get(copy.Alias)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: not configured", copy.Alias))
			continue
		}
		body, err := target.Open(ctx, copy.ObjectKey, copy.ETag)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", copy.Alias, err))
			continue
		}
		return body, copy.Alias, nil
	}

	// Every copy failed. That is the sentence an operator most needs spelled
	// out, so it names each destination and why.
	return nil, "", fmt.Errorf("no copy of %s could be read:\n  - %s",
		artifact.FileName, strings.Join(failures, "\n  - "))
}

func codecFor(artifact store.Artifact) string {
	if artifact.Codec != "" {
		return artifact.Codec
	}
	return archive.CodecForFileName(artifact.FileName)
}

// insertBatch writes one batch, counting documents that were already there
// rather than failing on them.
func insertBatch(ctx context.Context, log *slog.Logger, into *mongo.Collection, batch []any, dryRun bool) (inserted, duplicates int64, err error) {
	if dryRun {
		return int64(len(batch)), 0, nil
	}

	err = retry.Do(ctx, log, retry.DefaultPolicy, "restore batch", func(ctx context.Context) error {
		// Unordered so one duplicate does not abandon the rest of the batch.
		result, insertErr := into.InsertMany(ctx, batch, options.InsertMany().SetOrdered(false))
		if result != nil {
			inserted = int64(len(result.InsertedIDs))
		}
		if insertErr == nil {
			return nil
		}

		// A document that is already present is not a failure of the restore:
		// it means the source was never deleted, or this archive has been
		// restored before. Restoring has to be repeatable.
		var bulk mongo.BulkWriteException
		var many mongo.BulkWriteException
		if errors.As(insertErr, &bulk) || errors.As(insertErr, &many) {
			remaining := 0
			for _, writeErr := range bulk.WriteErrors {
				if writeErr.Code == duplicateKeyCode {
					duplicates++
					continue
				}
				remaining++
			}
			if remaining == 0 {
				return nil
			}
		}
		return insertErr
	})
	return inserted, duplicates, err
}

// replaceBatch writes one batch in ModeReplace: each document overwrites the
// one with the same _id, or is added if there is none.
//
// Unordered for the same reason as insertBatch, and an upsert that loses a
// race to a concurrent insert of the same _id is counted as already present
// rather than failing the batch.
func replaceBatch(ctx context.Context, log *slog.Logger, into *mongo.Collection, batch []any, dryRun bool) (inserted, replaced, duplicates int64, err error) {
	if dryRun {
		return int64(len(batch)), 0, 0, nil
	}

	models := make([]mongo.WriteModel, 0, len(batch))
	for _, item := range batch {
		doc, ok := item.(bson.Raw)
		if !ok {
			return 0, 0, 0, fmt.Errorf("restore batch item is %T, not bson.Raw", item)
		}
		id, lookupErr := doc.LookupErr("_id")
		if lookupErr != nil {
			models = append(models, mongo.NewInsertOneModel().SetDocument(doc))
			continue
		}
		models = append(models, mongo.NewReplaceOneModel().
			SetFilter(bson.D{{Key: "_id", Value: id}}).
			SetReplacement(doc).
			SetUpsert(true))
	}

	err = retry.Do(ctx, log, retry.DefaultPolicy, "restore batch", func(ctx context.Context) error {
		// Counted per attempt: a retried batch is the whole batch again.
		inserted, replaced, duplicates = 0, 0, 0
		result, writeErr := into.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
		if result != nil {
			inserted = result.InsertedCount + result.UpsertedCount
			replaced = result.MatchedCount
		}
		if writeErr == nil {
			return nil
		}
		var bulk mongo.BulkWriteException
		if errors.As(writeErr, &bulk) {
			remaining := 0
			for _, failure := range bulk.WriteErrors {
				if failure.Code == duplicateKeyCode {
					duplicates++
					continue
				}
				remaining++
			}
			if remaining == 0 {
				return nil
			}
		}
		return writeErr
	})
	return inserted, replaced, duplicates, err
}

// duplicateKeyCode is MongoDB's E11000.
const duplicateKeyCode = 11000
