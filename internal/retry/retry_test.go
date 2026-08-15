package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A fast policy: these tests care about how many attempts happen and why, not
// about how long the waits are.
var fast = Policy{Attempts: 4, Base: time.Millisecond, Max: 2 * time.Millisecond}

func TestDoStopsAtTheFirstSuccess(t *testing.T) {
	calls := 0
	err := Do(context.Background(), nil, fast, "upload", func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("a successful operation ran %d times", calls)
	}
}

func TestDoRetriesUntilItSucceeds(t *testing.T) {
	calls := 0
	err := Do(context.Background(), nil, fast, "upload", func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("503 slow down")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("an operation that recovered on the third try reported failure: %v", err)
	}
	if calls != 3 {
		t.Errorf("the operation ran %d times, want 3", calls)
	}
}

func TestDoGivesUpAndReportsTheLastError(t *testing.T) {
	sentinel := errors.New("connection reset")
	calls := 0

	err := Do(context.Background(), nil, fast, "upload", func(context.Context) error {
		calls++
		return sentinel
	})
	if err == nil {
		t.Fatal("a persistently failing operation reported success")
	}
	if calls != fast.Attempts {
		t.Errorf("the operation ran %d times, want %d", calls, fast.Attempts)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("the underlying cause was lost: %v", err)
	}
}

// The point of Permanent: a rejected credential or a missing bucket does not
// improve by waiting, and retrying it only delays the report the operator needs.
func TestDoDoesNotRetryAPermanentFailure(t *testing.T) {
	sentinel := errors.New("AccessDenied")
	calls := 0

	err := Do(context.Background(), nil, fast, "upload", func(context.Context) error {
		calls++
		return Permanent(sentinel)
	})
	if calls != 1 {
		t.Errorf("a permanent failure was retried %d times", calls-1)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Permanent lost the underlying error: %v", err)
	}
}

// A cancelled context means the run is over — its deadline passed, or the
// process is shutting down. Sleeping through a backoff at that point delays
// shutdown for no possible benefit.
func TestDoStopsWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	err := Do(ctx, nil, Policy{Attempts: 5, Base: time.Hour, Max: time.Hour}, "upload",
		func(context.Context) error {
			calls++
			cancel()
			return errors.New("transient")
		})
	if err == nil {
		t.Fatal("a cancelled operation reported success")
	}
	if calls != 1 {
		t.Errorf("the operation ran %d times after cancellation", calls)
	}
}

func TestDoRespectsCancellationDuringTheBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Do(ctx, nil, Policy{Attempts: 5, Base: time.Hour, Max: time.Hour}, "upload",
		func(context.Context) error { return errors.New("transient") })

	if err == nil {
		t.Fatal("expected a failure")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the error does not say the deadline passed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancellation waited out the backoff (%s)", elapsed)
	}
}

func TestDoTreatsContextErrorsAsPermanent(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if Retryable(err) {
			t.Errorf("%v was treated as worth retrying", err)
		}
	}
	// The default has to be yes: an object store's failures are overwhelmingly
	// transient, and one wasted retry costs seconds against a lost run.
	if !Retryable(errors.New("503 Service Unavailable")) {
		t.Error("an unclassified error was treated as permanent")
	}
	if Retryable(nil) {
		t.Error("a nil error was treated as worth retrying")
	}
}

// Full jitter: every delay is somewhere in the top half of an exponentially
// growing window, capped by Max. The jitter matters because a batch uploads to
// several destinations at once, usually the same service — a fixed backoff
// would have them all come back in step and collide again.
func TestBackoffGrowsAndStaysWithinTheCap(t *testing.T) {
	p := Policy{Attempts: 8, Base: 100 * time.Millisecond, Max: time.Second}

	for attempt := 1; attempt <= 8; attempt++ {
		for range 50 {
			delay := backoff(p, attempt)
			if delay < 0 || delay > p.Max {
				t.Fatalf("attempt %d produced a delay of %s, outside [0, %s]", attempt, delay, p.Max)
			}
		}
	}

	// Two draws from different attempts should differ, or there is no jitter.
	seen := map[time.Duration]bool{}
	for range 20 {
		seen[backoff(p, 3)] = true
	}
	if len(seen) == 1 {
		t.Error("the backoff produced an identical delay every time — no jitter")
	}
}

func TestAttemptsBelowOneStillRunsOnce(t *testing.T) {
	calls := 0
	_ = Do(context.Background(), nil, Policy{Attempts: 0}, "upload", func(context.Context) error {
		calls++
		return errors.New("nope")
	})
	if calls != 1 {
		t.Errorf("a zero-attempt policy ran the operation %d times", calls)
	}
}
