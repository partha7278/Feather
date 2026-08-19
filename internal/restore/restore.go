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
}

// Result summarises a restore.
type Result struct {
	Artifact       store.Artifact
	ReadFrom       string
	DocumentsRead  int64
	Inserted       int64
	AlreadyPresent int64
	DryRun         bool
	Duration       time.Duration
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

	started := time.Now()
	result := Result{Artifact: artifact, DryRun: opts.DryRun}

	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultRestoreBatch
	}
	if into == nil && !opts.DryRun {
		return result, errors.New("no target collection given")
	}

	source, target, err := openCopy(ctx, registry, artifact, copies, opts.PreferAlias)
	if err != nil {
		return result, err
	}
	defer func() { _ = source.Close() }()
	result.ReadFrom = target

	reader, err := archive.NewDocumentReader(source, codecFor(artifact))
	if err != nil {
		return result, err
	}
	defer func() { _ = reader.Close() }()

	batch := make([]any, 0, opts.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		inserted, duplicates, err := insertBatch(ctx, log, into, batch, opts.DryRun)
		result.Inserted += inserted
		result.AlreadyPresent += duplicates
		batch = batch[:0]
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

	// Checked last, and it still fails the restore. Documents inserted before
	// the mismatch was detectable stay — they are the operator's data, and
	// removing them again on the way out would be worse than leaving them.
	if artifact.SHA256 != "" && !strings.EqualFold(reader.SHA256(), artifact.SHA256) {
		return result, fmt.Errorf(
			"the archive read from %s hashes to %s but the catalogue records %s — this copy is not the archive it claims to be; "+
				"%d documents were read and %d inserted before this was detectable",
			target, reader.SHA256(), artifact.SHA256, result.DocumentsRead, result.Inserted)
	}
	if artifact.DocCount > 0 && result.DocumentsRead != artifact.DocCount {
		return result, fmt.Errorf("the archive holds %d documents but the catalogue records %d",
			result.DocumentsRead, artifact.DocCount)
	}

	result.Duration = time.Since(started)
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

// duplicateKeyCode is MongoDB's E11000.
const duplicateKeyCode = 11000
