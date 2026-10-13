package supplychain

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestCheckerRunsAtStartAndThenPerInterval(t *testing.T) {
	var runs atomic.Int32
	c := &Checker{
		Image:    "registry.example.com/platform/operator:v1",
		Interval: 20 * time.Millisecond,
		Timeout:  time.Second,
		Log:      logr.Discard(),
		Verify: func(ctx context.Context) Result {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("a check runs without a deadline")
			}
			n := runs.Add(1)
			return Result{Signature: Valid, Kid: "k", Reason: ReasonVerified, CheckedAt: time.Now(), Message: "ok", Keys: int(n)}
		},
	}
	if r := c.Current(); r.Signature != Unverified || r.Reason != ReasonNotChecked || !r.OK() || r.CheckedAt.IsZero() {
		t.Errorf("before the first check: %+v", r)
	}
	if r := c.Run(context.Background()); r.Signature != Valid || c.Current().Keys != 1 {
		t.Errorf("first run: %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if runs.Load() < 3 || c.Current().Keys < 3 {
		t.Errorf("%d runs; current %+v", runs.Load(), c.Current())
	}
	if c.NeedLeaderElection() {
		t.Error("the checker never elects")
	}
}

func TestCheckerWithoutIntervalWaitsForTheContext(t *testing.T) {
	c := &Checker{Log: logr.Discard(), Verify: func(context.Context) Result { return Result{} }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
}
