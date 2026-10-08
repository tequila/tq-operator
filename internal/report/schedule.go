package report

import "time"

// The report's rhythm: a heartbeat when nothing changes, an immediate report when something
// does, and a bounded backoff when the console or IAM refuses or cannot be reached.
const (
	// BackoffMin and BackoffMax bound the exponential backoff after a failed attempt.
	BackoffMin = time.Minute
	BackoffMax = time.Hour
	// UnboundEarly is the retry after an Unbound outcome during the first hour after start: an
	// operator usually starts before `tq estate:identity` registered its binding, and the
	// binding should be noticed within minutes, not an hour.
	UnboundEarly = 5 * time.Minute
	// UnboundEarlyWindow is how long after start UnboundEarly applies; after it, Unbound backs
	// off to BackoffMax at once (the binding was deleted, or the cluster's keys rotated).
	UnboundEarlyWindow = time.Hour
)

// Schedule decides when an estate's report is due. One per Estate (and one for the
// no-Estate report). Not safe for concurrent use; the Reporter serialises access.
type Schedule struct {
	failures     int
	next         time.Time
	lastAccepted time.Time
	lastHash     string
}

// Due reports whether a report with this payload hash should be sent now: never inside a
// backoff; otherwise when nothing was accepted yet, the payload changed, or the heartbeat
// interval passed since the last accepted report.
func (s *Schedule) Due(now time.Time, hash string, interval time.Duration) bool {
	if now.Before(s.next) {
		return false
	}
	if s.lastAccepted.IsZero() {
		return true
	}
	return hash != s.lastHash || now.Sub(s.lastAccepted) >= interval
}

// NextDue is the earliest time a report could next be due, assuming an unchanged payload.
func (s *Schedule) NextDue(interval time.Duration) time.Time {
	if !s.next.IsZero() {
		return s.next
	}
	if s.lastAccepted.IsZero() {
		return time.Time{}
	}
	return s.lastAccepted.Add(interval)
}

// Record files an attempt's result. started is when the process started (for the early-retry
// window).
func (s *Schedule) Record(now, started time.Time, r Result, hash string) {
	if r.Outcome == OutcomeAccepted {
		s.failures, s.next, s.lastAccepted, s.lastHash = 0, time.Time{}, now, hash
		return
	}
	s.failures++
	var d time.Duration
	if r.Outcome == OutcomeUnbound {
		d = BackoffMax
		if now.Sub(started) < UnboundEarlyWindow {
			d = UnboundEarly
		}
	} else {
		d = Backoff(s.failures)
		if r.RetryAfter > d {
			d = min(r.RetryAfter, BackoffMax)
		}
	}
	s.next = now.Add(d)
}

// Failures is the number of consecutive failed attempts.
func (s *Schedule) Failures() int { return s.failures }

// Backoff is the wait after the n-th consecutive failure: 1 m, 2 m, 4 m … capped at 1 h.
func Backoff(n int) time.Duration {
	if n < 1 {
		return 0
	}
	d := BackoffMin
	for i := 1; i < n && d < BackoffMax; i++ {
		d *= 2
	}
	return min(d, BackoffMax)
}
