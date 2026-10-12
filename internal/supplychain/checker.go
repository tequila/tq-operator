package supplychain

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Checker runs the self-check once at start and then once per interval, and keeps the latest
// result for the controller to write. One bounded check at a time; a check that cannot finish
// within Timeout is a result, not a hang.
type Checker struct {
	// Verify is the check (Verifier.Verify bound to the operator's image).
	Verify func(ctx context.Context) Result
	// Image is what the check is about (the initial result names it).
	Image    string
	Interval time.Duration
	Timeout  time.Duration
	Log      logr.Logger
	Now      func() time.Time

	mu   sync.Mutex
	last *Result
}

// DefaultTimeout bounds one check.
const DefaultTimeout = 30 * time.Second

// Current is the latest result; before the first check, NotChecked.
func (c *Checker) Current() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		return NotChecked(c.Image, c.now())
	}
	return *c.last
}

// Run performs one check now and keeps its result.
func (c *Checker) Run(ctx context.Context) Result {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r := c.Verify(ctx)
	c.mu.Lock()
	c.last = &r
	c.mu.Unlock()
	c.Log.Info("self-verification", "signature", r.Signature, "reason", r.Reason, "kid", r.Kid, "keys", r.Keys, "message", r.Message)
	return r
}

// Start re-runs the check every Interval until the context ends (the manager runs it).
func (c *Checker) Start(ctx context.Context) error {
	if c.Interval <= 0 {
		<-ctx.Done()
		return nil
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.Run(ctx)
		}
	}
}

// NeedLeaderElection: the operator runs one replica and never elects.
func (c *Checker) NeedLeaderElection() bool { return false }

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
