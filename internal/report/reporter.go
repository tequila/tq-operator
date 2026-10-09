package report

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Reporter decides when each estate reports and sends it: one Schedule per key, one Client.
type Reporter struct {
	Client *Client
	// Started is when the process started — the start of the early-retry window.
	Started time.Time
	Now     func() time.Time

	mu        sync.Mutex
	schedules map[string]*Schedule
}

// Attempt is what one call to Report did.
type Attempt struct {
	// Attempted is false when the report was not due (nothing was sent, nothing changed).
	Attempted bool
	Result    Result
	At        time.Time
	// Identity is the estate identity the report was addressed with (zero when the exchange
	// failed).
	Identity Identity
}

// NoEstateKey is the schedule key of the report an operator sends when no Estate is declared.
const NoEstateKey = "-"

// Report sends the payload for key when it is due. wantEnvironment, when set, must be the
// identity's environment: an Estate is reported only by the cluster whose identity is that
// environment, never addressed to another.
func (r *Reporter) Report(ctx context.Context, key string, payload Report, wantEnvironment string, interval time.Duration) (Attempt, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	s := r.schedule(key)
	hash := payload.Hash()
	if !s.Due(now, hash, interval) {
		return Attempt{}, s.NextDue(interval)
	}
	attempt := Attempt{Attempted: true, At: now}

	id, token, failed := r.Client.Identity(ctx)
	if failed != nil {
		attempt.Result = *failed
		s.Record(now, r.Started, attempt.Result, hash)
		return attempt, s.NextDue(interval)
	}
	attempt.Identity = id
	if wantEnvironment != "" && id.Environment != wantEnvironment {
		attempt.Result = Result{
			Outcome: OutcomeRefused,
			Code:    "environment_mismatch",
			Message: fmt.Sprintf("this cluster's identity is %s; the Estate declares environment %q — not reported", id.Subject(), wantEnvironment),
		}
		s.Record(now, r.Started, attempt.Result, hash)
		return attempt, s.NextDue(interval)
	}

	payload.Estate.Account = id.Account
	payload.Estate.Environment = id.Environment
	payload.ReportedAt = now.UTC().Format(time.RFC3339)
	body, err := payload.Marshal()
	if err != nil {
		attempt.Result = Result{Outcome: OutcomeInvalid, Code: "marshal", Message: err.Error()}
		s.Record(now, r.Started, attempt.Result, hash)
		return attempt, s.NextDue(interval)
	}
	attempt.Result = r.Client.Put(ctx, id, token, body)
	s.Record(now, r.Started, attempt.Result, hash)
	return attempt, s.NextDue(interval)
}

// NextDue is when key's report could next be due (zero: now).
func (r *Reporter) NextDue(key string, interval time.Duration) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.schedule(key).NextDue(interval)
}

// Forget drops a key's schedule (its Estate is gone).
func (r *Reporter) Forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.schedules, key)
}

func (r *Reporter) schedule(key string) *Schedule {
	if r.schedules == nil {
		r.schedules = map[string]*Schedule{}
	}
	s, ok := r.schedules[key]
	if !ok {
		s = &Schedule{}
		r.schedules[key] = s
	}
	return s
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
