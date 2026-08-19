package runner

import (
	"errors"
	"testing"

	"github.com/unifers/feather/internal/store"
)

// This is the promise feather makes: documents go only after every copy is
// stored and verified. Until now it was asserted by a comment.
//
// Each case fails a destination at a different point. In none of them may a
// single document be deleted.
func TestNothingIsDeletedWhenADestinationFails(t *testing.T) {
	cases := []struct {
		name    string
		failure func(*fakeDestination)
	}{
		{"upload fails", func(d *fakeDestination) { d.uploadErr = errors.New("connection reset by peer") }},
		{"verify fails", func(d *fakeDestination) { d.verifyErr = errors.New("checksum does not match") }},
		{"the stored copy is corrupt", func(d *fakeDestination) { d.corrupt = true }},
		{"the copy vanishes before verification", func(d *fakeDestination) { d.vanish = true }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, 250)
			rig.job.Retention.DeleteAfterArchive = true
			tc.failure(rig.destinations["backup"])

			_, err := rig.runner.Run(t.Context(), rig.job, store.TriggerManual)
			if err == nil {
				t.Fatal("the run reported success even though a destination failed")
			}

			if rig.source.deleted != 0 {
				t.Fatalf("%d documents were deleted after a destination failed — "+
					"this is the failure the whole design exists to prevent", rig.source.deleted)
			}
			if calls := rig.source.deleteCalls; calls != 0 {
				t.Fatalf("DeleteByIDs was called %d times after a destination failed", calls)
			}
		})
	}
}

