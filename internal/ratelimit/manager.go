package ratelimit

import (
	"context"
	"sync"
)

// Manager hands out one Bucket per key (in this codebase, a key is a
// request domain, e.g. "example.com") so that a burst of checks against one
// slow or misbehaving host never eats into the rate budget of any other
// host. All buckets share the same configured rate.
//
// Manager is safe for concurrent use. Buckets are created lazily on first
// use and guarded by a mutex — see TestManager_ConcurrentCreation for the
// -race-verified proof that many goroutines racing to create the same or
// different keys can't corrupt the bucket map or double-create a bucket.
type Manager struct {
	ratePerSecond int

	mu      sync.Mutex
	buckets map[string]*Bucket
}

// NewManager creates a Manager whose buckets each admit ratePerSecond
// requests per second, per key.
func NewManager(ratePerSecond int) *Manager {
	return &Manager{
		ratePerSecond: ratePerSecond,
		buckets:       make(map[string]*Bucket),
	}
}

// bucketFor returns the Bucket for key, creating it if this is the first
// time key has been seen. The mutex is held only long enough to check/insert
// into the map — NewBucket's own goroutine start happens before we release
// it, but the (comparatively slow) waiting on tokens happens afterward, in
// Wait, outside the lock.
func (m *Manager) bucketFor(key string) *Bucket {
	m.mu.Lock()
	defer m.mu.Unlock()

	b, ok := m.buckets[key]
	if !ok {
		b = NewBucket(m.ratePerSecond)
		m.buckets[key] = b
	}
	return b
}

// Wait blocks until key has an available token, ctx is done, or the
// Manager (and therefore key's bucket) has been closed.
func (m *Manager) Wait(ctx context.Context, key string) error {
	return m.bucketFor(key).Wait(ctx)
}

// Close stops every bucket the Manager has created. Any goroutine currently
// blocked in Wait for one of those keys unblocks with ErrClosed. Close is
// meant to be called once, during shutdown, from the same place that created
// the Manager — it is not safe to call Wait after Close.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.buckets {
		b.Close()
	}
}
