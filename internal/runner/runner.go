package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"golang.org/x/sync/errgroup"

	"github.com/unifers/feather/internal/archive"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/mongox"
	"github.com/unifers/feather/internal/store"
)

// Runner executes retention jobs. One instance is shared by the scheduler, the
// web UI, and manual CLI invocations.
type Runner struct {
	env      *config.Env
	mongo    *mongox.Pool
	store    *store.Store
	registry *dest.Registry
	host     string
	log      *slog.Logger
}

func New(env *config.Env, pool *mongox.Pool, st *store.Store, registry *dest.Registry, log *slog.Logger) *Runner {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return &Runner{env: env, mongo: pool, store: st, registry: registry, host: host, log: log}
}

// Outcome summarises a finished run for the caller.
type Outcome struct {
	RunID        uuid.UUID
	Status       store.RunStatus
	DocsScanned  int64
	DocsArchived int64
	DocsDeleted  int64
	Batches      int
	BytesRaw     int64
	BytesStored  int64
	Skipped      bool
	Err          error
}

// ErrLocked means another run of the same config is already in flight.
var ErrLocked = errors.New("another run of this config is already in progress")

// Run executes one retention job end to end.
//
// The ordering is the safety property: documents are read, written to a
// compressed local file, uploaded to every destination, verified by reading the
// stored object back, and only then deleted from Mongo. A failure at any point
// leaves the source data untouched.
func (r *Runner) Run(ctx context.Context, job *config.JobConfig, trigger store.TriggerKind) (*Outcome, error) {
	log := r.log.With("config", job.Name, "collection", job.Source.Database+"."+job.Source.Collection)

	lock, acquired, err := r.store.TryLockJob(ctx, job.Name)
	if err != nil {
		return nil, err
	}
	if !acquired {
		log.Warn("skipping run, config is already running elsewhere")
		return &Outcome{Status: store.RunSkipped, Skipped: true}, ErrLocked
	}
	defer lock.Release(ctx)

	// A run is bounded so a pathological job cannot hold the lock forever.
	ctx, cancel := context.WithTimeout(ctx, job.Limits.MaxRuntime.Std())
	defer cancel()

	resolvedFilter, err := config.ResolveTimeTokens(job.Source.Filter, time.Now())
	if err != nil {
		return nil, fmt.Errorf("resolving filter for %s: %w", job.Name, err)
	}
	// The config file's formatting would otherwise put newlines through the log
	// line and the catalogue row.
	var compactFilter bytes.Buffer
	if err := json.Compact(&compactFilter, resolvedFilter); err == nil {
		resolvedFilter = compactFilter.Bytes()
	}
	filter, err := mongox.ParseFilter(resolvedFilter)
	if err != nil {
		return nil, err
	}
	projection, err := mongox.ParseProjection(job.Source.Projection)
	if err != nil {
		return nil, err
	}

	run := &store.Run{
		ID:           uuid.New(),
		ConfigName:   job.Name,
		ConfigSHA256: job.SHA256,
		Database:     job.Source.Database,
		Collection:   job.Source.Collection,
		FilterJSON:   string(resolvedFilter),
		Trigger:      trigger,
		Host:         r.host,
		DryRun:       job.DryRun,
	}
	if err := r.store.StartRun(ctx, run); err != nil {
		return nil, err
	}
	log = log.With("run_id", run.ID)
	log.Info("run started", "filter", string(resolvedFilter), "dry_run", job.DryRun,
		"destinations", job.Destinations, "delete_after_archive", job.Retention.DeleteAfterArchive)

	outcome, runErr := r.execute(ctx, log, job, run, filter, projection)

	run.Status = store.RunSuccess
	switch {
	case job.DryRun:
		run.Status = store.RunDryRun
	case runErr != nil:
		run.Status = store.RunFailed
		run.Error = runErr.Error()
		// Batches that completed before the failure did upload and delete
		// correctly, so the run is partial rather than a total loss.
		if run.BatchCount > 0 {
			run.Status = store.RunPartial
		}
	}

	// The run row must be closed even if the run's context expired, otherwise it
	// stays "running" forever.
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer closeCancel()
	if err := r.store.FinishRun(closeCtx, run); err != nil {
		log.Error("could not close run row", "error", err)
	}

	outcome.RunID = run.ID
	outcome.Status = run.Status
	outcome.Err = runErr

	log.Info("run finished",
		"status", run.Status,
		"scanned", run.DocsScanned,
		"archived", run.DocsArchived,
		"deleted", run.DocsDeleted,
		"batches", run.BatchCount,
		"raw", config.HumanBytes(run.BytesRaw),
		"stored", config.HumanBytes(run.BytesCompressed),
		"error", runErr)

	return outcome, runErr
}

