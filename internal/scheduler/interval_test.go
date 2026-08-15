package scheduler

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	return loc
}

// The whole reason this type exists: "every 2 days" must stay 2 days apart even
// across a month boundary, where cron's "*/2" day-of-month resets.
func TestIntervalSurvivesMonthBoundary(t *testing.T) {
	ist := mustLoad(t, "Asia/Kolkata")

	// Last run on the 31st; two days later is the 2nd of the next month.
	last := time.Date(2026, 8, 31, 2, 30, 0, 0, ist)
	s := newIntervalSchedule(48*time.Hour, 2, 30, ist, last)

	next := s.Next(time.Date(2026, 8, 31, 3, 0, 0, 0, ist))
	want := time.Date(2026, 9, 2, 2, 30, 0, 0, ist)

	if !next.Equal(want) {
		t.Errorf("got %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestIntervalHonoursPinnedTimeOfDay(t *testing.T) {
	ist := mustLoad(t, "Asia/Kolkata")

	// A run that started late must not drag the schedule later each cycle.
	last := time.Date(2026, 8, 10, 4, 47, 12, 0, ist)
	s := newIntervalSchedule(24*time.Hour, 2, 30, ist, last)

	next := s.Next(time.Date(2026, 8, 10, 5, 0, 0, 0, ist))
	want := time.Date(2026, 8, 11, 2, 30, 0, 0, ist)

	if !next.Equal(want) {
		t.Errorf("got %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// A window missed while the host was down should be caught up promptly rather
// than skipped for another whole interval.
func TestIntervalCatchesUpAfterDowntime(t *testing.T) {
	utc := time.UTC
	last := time.Date(2026, 8, 1, 2, 30, 0, 0, utc)
	s := newIntervalSchedule(48*time.Hour, 2, 30, utc, last)

	now := time.Date(2026, 8, 9, 11, 0, 0, 0, utc) // a week of downtime
	next := s.Next(now)

	if got := next.Sub(now); got != catchUpDelay {
		t.Errorf("expected a catch-up %s after now, got %s (at %s)", catchUpDelay, got, next)
	}
}

// A fresh install with a pinned time should wait for that time, not fire an
// interval after whenever the deploy happened to land.
func TestIntervalFirstRunUsesPinnedTime(t *testing.T) {
	utc := time.UTC
	s := newIntervalSchedule(24*time.Hour, 2, 30, utc, time.Time{})

	next := s.Next(time.Date(2026, 8, 12, 16, 0, 0, 0, utc))
	want := time.Date(2026, 8, 13, 2, 30, 0, 0, utc)

	if !next.Equal(want) {
		t.Errorf("got %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestIntervalWithoutPinnedTime(t *testing.T) {
	utc := time.UTC
	last := time.Date(2026, 8, 12, 10, 0, 0, 0, utc)
	s := newIntervalSchedule(6*time.Hour, -1, -1, utc, last)

	next := s.Next(time.Date(2026, 8, 12, 10, 1, 0, 0, utc))
	want := time.Date(2026, 8, 12, 16, 0, 0, 0, utc)

	if !next.Equal(want) {
		t.Errorf("got %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// Regression: cron computes the next slot immediately after firing. Each such
// call must land a full interval ahead, never a few milliseconds later — that
// bug made every cycle fire twice.
func TestConsecutiveFiresAreOneIntervalApart(t *testing.T) {
	utc := time.UTC
	anchor := time.Date(2026, 8, 12, 10, 0, 0, 0, utc)
	s := newIntervalSchedule(time.Minute, -1, -1, utc, anchor)

	// Startup scheduling: one interval past the last completed run.
	first := s.Next(time.Date(2026, 8, 12, 10, 0, 30, 0, utc))
	if want := anchor.Add(time.Minute); !first.Equal(want) {
		t.Fatalf("first fire: got %s, want %s", first, want)
	}

	// Cron fires at `first` and immediately asks for the following slot.
	for i := range 5 {
		next := s.Next(first)
		if gap := next.Sub(first); gap != time.Minute {
			t.Fatalf("cycle %d: fires were %s apart, want %s (fired %s, next %s)", i, gap, time.Minute, first, next)
		}
		first = next
	}
}

// preview must not consume the first-fire state, or logging the upcoming run
// would change when it actually happens.
func TestPreviewDoesNotAdvanceTheSchedule(t *testing.T) {
	utc := time.UTC
	anchor := time.Date(2026, 8, 12, 10, 0, 0, 0, utc)
	s := newIntervalSchedule(time.Hour, -1, -1, utc, anchor)

	now := time.Date(2026, 8, 12, 10, 30, 0, 0, utc)
	peeked := s.preview(now)
	actual := s.Next(now)

	if !peeked.Equal(actual) {
		t.Errorf("preview said %s but Next gave %s", peeked, actual)
	}
}
