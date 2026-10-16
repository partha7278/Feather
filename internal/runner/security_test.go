package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/dest"
	"github.com/unifers/feather/internal/mongox"
	"github.com/unifers/feather/internal/store"
)

// Two runs of one config deleting from the same collection at once is the
// failure the job lease exists to prevent. These tests hold a run open inside
// the source and throw more runs at the same config — from the same process
// and from a second process sharing the database.

// gatedSource parks the first MaxID call until released, so a run is provably
// in flight while the others try to start.
type gatedSource struct {
	*fakeSource
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedSource) MaxID(ctx context.Context, filter bson.D) (bson.RawValue, bool, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.fakeSource.MaxID(ctx, filter)
}

// panickingSource blows up mid-run, standing in for any bug below the runner.
type panickingSource struct{ *fakeSource }

func (p panickingSource) Count(context.Context, bson.D) (int64, error) {
	panic("source exploded")
}

func TestConcurrentRunsOfOneConfigNeverOverlap(t *testing.T) {
	rig := newRig(t, 25)
	rig.job.Retention.DeleteAfterArchive = true

	gate := &gatedSource{fakeSource: rig.source, entered: make(chan struct{}), release: make(chan struct{})}
	sourceFor := func(context.Context, *config.JobConfig) (mongox.Source, error) { return gate, nil }
	rig.runner.sourceFor = sourceFor

	// A second feather process: its own store handle and runner over the same
	// database file and the same destinations.
	otherStore, err := store.Open(t.Context(), rig.store.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(otherStore.Close)
	byAlias := map[string]dest.Destination{}
	for alias, d := range rig.destinations {
		byAlias[alias] = d
	}
	other := New(&config.Env{SpoolDir: t.TempDir()}, nil, otherStore, dest.NewRegistryWith(byAlias), discardLog())
	other.sourceFor = sourceFor
	other.retry.Attempts = 1

	type result struct {
		outcome *Outcome
		err     error
	}
	first := make(chan result, 1)
	go func() {
		outcome, err := rig.runner.Run(context.Background(), rig.job, store.TriggerManual)
		first <- result{outcome, err}
	}()

	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first run never reached the source")
	}

	const contenders = 16
	var wg sync.WaitGroup
	results := make(chan result, contenders)
	for i := range contenders {
		r := rig.runner
		if i%2 == 1 {
			r = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcome, err := r.Run(context.Background(), rig.job, store.TriggerCron)
			results <- result{outcome, err}
		}()
	}
	wg.Wait()
	close(results)

	for res := range results {
		if !errors.Is(res.err, ErrLocked) {
			t.Errorf("a run started while another held the config: err=%v", res.err)
			continue
		}
		if res.outcome == nil || !res.outcome.Skipped || res.outcome.DocsDeleted != 0 {
			t.Errorf("a refused run reported work: %+v", res.outcome)
		}
	}

	close(gate.release)
	got := <-first
	if got.err != nil {
		t.Fatalf("the run holding the lease failed: %v", got.err)
	}
	if got.outcome.DocsDeleted != 25 {
		t.Errorf("the run deleted %d documents, want 25", got.outcome.DocsDeleted)
	}
	if rig.source.deleted != 25 {
		t.Errorf("the source saw %d deletions in total, want exactly 25", rig.source.deleted)
	}

	// The lease is free again for whoever comes next, in either process.
	lock, ok, err := otherStore.TryLockJob(t.Context(), rig.job.Name)
	if err != nil || !ok {
		t.Fatalf("the lease was not released after the run: %v %v", ok, err)
	}
	lock.Release(t.Context())
}

// A panic somewhere below the runner must not leave the config locked: the
// lease is the only thing standing between the next run and a permanently
// stuck schedule.
func TestAPanickingRunReleasesItsLease(t *testing.T) {
	rig := newRig(t, 5)
	rig.runner.sourceFor = func(context.Context, *config.JobConfig) (mongox.Source, error) {
		return panickingSource{rig.source}, nil
	}

	func() {
		defer func() { _ = recover() }()
		_, _ = rig.runner.Run(context.Background(), rig.job, store.TriggerManual)
		t.Fatal("the panic did not surface")
	}()

	lock, ok, err := rig.store.TryLockJob(t.Context(), rig.job.Name)
	if err != nil || !ok {
		t.Fatalf("the config is still locked after a panicking run: ok=%v err=%v", ok, err)
	}
	lock.Release(t.Context())

	if rig.source.deleted != 0 {
		t.Errorf("a run that panicked before archiving deleted %d documents", rig.source.deleted)
	}
}

// A config whose key template could overwrite an earlier run's archive is
// refused before the lease is even taken, so it cannot block a legitimate run.
func TestAnUnsafeConfigIsRefusedWithoutTakingTheLease(t *testing.T) {
	rig := newRig(t, 5)
	rig.job.Output.KeyTemplate = "archive/{config}{ext}" // no {runId}

	if _, err := rig.runner.Run(context.Background(), rig.job, store.TriggerManual); err == nil {
		t.Fatal("an unsafe config was run")
	}
	running, err := rig.store.RunningConfigs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if running[rig.job.Name] {
		t.Error("the refused config was left holding a lease")
	}
	if rig.source.deleted != 0 {
		t.Errorf("a refused config deleted %d documents", rig.source.deleted)
	}
}
