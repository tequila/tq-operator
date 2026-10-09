// Package watchdog is the operator's liveness without a socket: no probe port exists, so the
// process watches itself and exits non-zero when it stalls — restartPolicy: Always brings it
// back, and a stall is visible as a restart count and a gap in reports, never as a port.
package watchdog

import (
	"context"
	"sync/atomic"
	"time"
)

// Watchdog stops the process when the informer caches do not sync in time or no reconcile
// completes within the limit.
type Watchdog struct {
	// Limit is the longest a reconcile may take to complete (3 × the interval).
	Limit time.Duration
	// SyncTimeout is how long the caches may take to sync after start.
	SyncTimeout time.Duration
	// WaitForSync blocks until the caches synced (false when they could not).
	WaitForSync func(ctx context.Context) bool
	// Stop ends the process with a non-zero code and a reason.
	Stop func(reason string)
	// Check is how often the limit is checked (default one minute).
	Check time.Duration
	Now   func() time.Time

	last atomic.Int64
}

// Beat records a completed reconcile.
func (w *Watchdog) Beat() { w.last.Store(w.now().UnixNano()) }

// NeedLeaderElection: the operator never elects.
func (w *Watchdog) NeedLeaderElection() bool { return false }

// Start runs the watchdog until the context ends.
func (w *Watchdog) Start(ctx context.Context) error {
	w.Beat()
	if w.WaitForSync != nil {
		syncCtx, cancel := context.WithTimeout(ctx, w.SyncTimeout)
		synced := w.WaitForSync(syncCtx)
		cancel()
		if !synced {
			if ctx.Err() == nil {
				w.Stop("the informer caches did not sync within " + w.SyncTimeout.String())
			}
			return nil
		}
	}
	check := w.Check
	if check == 0 {
		check = time.Minute
	}
	t := time.NewTicker(check)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if since := w.now().Sub(time.Unix(0, w.last.Load())); since > w.Limit {
				w.Stop("no reconcile completed for " + since.Round(time.Second).String() + " (limit " + w.Limit.String() + ")")
				return nil
			}
		}
	}
}

func (w *Watchdog) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
