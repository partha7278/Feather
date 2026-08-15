package scheduler

import (
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// catchUpDelay is how long after startup a missed window fires. Short enough to
// be prompt, long enough that the daemon has finished booting.
const catchUpDelay = time.Minute

// intervalSchedule implements "every 2 days at 02:30" — a true N-apart interval
// rather than a calendar pattern.
//
// Cron cannot express this: "0 30 2 */2 * *" fires on odd days of the month and
// so produces a one-day gap at every month boundary. Anchoring the first fire on
// the last completed run also means a restart does not reset the cycle, and a
// window missed while the host was down is caught up once instead of skipped.
//
// The anchor is owned entirely by this type. Cron calls Next(t) immediately
// after firing at t, so from the second call onwards t *is* the current cycle's
// run time and the next slot follows from it. An earlier version updated the
// anchor from the job goroutine instead, which raced: cron computed the next
// slot before the job finished, so the anchor lagged a cycle, the candidate
// landed a few milliseconds in the future, and every cycle fired twice.
type intervalSchedule struct {
	every  time.Duration
	hour   int // -1 when no wall-clock time was pinned
	minute int
	loc    *time.Location

	mu        sync.Mutex
	anchor    time.Time // last completed run, read from the catalogue at startup
	scheduled bool      // whether Next has been called at least once
}

var _ cron.Schedule = (*intervalSchedule)(nil)

func newIntervalSchedule(every time.Duration, hour, minute int, loc *time.Location, lastRun time.Time) *intervalSchedule {
	return &intervalSchedule{every: every, hour: hour, minute: minute, loc: loc, anchor: lastRun}
}

// Next returns the time this job should run after t.
func (s *intervalSchedule) Next(t time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	t = t.In(s.loc)

	if !s.scheduled {
		s.scheduled = true
		return s.firstFire(t)
	}

	// Cron asks for the next slot immediately after firing at t, so t is this
	// cycle's run and the next one is a full interval beyond it.
	return s.after(t)
}

// preview reports what Next would return without consuming the first-fire
// state. Logging or displaying an upcoming run must not advance the schedule.
func (s *intervalSchedule) preview(t time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	t = t.In(s.loc)
	if !s.scheduled {
		return s.firstFire(t)
	}
	return s.after(t)
}

// firstFire picks the first slot after the daemon starts, using whatever the
// catalogue knows about previous runs.
func (s *intervalSchedule) firstFire(t time.Time) time.Time {
	// Never run before. Honour a pinned time of day if there is one, so a fresh
	// deploy at 4pm does not archive at 4pm every day thereafter.
	if s.anchor.IsZero() {
		if s.hour < 0 {
			return t.Add(s.every)
		}
		return nextWallClock(t, s.hour, s.minute, s.loc)
	}

	candidate := s.anchor.In(s.loc).Add(s.every)
	if s.hour >= 0 {
		candidate = alignToWallClock(candidate, s.hour, s.minute, s.loc)
	}

	// The window has already passed — the host was down, or the last run ran
	// long. Catch up shortly rather than waiting a whole further interval.
	if !candidate.After(t) {
		return t.Add(catchUpDelay)
	}
	return candidate
}

func (s *intervalSchedule) after(t time.Time) time.Time {
	next := t.Add(s.every)
	if s.hour >= 0 {
		next = alignToWallClock(next, s.hour, s.minute, s.loc)
	}
	// Defensive: a pinned time is only allowed with intervals of a day or more,
	// so aligning can never move the slot back past t in practice.
	if !next.After(t) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// alignToWallClock pins a candidate to the configured time of day on its own
// date. It deliberately does not roll forward when that moment is earlier in the
// day than the candidate: the interval has already advanced the date, so rolling
// again would turn a run that started late into a skipped cycle — "daily at
// 02:30" would quietly become every other day after one delayed run.
func alignToWallClock(candidate time.Time, hour, minute int, loc *time.Location) time.Time {
	return time.Date(candidate.Year(), candidate.Month(), candidate.Day(), hour, minute, 0, 0, loc)
}

// nextWallClock is the next occurrence of hour:minute strictly after t.
func nextWallClock(t time.Time, hour, minute int, loc *time.Location) time.Time {
	next := time.Date(t.Year(), t.Month(), t.Day(), hour, minute, 0, 0, loc)
	if !next.After(t) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