func (r *Runner) execute(ctx context.Context, log *slog.Logger, job *config.JobConfig, run *store.Run, filter, projection bson.D) (*Outcome, error) {
	outcome := &Outcome{}

	targets, err := r.registry.Resolve(job.Destinations)
	if err != nil {
		return outcome, err
	}

	client, err := r.mongo.Get(ctx, job.Source.MongoSource)
	if err != nil {
		return outcome, err
	}
	coll, err := client.Collection(job.Source.Database, job.Source.Collection, job.Source.ReadPreference)
	if err != nil {
		return outcome, err
	}

	matching, err := mongox.Count(ctx, coll, filter)
	if err != nil {
		return outcome, fmt.Errorf("counting matching documents: %w", err)
	}
	log.Info("documents matched", "count", matching)
	if matching == 0 {
		return outcome, nil
	}

	cursor, err := mongox.OpenCursor(ctx, coll, filter, projection, job.Batch.FetchSize, job.Source.TimestampField)
	if err != nil {
		return outcome, err
	}
	defer func() {
		if err := cursor.Close(context.WithoutCancel(ctx)); err != nil {
			log.Warn("closing cursor", "error", err)
		}
	}()

	spoolDir := filepath.Join(r.env.SpoolDir, job.Name)
	seq := 0

	for {
		batch, more, err := r.fillBatch(ctx, cursor, job, spoolDir, run)
		if err != nil {
			return outcome, err
		}
		if batch == nil {
			break
		}

		seq++
		err = r.shipBatch(ctx, log, job, run, coll, filter, targets, batch, seq)
		discardErr := batch.spool.Discard()
		if err != nil {
			return outcome, err
		}
		if discardErr != nil {
			log.Warn("could not remove spool file", "path", batch.result.Path, "error", discardErr)
		}

		run.BatchCount = seq
		if err := r.store.UpdateRunProgress(ctx, run); err != nil {
			log.Warn("could not flush run progress", "error", err)
		}

		if !more {
			break
		}
		if job.Limits.MaxDocsPerRun > 0 && run.DocsScanned >= int64(job.Limits.MaxDocsPerRun) {
			log.Info("reached limits.maxDocsPerRun, stopping this run", "limit", job.Limits.MaxDocsPerRun)
			break
		}
	}

	outcome.DocsScanned = run.DocsScanned
	outcome.DocsArchived = run.DocsArchived
	outcome.DocsDeleted = run.DocsDeleted
	outcome.Batches = run.BatchCount
	outcome.BytesRaw = run.BytesRaw
	outcome.BytesStored = run.BytesCompressed
	return outcome, nil
}

type batch struct {
	spool  *archive.Spool
	result *archive.Result
}

