package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestManager_PerKeyIsolation saturates one key's bucket with far more
// waiters than its rate can serve quickly, then asserts a *different* key
// is still served promptly. If keys shared a bucket (or a lock held across
// Wait), the isolated key would queue up behind the busy one's backlog.
func TestManager_PerKeyIsolation(t *testing.T) {
	const rate = 20 // one token every 50ms, per key
	m := NewManager(rate)
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var busy sync.WaitGroup
	for i := 0; i < 20; i++ {
		busy.Add(1)
		go func() {
			defer busy.Done()
			_ = m.Wait(ctx, "busy.example.com")
		}()
	}

	quietStart := time.Now()
	if err := m.Wait(ctx, "quiet.example.com"); err != nil {
		t.Fatalf("Wait() for isolated key returned error: %v", err)
	}
	quietElapsed := time.Since(quietStart)

	// The isolated key's own limit allows one token every 1/rate seconds.
	// Give generous headroom above that (5x) so scheduler jitter can't flake
	// this, while still catching the real bug: "quiet" queuing up behind
	// "busy"'s 20 waiters, which at this rate would take multiple seconds.
	maxExpected := 5 * (time.Second / rate)
	if quietElapsed > maxExpected {
		t.Fatalf("isolated key waited %s, want under %s — keys are not isolated", quietElapsed, maxExpected)
	}

	busy.Wait()
}

// TestManager_ConcurrentCreation floods the Manager with many goroutines
// racing to create and use buckets for a handful of shared keys. Run with
// -race, this is the test that would fail if bucketFor's map access weren't
// properly guarded. It also checks that racing creators converge on exactly
// one bucket per key rather than each goroutine creating its own.
func TestManager_ConcurrentCreation(t *testing.T) {
	const (
		rate       = 1000 // fast: this test is about safe creation, not timing
		numKeys    = 8
		perKeyHits = 50
	)
	m := NewManager(rate)
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	keys := make([]string, numKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("host-%d.example.com", i)
	}

	var wg sync.WaitGroup
	for i := 0; i < numKeys*perKeyHits; i++ {
		key := keys[i%numKeys]
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			if err := m.Wait(ctx, key); err != nil {
				t.Errorf("Wait(%q) error: %v", key, err)
			}
		}(key)
	}
	wg.Wait()

	m.mu.Lock()
	got := len(m.buckets)
	m.mu.Unlock()
	if got != numKeys {
		t.Fatalf("manager holds %d buckets for %d distinct keys, want exactly %d (racing creators must converge on one bucket per key)", got, numKeys, numKeys)
	}
}

// TestManager_CloseUnblocksWaiters verifies Close() releases every pending
// waiter across every key, not just the one most recently created.
func TestManager_CloseUnblocksWaiters(t *testing.T) {
	m := NewManager(1) // slow: nothing refills fast enough to finish first

	const numKeys = 4
	errCh := make(chan error, numKeys)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("host-%d.example.com", i)
		go func(key string) {
			errCh <- m.Wait(context.Background(), key)
		}(key)
	}

	time.Sleep(5 * time.Millisecond) // let all goroutines reach Wait
	m.Close()

	for i := 0; i < numKeys; i++ {
		select {
		case err := <-errCh:
			if err != ErrClosed {
				t.Fatalf("Wait() error = %v, want ErrClosed", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a waiter did not unblock after Manager.Close()")
		}
	}
}
