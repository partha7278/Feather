// Package retry runs an operation again when it failed for a reason that might
// not still be true a moment later.
//
// feather's runs are long and its steps are expensive. A batch that has been
// read from MongoDB, compressed to disk and uploaded to two of three
// destinations represents minutes of work and a spool file that will be thrown
// away if the third upload fails. Before this package there were no retries at
// all: one 500 from an object store that outlived the AWS SDK's own three
// attempts ended a multi-hour run, and the next scheduled run started again
// from the first document.
//
// The policy is deliberately conservative about what it will retry. An
// operation on the deletion path must never be repeated because of an error
// that means "this did not work and will not work" — retrying a permission
// failure only delays the report. See Retryable.
package retry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"
)

// Policy describes how persistent to be.
type Policy struct {
	// Attempts is the total number of tries, including the first. 1 disables
	// retrying without disabling this package.
	Attempts int
	// Base is the delay before the second attempt. Each subsequent delay
	// doubles it, up to Max.
	Base time.Duration
	// Max caps a single delay, so a long backoff cannot outlive the run.
	Max time.Duration
}

// DefaultPolicy is what the runner uses: four attempts over roughly fourteen
// seconds. Long enough to ride out a leader election, a throttle, or a brief
// network partition; short enough that a destination which is genuinely down
// is reported inside a batch rather than at the end of the run.
var DefaultPolicy = Policy{Attempts: 4, Base: 500 * time.Millisecond, Max: 8 * time.Second}

// Do runs op until it succeeds, until the policy is exhausted, or until the
// error is one that will not fix itself. The returned error is the last one.
//
// log may be nil. When it is not, every retry is reported at warn level with
// the attempt number and the delay, so a run that succeeded only after a
// wobble does not look identical in the log to one that went cleanly.
func Do(ctx context.Context, log *slog.Logger, p Policy, what string, op func(ctx context.Context) error) error {
	if p.Attempts < 1 {
		p.Attempts = 1
	}

	var err error
	for attempt := 1; ; attempt++ {
		err = op(ctx)
		if err == nil {
			if attempt > 1 && log != nil {
				log.Info("succeeded after retrying", "operation", what, "attempts", attempt)
			}
			return nil
		}

		if attempt >= p.Attempts {
			break
		}
		// A cancelled run is not a transient failure: the deadline has passed
		// or the process is shutting down, and sleeping would only delay it.
		if ctx.Err() != nil {
			break
		}
		if !Retryable(err) {
			break
		}

		delay := backoff(p, attempt)
		if log != nil {
			log.Warn("retrying after a transient failure",
				"operation", what, "attempt", attempt, "of", p.Attempts,
				"retrying_in", delay.Round(time.Millisecond), "error", err)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%s: %w (last error: %w)", what, ctx.Err(), err)
		case <-timer.C:
		}
	}

	if p.Attempts > 1 && Retryable(err) {
		return fmt.Errorf("%s: giving up after %d attempts: %w", what, p.Attempts, err)
	}
	return err
}

// backoff is exponential with full jitter: a delay chosen uniformly from
// [0, base*2^n]. Jitter matters here because a batch uploads to every
// destination at once, and several of them are usually the same service — a
// fixed backoff would have them all come back in step and collide again.
func backoff(p Policy, attempt int) time.Duration {
	window := float64(p.Base) * math.Pow(2, float64(attempt-1))
	if p.Max > 0 && window > float64(p.Max) {
		window = float64(p.Max)
	}
	if window <= 0 {
		return 0
	}
	// Keep a floor so the first retry is not effectively immediate.
	return time.Duration(window/2 + rand.Float64()*window/2)
}

// Permanent wraps an error to declare that retrying it is pointless, for a
// caller that knows more about the failure than this package can.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Retryable reports whether trying again could plausibly produce a different
// answer.
//
// The default is yes. That is the right default for a network call to an object
// store, where the large majority of failures are timeouts, resets, throttles
// and 5xx responses, and where the cost of one wasted retry is a few seconds
// against the cost of a lost run. Callers narrow it with Permanent for the
// failures they can positively identify — a missing bucket, a rejected
// credential, a malformed request — none of which improve on their own.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var permanent permanentError
	if errors.As(err, &permanent) {
		return false
	}
	// A cancelled or expired context is the caller's decision, not the
	// service's failure.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}
