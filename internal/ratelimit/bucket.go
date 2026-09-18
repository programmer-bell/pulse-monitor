// Package ratelimit implements a hand-rolled, per-key token bucket rate
// limiter. It exists so the monitor's worker pool can be bounded by
// concurrency (how many checks run at once) independently from bounded by
// rate (how many requests per second any single domain receives) — see the
// README's "Why these choices" section for the reasoning.
//
// There is no external dependency here on purpose: golang.org/x/time/rate
// would do this too, but the point of this package is to demonstrate the
// mechanism, not just call a library that already implements it.
package ratelimit

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrClosed is returned by Wait when the bucket has been closed while a
// caller was waiting for a token.
var ErrClosed = errors.New("ratelimit: bucket closed")

// Bucket is a single-key token bucket. Tokens are added at a fixed rate by a
// background refill goroutine and consumed one at a time by Wait. The bucket
// starts empty: the first token is only available after the first tick, so
// the rate is enforced from the very first call rather than allowing an
// initial burst up to capacity.
//
// A Bucket owns exactly one goroutine (its refill loop), which is stopped by
// Close. Every Bucket must be closed by whoever creates it — see Manager.Close
// for the usual way that happens in this codebase.
type Bucket struct {
	tokens chan struct{}
	ticker *time.Ticker

	closeOnce sync.Once
	done      chan struct{}
}

// NewBucket creates a Bucket that admits at most ratePerSecond tokens every
// second, and starts its refill goroutine. ratePerSecond must be at least 1;
// values below that are clamped to 1 so a misconfigured rate degrades to
// "very slow" instead of a divide-by-zero panic.
func NewBucket(ratePerSecond int) *Bucket {
	if ratePerSecond < 1 {
		ratePerSecond = 1
	}

	b := &Bucket{
		// Capacity equals the rate: at most one second's worth of tokens can
		// ever sit unused in the bucket, so a caller that stops waiting for a
		// while cannot "bank" tokens into a large burst later.
		tokens: make(chan struct{}, ratePerSecond),
		ticker: time.NewTicker(time.Second / time.Duration(ratePerSecond)),
		done:   make(chan struct{}),
	}
	go b.refill()
	return b
}

// refill runs for the lifetime of the bucket, adding one token per tick. If
// the bucket is already full the tick is simply dropped — tokens never queue
// up beyond capacity. refill returns, stopping the ticker, once Close is
// called; this is the one goroutine a Bucket owns, and this is how it stops.
func (b *Bucket) refill() {
	defer b.ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-b.ticker.C:
			select {
			case b.tokens <- struct{}{}:
			default:
				// Bucket already at capacity; drop the tick.
			}
		}
	}
}

// Wait blocks until a token is available, ctx is done, or the bucket is
// closed, whichever happens first. It returns ctx.Err() or ErrClosed in the
// latter two cases so callers can distinguish "gave up" from "shut down".
func (b *Bucket) Wait(ctx context.Context) error {
	select {
	case <-b.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrClosed
	}
}

// Close stops the bucket's refill goroutine. It is safe to call more than
// once and safe to call concurrently with Wait — any in-flight Wait calls
// unblock with ErrClosed instead of hanging forever.
func (b *Bucket) Close() {
	b.closeOnce.Do(func() { close(b.done) })
}
