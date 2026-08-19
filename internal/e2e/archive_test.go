//go:build integration

package e2e

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/unifers/feather/internal/restore"
	"github.com/unifers/feather/internal/store"
)

// The promise, end to end: after a run that deletes, the documents are gone
// from MongoDB and the bytes are in every bucket, hashing to what the catalogue
// recorded, and they restore to the documents that were deleted.
//
// The unit tests prove the runner's ordering with fakes. This proves the fakes
// were not lying about what a real S3 and a real MongoDB do.
func TestArchiveDeleteThenRestoreReturnsTheSameDocuments(t *testing.T) {
	env := newEnv(t)
	const documents = 2000

	originals := env.seed(t, documents)
	job := env.config(t, "orders-archive", true)

	outcome, err := env.app.Runner.Run(t.Context(), job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if outcome.DocsArchived != documents {
		t.Fatalf("archived %d documents, want %d", outcome.DocsArchived, documents)
	}
	if outcome.DocsDeleted != documents {
		t.Fatalf("deleted %d documents, want %d", outcome.DocsDeleted, documents)
	}

	// Gone from MongoDB.
	if remaining := env.count(t); remaining != 0 {
		t.Errorf("%d documents still match the filter after the run", remaining)
	}

	// Present in every bucket, and byte-identical between them.
	artifacts, err := env.app.Store.ArtifactsForRun(t.Context(), outcome.RunID)
	if err != nil {
		t.Fatalf("reading the catalogue: %v", err)
	}
	if len(artifacts) == 0 {
		t.Fatal("the run recorded no archives")
	}
	for _, artifact := range artifacts {
		if len(artifact.Copies) != 3 {
			t.Errorf("%s has %d copies, want 3", artifact.FileName, len(artifact.Copies))
		}
		var reference []byte
		for _, copy := range artifact.Copies {
			body := env.object(t, copy.Bucket, copy.ObjectKey)
			if reference == nil {
				reference = body
				continue
			}
			if string(body) != string(reference) {
				t.Errorf("%s differs between buckets", artifact.FileName)
			}
		}
	}

	// Every copy re-reads and hashes to what the catalogue says.
	for _, artifact := range artifacts {
		result := restore.VerifyArtifact(t.Context(), env.app.Log, env.app.Registry,
			artifact.Artifact, artifact.Copies)
		for _, copy := range result.Copies {
			if !copy.OK() {
				t.Errorf("copy of %s on %s did not verify: %v", artifact.FileName, copy.Alias, copy.Err)
			}
		}
	}

	// And they restore to what was deleted.
	into := env.mongo.Database(testDatabase).Collection("orders_restored")
	for _, artifact := range artifacts {
		if _, err := restore.Artifact(t.Context(), env.app.Log, env.app.Registry, into,
			artifact.Artifact, artifact.Copies, restore.Options{}); err != nil {
			t.Fatalf("restoring %s: %v", artifact.FileName, err)
		}
	}

	restored := env.readAll(t, "orders_restored")
	if len(restored) != len(originals) {
		t.Fatalf("restored %d documents, archived %d", len(restored), len(originals))
	}
	for id, original := range originals {
		back, ok := restored[id]
		if !ok {
			t.Errorf("document %s did not come back", id)
			continue
		}
		// Compared as BSON, so a Date that came back as a string or an int32
		// that came back as an int64 is a failure rather than a near miss.
		if !bytes.Equal(original, back) {
			t.Errorf("document %s changed in the round trip:\n  before %s\n  after  %s",
				id, original.String(), back.String())
		}
	}
}

// The safety property against real infrastructure: point a config at a bucket
// that does not exist, and nothing may be deleted.
func TestNothingIsDeletedWhenARealDestinationIsBroken(t *testing.T) {
	env := newEnv(t)
	const documents = 500
	env.seed(t, documents)

	job := env.config(t, "orders-archive", true)
	job.Destinations = append(job.Destinations, brokenAlias)

	if _, err := env.app.Runner.Run(t.Context(), job, store.TriggerManual); err == nil {
		t.Fatal("the run succeeded even though one destination was unreachable")
	}

	if remaining := env.count(t); remaining != documents {
		t.Fatalf("%d of %d documents survived — a broken destination must delete nothing",
			remaining, documents)
	}
}

// A dry run against real infrastructure must leave both sides untouched.
func TestADryRunTouchesNeitherMongoNorTheBuckets(t *testing.T) {
	env := newEnv(t)
	const documents = 300
	env.seed(t, documents)

	job := env.config(t, "orders-archive", true)
	job.DryRun = true

	if _, err := env.app.Runner.Run(t.Context(), job, store.TriggerManual); err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if remaining := env.count(t); remaining != documents {
		t.Errorf("a dry run deleted documents: %d of %d remain", remaining, documents)
	}
	for _, bucket := range env.rig.Buckets {
		if keys := env.list(t, bucket); len(keys) != 0 {
			t.Errorf("a dry run wrote %d objects to %s", len(keys), bucket)
		}
	}
}

// Corruption after the fact is the failure `feather verify` exists to catch:
// verification at write time says nothing about the object months later.
func TestVerifyDetectsAnArchiveCorruptedAfterItWasWritten(t *testing.T) {
	env := newEnv(t)
	env.seed(t, 200)

	job := env.config(t, "orders-archive", false)
	outcome, err := env.app.Runner.Run(t.Context(), job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	artifacts, err := env.app.Store.ArtifactsForRun(t.Context(), outcome.RunID)
	if err != nil || len(artifacts) == 0 {
		t.Fatalf("reading the catalogue: %v", err)
	}
	artifact := artifacts[0]

	// Everything is fine to begin with.
	before := restore.VerifyArtifact(t.Context(), env.app.Log, env.app.Registry, artifact.Artifact, artifact.Copies)
	if !before.Intact() {
		t.Fatal("a freshly written archive did not verify")
	}

	// Overwrite one copy with something the right length but the wrong bytes,
	// which is what a size-only check would wave through.
	victim := artifact.Copies[0]
	original := env.object(t, victim.Bucket, victim.ObjectKey)
	damaged := append([]byte(nil), original...)
	damaged[len(damaged)/2] ^= 0xFF
	env.putObject(t, victim.Bucket, victim.ObjectKey, damaged)

	after := restore.VerifyArtifact(t.Context(), env.app.Log, env.app.Registry, artifact.Artifact, artifact.Copies)

	var damagedReported bool
	for _, copy := range after.Copies {
		if copy.Alias == victim.Alias {
			damagedReported = !copy.OK()
		}
	}
	if !damagedReported {
		t.Fatal("a corrupted copy verified clean — this is exactly the failure that would " +
			"let feather delete documents whose only archive is unreadable")
	}
	// The other copies are untouched, so the data is still recoverable.
	if !after.Intact() {
		t.Error("the undamaged copies should still verify")
	}
}

// Restoring from a corrupted copy must fail rather than return wrong data.
func TestRestoreRefusesACorruptedCopyAndFallsBackToAGoodOne(t *testing.T) {
	env := newEnv(t)
	env.seed(t, 200)

	job := env.config(t, "orders-archive", false)
	outcome, err := env.app.Runner.Run(t.Context(), job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	artifacts, err := env.app.Store.ArtifactsForRun(t.Context(), outcome.RunID)
	if err != nil || len(artifacts) == 0 {
		t.Fatalf("reading the catalogue: %v", err)
	}
	artifact := artifacts[0]

	victim := artifact.Copies[0]
	original := env.object(t, victim.Bucket, victim.ObjectKey)
	damaged := append([]byte(nil), original...)
	damaged[len(damaged)/2] ^= 0xFF
	env.putObject(t, victim.Bucket, victim.ObjectKey, damaged)

	into := env.mongo.Database(testDatabase).Collection("orders_from_bad_copy")

	// Asked for the damaged copy specifically: it must refuse.
	if _, err := restore.Artifact(t.Context(), env.app.Log, env.app.Registry, into,
		artifact.Artifact, artifact.Copies, restore.Options{PreferAlias: victim.Alias}); err == nil {
		t.Fatal("restoring from a corrupted copy succeeded")
	}

	// A healthy copy still restores.
	var healthy string
	for _, copy := range artifact.Copies[1:] {
		healthy = copy.Alias
		break
	}
	if healthy == "" {
		t.Fatal("no second copy to fall back to")
	}
	if _, err := restore.Artifact(t.Context(), env.app.Log, env.app.Registry, into,
		artifact.Artifact, artifact.Copies, restore.Options{PreferAlias: healthy}); err != nil {
		t.Fatalf("restoring from the good copy failed: %v", err)
	}
}

// Object keys carry the run id, so a second run writes new objects rather than
// overwriting the first — which would destroy an archive whose documents are
// already gone.
func TestASecondRunNeverOverwritesTheFirstRunsObjects(t *testing.T) {
	env := newEnv(t)
	env.seed(t, 200)

	job := env.config(t, "orders-archive", false)
	first, err := env.app.Runner.Run(t.Context(), job, store.TriggerManual)
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	firstKeys := env.list(t, "feather-primary")

	second, err := env.app.Runner.Run(t.Context(), job, store.TriggerManual)
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if first.RunID == second.RunID {
		t.Fatal("both runs share a run id")
	}

	afterKeys := env.list(t, "feather-primary")
	if len(afterKeys) <= len(firstKeys) {
		t.Fatalf("the second run wrote no new objects: %d keys before, %d after",
			len(firstKeys), len(afterKeys))
	}
	for _, key := range firstKeys {
		if !contains(afterKeys, key) {
			t.Errorf("the second run removed or replaced %s", key)
		}
	}
}

// A lease is what stops two processes archiving the same config at once, which
// against a real MongoDB would mean two DeleteMany passes over the same ids.
func TestASecondRunnerIsLockedOutWhileARunIsInFlight(t *testing.T) {
	env := newEnv(t)
	env.seed(t, 100)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	lease, taken, err := env.app.Store.TryLockJob(ctx, "orders-archive")
	if err != nil {
		t.Fatalf("taking the lease: %v", err)
	}
	if !taken {
		t.Fatal("the lease was not available on an idle config")
	}
	defer lease.Release(context.WithoutCancel(ctx))

	job := env.config(t, "orders-archive", true)
	if _, err := env.app.Runner.Run(ctx, job, store.TriggerManual); err == nil {
		t.Fatal("a run started while another process held the lease")
	}
	if remaining := env.count(t); remaining != 100 {
		t.Errorf("%d of 100 documents survived — a locked-out run must delete nothing", remaining)
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
