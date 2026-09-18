// Package sse implements the Server-Sent Events broadcast hub — the
// transport side of the phase-4 real-time dashboard. The monitor engine
// renders a per-check "result" event and a per-tick "stats" event (in
// internal/handlers, which renders out-of-band HTML frames), Publish
// broadcasts them here, and every open browser tab pulls them down its own
// GET /events stream.
//
// The hub is deliberately small and standard-library-only: a mutex-protected
// set of subscribed channels, with Publish broadcasting to all of them. This
// is a single-instance dashboard — there is no external pub/sub system.
package sse

import (
	"strings"
	"sync"
)

// Event is a named SSE message. Name maps to the SSE `event:` field and Data
// to the `data:` field(s). htmx's SSE extension subscribes by event name, so
// the hub does no filtering — every subscriber receives every frame and the
// browser routes by name.
type Event struct {
	Name string
	Data []byte
}

// Frame renders Event in the SSE wire format:
//
//	event: <name>
//	data: <line 1>
//	data: <line 2>
//	<blank line>
//
// Multi-line data is split across `data:` lines per the SSE spec so the
// browser's EventSource reassembles the exact payload we sent.
func (e Event) Frame() []byte {
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(e.Name)
	b.WriteByte('\n')
	for _, line := range strings.Split(string(e.Data), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

// Hub is a mutex-protected broadcast hub.
//
// A client is a buffered channel of SSE frames. The client is owned by the
// subscriber: only the subscriber may close it, and the subscriber must call
// Unsubscribe (under the same mutex) first so no in-flight send races a
// close. Publish holds the mutex while sending, which serializes every send
// against Unsubscribe's delete+close of the same channel.
//
// Publish never blocks. If a client's buffer is full, that frame is dropped
// for it: a slow or stalled tab misses a frame instead of stalling the
// monitor tick, and it catches the next event a moment later.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

// New returns a Hub ready for use.
func New() *Hub {
	return &Hub{clients: make(map[chan []byte]struct{})}
}

// Subscribe registers ch as a client.
func (h *Hub) Subscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[ch] = struct{}{}
}

// Unsubscribe removes ch from the hub. It must be called exactly once per
// subscribed channel, by the owning subscriber, before closing the channel.
func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, ch)
}

// Publish broadcasts ev's frame to every subscribed client, dropping frames
// for any client whose buffer is full.
func (h *Hub) Publish(ev Event) {
	frame := ev.Frame()

	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- frame:
		default:
			// Client buffer full: drop this frame rather than block the
			// publisher (and, with it, the monitor tick).
		}
	}
}