// The healthy path, so the test above cannot pass by never deleting anything.
func TestDocumentsAreDeletedOnceEveryCopyIsVerified(t *testing.T) {
	rig := newRig(t, 250)
	rig.job.Retention.DeleteAfterArchive = true

	outcome, err := rig.runner.Run(t.Context(), rig.job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if outcome.DocsArchived != 250 {
		t.Errorf("archived %d documents, want 250", outcome.DocsArchived)
	}
	if rig.source.deleted != 250 {
		t.Errorf("deleted %d documents, want 250", rig.source.deleted)
	}
	for alias, destination := range rig.destinations {
		if destination.uploads() == 0 {
			t.Errorf("destination %s received nothing", alias)
		}
		if destination.verifies() == 0 {
			t.Errorf("destination %s was never verified", alias)
		}
	}
}

// Deletion off makes feather a backup tool. The switch has to be honoured
// exactly, because the difference between the two modes is whether the original
// still exists.
func TestDeletionOffArchivesAndDeletesNothing(t *testing.T) {
	rig := newRig(t, 100)
	rig.job.Retention.DeleteAfterArchive = false

	outcome, err := rig.runner.Run(t.Context(), rig.job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if outcome.DocsArchived != 100 {
		t.Errorf("archived %d, want 100", outcome.DocsArchived)
	}
	if rig.source.deleteCalls != 0 {
		t.Error("documents were deleted by a config with deletion switched off")
	}
}

// A dry run must touch nothing at all — that is what makes it safe to run
// against a config that deletes.
func TestADryRunUploadsNothingAndDeletesNothing(t *testing.T) {
	rig := newRig(t, 100)
	rig.job.Retention.DeleteAfterArchive = true
	rig.job.DryRun = true

	if _, err := rig.runner.Run(t.Context(), rig.job, store.TriggerManual); err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	for alias, destination := range rig.destinations {
		if destination.uploads() != 0 {
			t.Errorf("dry run uploaded to %s", alias)
		}
	}
	if rig.source.deleteCalls != 0 {
		t.Error("a dry run deleted documents")
	}
}

// Every destination gets a complete copy. Mixing providers is the whole point:
// two copies that fail for different reasons.
func TestEveryDestinationReceivesACompleteCopy(t *testing.T) {
	rig := newRig(t, 300)

	if _, err := rig.runner.Run(t.Context(), rig.job, store.TriggerManual); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	var reference map[string][]byte
	for alias, destination := range rig.destinations {
		stored := destination.stored()
		if reference == nil {
			reference = stored
			continue
		}
		if len(stored) != len(reference) {
			t.Fatalf("%s holds %d objects, another destination holds %d", alias, len(stored), len(reference))
		}
		for key, body := range reference {
			other, ok := stored[key]
			if !ok {
				t.Errorf("%s is missing %s", alias, key)
				continue
			}
			if string(other) != string(body) {
				t.Errorf("%s holds different bytes for %s", alias, key)
			}
		}
	}
}

// The catalogue is how an operator answers "where did the January orders go?".
// A run that deleted documents must leave a complete record of where they went.
func TestTheCatalogueRecordsEveryCopyAndTheDeletion(t *testing.T) {
	rig := newRig(t, 200)
	rig.job.Retention.DeleteAfterArchive = true

	outcome, err := rig.runner.Run(t.Context(), rig.job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	artifacts, err := rig.store.ArtifactsForRun(t.Context(), outcome.RunID)
	if err != nil {
		t.Fatalf("reading the catalogue: %v", err)
	}
	if len(artifacts) == 0 {
		t.Fatal("the run recorded no archives")
	}

	var archived, deleted int64
	for _, artifact := range artifacts {
		archived += artifact.DocCount
		deleted += artifact.DeletedCount

		if !artifact.DeletedFromSource {
			t.Errorf("%s is not recorded as deleted from source", artifact.FileName)
		}
		if artifact.SHA256 == "" {
			t.Errorf("%s has no checksum recorded, so it can never be verified", artifact.FileName)
		}
		if len(artifact.Copies) != len(rig.destinations) {
			t.Errorf("%s records %d copies, want %d", artifact.FileName, len(artifact.Copies), len(rig.destinations))
		}
		for _, copy := range artifact.Copies {
			if copy.Status != store.DestVerified {
				t.Errorf("copy of %s on %s is %q, want %q",
					artifact.FileName, copy.Alias, copy.Status, store.DestVerified)
			}
		}
	}

	if archived != 200 {
		t.Errorf("catalogue accounts for %d archived documents, want 200", archived)
	}
	if deleted != 200 {
		t.Errorf("catalogue accounts for %d deleted documents, want 200", deleted)
	}
}

// A run split across several files must still delete exactly what it archived,
// and each file must be separately addressable.
func TestABatchedRunArchivesAndDeletesEveryDocumentExactlyOnce(t *testing.T) {
	rig := newRig(t, 500)
	rig.job.Retention.DeleteAfterArchive = true
	rig.job.Batch.MaxDocs = 60

	outcome, err := rig.runner.Run(t.Context(), rig.job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if outcome.Batches < 2 {
		t.Fatalf("expected the run to split into batches, got %d", outcome.Batches)
	}
	if outcome.DocsArchived != 500 || rig.source.deleted != 500 {
		t.Errorf("archived %d and deleted %d, want 500 of each", outcome.DocsArchived, rig.source.deleted)
	}

	// Object keys carry the run id and the batch number, so no batch can
	// overwrite another.
	seen := map[string]bool{}
	for key := range rig.destinations["primary"].stored() {
		if seen[key] {
			t.Errorf("two batches were written to the same key %s", key)
		}
		seen[key] = true
	}
	if len(seen) != outcome.Batches {
		t.Errorf("%d objects for %d batches", len(seen), outcome.Batches)
	}
}

// Deleting only what was archived matters when a document stops matching the
// filter between being read and being deleted.
func TestOnlyArchivedDocumentsAreOfferedForDeletion(t *testing.T) {
	rig := newRig(t, 120)
	rig.job.Retention.DeleteAfterArchive = true

	if _, err := rig.runner.Run(t.Context(), rig.job, store.TriggerManual); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if len(rig.source.offeredIDs) != 120 {
		t.Fatalf("%d ids were offered for deletion, want 120", len(rig.source.offeredIDs))
	}
	unique := map[string]bool{}
	for _, id := range rig.source.offeredIDs {
		key := id.String()
		if unique[key] {
			t.Errorf("id %s was offered for deletion twice", key)
		}
		unique[key] = true
	}
}
