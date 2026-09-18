package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestBucket_RespectsRate floods a bucket with far more concurrent callers
// than its rate allows and asserts that draining them takes roughly as long
// as the rate implies. This is the test that would fail if Wait let every
// caller through immediately instead of actually gating on tokens.
func TestBucket_RespectsRate(t *testing.T) {
	const (
		rate    = 50 // tokens/sec -> one token every 20ms
		callers = 10
	)
	b := NewBucket(rate)
	defer b.Close()

	// Generous ceiling so a genuinely broken (hanging) limiter fails the
	// test instead of hanging the test suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- b.Wait(ctx)
		}()
	}
	wg.Wait()
	close(errs)

	elapsed := time.Since(start)

	for err := range errs {
		if err != nil {
			t.Fatalf("Wait() returned unexpected error: %v", err)
		}
	}

	// Draining `callers` tokens from an empty bucket that refills one token
	// every 1/rate seconds cannot possibly take less than (callers-1) ticks
	// — that's the whole point of the limiter. We check for at least half of
	// that theoretical floor, which leaves plenty of room for scheduler
	// jitter while still catching a limiter that isn't limiting anything
	// (which would finish in well under a millisecond).
	minExpected := time.Duration(callers-1) * (time.Second / rate) / 2
	if elapsed < minExpected {
		t.Fatalf("drained %d tokens in %s, want at least %s — rate is not being enforced", callers, elapsed, minExpected)
	}
}

// TestBucket_WaitRespectsContext verifies Wait gives up when its context is
// canceled instead of blocking forever, since every I/O wait in this
// codebase is required to be context-bounded (see .agents/instruction).
func TestBucket_WaitRespectsContext(t *testing.T) {
	// A slow bucket (1/sec) with no tokens pre-loaded: the very first Wait
	// has nothing to consume until the first tick, so a short-lived context
	// is guaranteed to expire first.
	b := NewBucket(1)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := b.Wait(ctx); err != context.DeadlineExceeded {
		t.Fatalf("Wait() error = %v, want context.DeadlineExceeded", err)
	}
}

// TestBucket_WaitAfterClose verifies a caller blocked on Wait unblocks with
// ErrClosed, rather than hanging, once the bucket is closed.
func TestBucket_WaitAfterClose(t *testing.T) {
	b := NewBucket(1)

	errCh := make(chan error, 1)
	go func() {
		errCh <- b.Wait(context.Background())
	}()

	// Give the goroutine a moment to actually reach Wait before closing.
	time.Sleep(5 * time.Millisecond)
	b.Close()

	select {
	case err := <-errCh:
		if err != ErrClosed {
			t.Fatalf("Wait() error = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not unblock after Close()")
	}
}
