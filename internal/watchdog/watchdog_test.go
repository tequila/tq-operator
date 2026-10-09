package watchdog

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestStallStopsTheProcess(t *testing.T) {
	c := &clock{now: time.Now()}
	stopped := make(chan string, 1)
	w := &Watchdog{Limit: 45 * time.Minute, SyncTimeout: time.Second, Check: 5 * time.Millisecond, Now: c.Now,
		WaitForSync: func(context.Context) bool { return true },
		Stop:        func(reason string) { stopped <- reason }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	time.Sleep(20 * time.Millisecond)
	c.add(30 * time.Minute)
	w.Beat()
	c.add(40 * time.Minute) // 40 minutes since the beat: within the limit
	select {
	case r := <-stopped:
		t.Fatalf("stopped early: %s", r)
	case <-time.After(50 * time.Millisecond):
	}
	c.add(10 * time.Minute) // 50 minutes since the beat
	select {
	case r := <-stopped:
		if !strings.Contains(r, "no reconcile completed") {
			t.Errorf("reason %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("a stall did not stop the process")
	}
}

func TestCachesThatNeverSyncStopTheProcess(t *testing.T) {
	stopped := make(chan string, 1)
	w := &Watchdog{Limit: time.Hour, SyncTimeout: 10 * time.Millisecond,
		WaitForSync: func(ctx context.Context) bool { <-ctx.Done(); return false },
		Stop:        func(reason string) { stopped <- reason }}
	go func() { _ = w.Start(context.Background()) }()
	select {
	case r := <-stopped:
		if !strings.Contains(r, "did not sync") {
			t.Errorf("reason %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("unsynced caches did not stop the process")
	}
}

func TestShutdownIsNotAStall(t *testing.T) {
	called := false
	w := &Watchdog{Limit: time.Hour, SyncTimeout: time.Minute,
		WaitForSync: func(ctx context.Context) bool { <-ctx.Done(); return false },
		Stop:        func(string) { called = true }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = w.Start(ctx)
	if called {
		t.Error("a cancelled context is a shutdown, not a stall")
	}
}