// fillBatch reads documents into a fresh spool file until a size or count limit
// is reached. The returned bool reports whether the cursor has more to give.
func (r *Runner) fillBatch(ctx context.Context, cursor *mongox.Cursor, job *config.JobConfig, spoolDir string, run *store.Run) (*batch, bool, error) {
	spool, err := archive.NewSpool(spoolDir, job.Name, job.Compression)
	if err != nil {
		return nil, false, err
	}

	remaining := int64(-1)
	if job.Limits.MaxDocsPerRun > 0 {
		remaining = int64(job.Limits.MaxDocsPerRun) - run.DocsScanned
		if remaining <= 0 {
			_ = spool.Discard()
			return nil, false, nil
		}
	}

	more := true
	for {
		if spool.DocCount() >= int64(job.Batch.MaxDocs) || spool.RawBytes() >= int64(job.Batch.MaxBytes) {
			break
		}
		if remaining >= 0 && spool.DocCount() >= remaining {
			break
		}

		doc, ok, err := cursor.Next(ctx)
		if err != nil {
			_ = spool.Discard()
			return nil, false, err
		}
		if !ok {
			more = false
			break
		}
		if err := spool.Add(doc); err != nil {
			_ = spool.Discard()
			return nil, false, err
		}
		run.DocsScanned++
	}

	if spool.IsEmpty() {
		_ = spool.Discard()
		return nil, false, nil
	}
	return &batch{spool: spool}, more, nil
}

// shipBatch uploads one spooled file everywhere it belongs, verifies each copy,
// and deletes the source documents once all copies are confirmed.
func (r *Runner) shipBatch(ctx context.Context, log *slog.Logger, job *config.JobConfig, run *store.Run,
	coll *mongo.Collection, filter bson.D, targets []dest.Destination, b *batch, seq int) error {

	baseKey := config.RenderKey(job.Output.KeyTemplate, config.KeyVars{
		Database:   job.Source.Database,
		Collection: job.Source.Collection,
		Config:     job.Name,
		RunID:      run.ID.String(),
		Seq:        seq,
		Extension:  job.Extension(),
		At:         run.StartedAt,
	})

	result, err := b.spool.Finish(filepath.Base(baseKey))
	if err != nil {
		return err
	}
	b.result = result

	log = log.With("batch", seq, "docs", result.DocCount)
	log.Info("batch spooled",
		"raw", config.HumanBytes(result.BytesRaw),
		"compressed", config.HumanBytes(result.BytesCompressed),
		"ratio", compressionRatio(result),
		"sha256", result.SHA256)

	if job.DryRun {
		for _, target := range targets {
			log.Info("dry run: would upload", "destination", target.Alias(),
				"bucket", target.Info().Bucket, "key", target.Key(baseKey))
		}
		if job.Retention.DeleteAfterArchive {
			log.Info("dry run: would delete these documents from mongo")
		}
		run.DocsArchived += result.DocCount
		run.BytesRaw += result.BytesRaw
		run.BytesCompressed += result.BytesCompressed
		return nil
	}

	artifact := &store.Artifact{
		RunID:           run.ID,
		ConfigName:      job.Name,
		Database:        job.Source.Database,
		Collection:      job.Source.Collection,
		BatchSeq:        seq,
		FileName:        result.FileName,
		Format:          job.Output.Format,
		Codec:           job.Compression.Codec,
		DocCount:        result.DocCount,
		BytesRaw:        result.BytesRaw,
		BytesCompressed: result.BytesCompressed,
		SHA256:          result.SHA256,
		MinID:           result.MinID,
		MaxID:           result.MaxID,
		MinTS:           result.MinTS,
		MaxTS:           result.MaxTS,
	}
	if err := r.store.InsertArtifact(ctx, artifact); err != nil {
		return err
	}

	if err := r.uploadEverywhere(ctx, log, job, run, targets, b, artifact, baseKey); err != nil {
		return err
	}

	run.DocsArchived += result.DocCount
	run.BytesRaw += result.BytesRaw
	run.BytesCompressed += result.BytesCompressed

	if !job.Retention.DeleteAfterArchive {
		return nil
	}

	// Every copy is stored and verified; the source documents may now go.
	deleted, err := mongox.DeleteByIDs(ctx, coll, filter, b.spool.IDs(), job.Retention.DeleteBatchSize)
	if err != nil {
		return err
	}
	run.DocsDeleted += deleted

	if err := r.store.MarkArtifactDeleted(ctx, artifact.ID, deleted); err != nil {
		log.Warn("could not record deletion in catalogue", "error", err)
	}

	// A shortfall means some documents stopped matching the filter between the
	// read and the delete. They are intentionally left in place.
	if deleted < result.DocCount {
		log.Info("some archived documents no longer matched the filter and were kept",
			"archived", result.DocCount, "deleted", deleted)
	}
	log.Info("batch complete", "deleted", deleted)
	return nil
}

