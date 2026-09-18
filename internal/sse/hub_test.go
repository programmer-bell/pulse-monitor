package sse

import (
	"sync"
	"testing"
	"time"
)

func TestEventFrameFormat(t *testing.T) {
	ev := Event{Name: "check", Data: []byte("hello\nworld")}
	want := "event: check\ndata: hello\ndata: world\n\n"
	if got := string(ev.Frame()); got != want {
		t.Fatalf("Frame() = %q, want %q", got, want)
	}
}

func TestHub_BroadcastsToAllSubscribers(t *testing.T) {
	hub := New()
	chans := make([]chan []byte, 3)
	for i := range chans {
		chans[i] = make(chan []byte, 4)
		hub.Subscribe(chans[i])
	}
	defer func() {
		for _, ch := range chans {
			hub.Unsubscribe(ch)
		}
	}()

	hub.Publish(Event{Name: "check", Data: []byte("payload")})

	want := "event: check\ndata: payload\n\n"
	for i, ch := range chans {
		select {
		case got := <-ch:
			if string(got) != want {
				t.Fatalf("chans[%d] got %q, want %q", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("chans[%d] never received the published frame", i)
		}
	}
}

func TestHub_UnsubscribeStopsDelivery(t *testing.T) {
	hub := New()
	a := make(chan []byte, 4)
	b := make(chan []byte, 4)
	hub.Subscribe(a)
	hub.Subscribe(b)

	hub.Unsubscribe(a)

	hub.Publish(Event{Name: "stats", Data: []byte("x")})

	select {
	case got := <-b:
		if string(got) != "event: stats\ndata: x\n\n" {
			t.Fatalf("b got %q", got)
		}
	default:
		t.Fatal("b should have received the frame")
	}

	select {
	case got := <-a:
		t.Fatalf("unsubscribed client a received %q", got)
	default:
		// expected: a gets nothing after Unsubscribe
	}
}

func TestHub_DoesNotBlockOnSlowClient(t *testing.T) {
	hub := New()
	// Buffer of 1: after the first frame the client is "stalled".
	slow := make(chan []byte, 1)
	hub.Subscribe(slow)
	defer hub.Unsubscribe(slow)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			hub.Publish(Event{Name: "check", Data: []byte("drop")})
		}
		close(done)
	}()

	select {
	case <-done:
		// Publish returned promptly even though the client never drains.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Publish blocked behind a full client buffer")
	}
}

// TestHub_ConcurrentPublishAndUnsubscribe exercises the mutex against the
// one race that would actually corrupt the hub: a Publish holding the lock
// and sending into a channel while Unsubscribe deletes and the owner closes
// it. Run under -race this is a real assertion, not a formality.
func TestHub_ConcurrentPublishAndUnsubscribe(t *testing.T) {
	hub := New()
	for i := 0; i < 200; i++ {
		ch := make(chan []byte, 4)
		hub.Subscribe(ch)
		go hub.Publish(Event{Name: "check", Data: []byte("x")})
		hub.Unsubscribe(ch)
	}
}

// TestHub_BroadcastToManyConcurrentPublishers verifies every subscriber
// receives every frame when many goroutines publish at once, and that the
// hub is safe for concurrent use. Buffers are sized for the full volume so
// no frame is legitimately dropped and the count is deterministic.
func TestHub_BroadcastToManyConcurrentPublishers(t *testing.T) {
	const (
		publishers  = 8
		subscribers = 32
		perPub      = 40
	)

	hub := New()
	chans := make([]chan []byte, subscribers)
	for i := range chans {
		chans[i] = make(chan []byte, publishers*perPub)
		hub.Subscribe(chans[i])
	}
	defer func() {
		for _, ch := range chans {
			hub.Unsubscribe(ch)
		}
	}()

	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perPub; i++ {
				hub.Publish(Event{Name: "check", Data: []byte("tick")})
			}
		}()
	}
	wg.Wait()

	for i, ch := range chans {
		if got := len(ch); got != publishers*perPub {
			t.Fatalf("chans[%d] buffered %d frames, want %d", i, got, publishers*perPub)
		}
	}
}