// uploadEverywhere puts the batch on every destination in parallel and verifies
// each one. Any failure aborts before deletion — that is the whole point.
func (r *Runner) uploadEverywhere(ctx context.Context, log *slog.Logger, job *config.JobConfig, run *store.Run,
	targets []dest.Destination, b *batch, artifact *store.Artifact, baseKey string) error {

	meta := dest.Metadata{
		SHA256:     b.result.SHA256,
		DocCount:   b.result.DocCount,
		Config:     job.Name,
		RunID:      run.ID.String(),
		Collection: job.Source.Database + "." + job.Source.Collection,
	}

	rows := make([]*store.ArtifactDestination, len(targets))
	for i, target := range targets {
		info := target.Info()
		rows[i] = &store.ArtifactDestination{
			ArtifactID:   artifact.ID,
			Alias:        info.Alias,
			Provider:     info.Provider,
			Bucket:       info.Bucket,
			ObjectKey:    target.Key(baseKey),
			Region:       info.Region,
			Endpoint:     info.Endpoint,
			StorageClass: info.StorageClass,
			SizeBytes:    b.result.BytesCompressed,
		}
		if err := r.store.InsertArtifactDestination(ctx, rows[i]); err != nil {
			return err
		}
	}

	group, groupCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex

	for i, target := range targets {
		row := rows[i]
		group.Go(func() error {
			err := r.uploadOne(groupCtx, target, row, b, meta, job.ShouldVerify())
			// Recording the failure uses the parent context: the group's context
			// is already cancelled by the time we get here.
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if markErr := r.store.MarkDestinationFailed(context.WithoutCancel(ctx), row.ID, describeUploadError(ctx, err)); markErr != nil {
					log.Warn("could not record destination failure", "destination", row.Alias, "error", markErr)
				}
				return err
			}
			log.Info("copy verified", "destination", row.Alias, "bucket", row.Bucket, "key", row.ObjectKey)
			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return fmt.Errorf("batch not stored everywhere, keeping source documents: %w", err)
	}
	return nil
}

func (r *Runner) uploadOne(ctx context.Context, target dest.Destination, row *store.ArtifactDestination,
	b *batch, meta dest.Metadata, verify bool) error {

	// Each destination reads the spool file through its own handle so the
	// parallel uploads do not share a file offset.
	file, err := b.spool.Open()
	if err != nil {
		return err
	}
	defer file.Close()

	etag, err := target.Upload(ctx, row.ObjectKey, file, b.result.BytesCompressed, meta)
	if err != nil {
		return err
	}
	row.ETag = etag
	if err := r.store.MarkDestinationUploaded(ctx, row.ID, etag, b.result.BytesCompressed); err != nil {
		return err
	}

	if !verify {
		return nil
	}
	if err := target.Verify(ctx, row.ObjectKey, b.result.BytesCompressed, b.result.SHA256); err != nil {
		return err
	}
	return r.store.MarkDestinationVerified(ctx, row.ID)
}

// describeUploadError keeps the catalogue honest about cause. When one
// destination fails, the shared context cancels the others mid-transfer; those
// siblings would otherwise be recorded with a bare "context canceled", which
// reads like three independent failures instead of one.
func describeUploadError(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return "aborted: another destination for this batch failed, so this upload was stopped"
	}
	return err.Error()
}

func compressionRatio(result *archive.Result) string {
	if result.BytesCompressed == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1fx", float64(result.BytesRaw)/float64(result.BytesCompressed))
}
