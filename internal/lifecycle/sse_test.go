package lifecycle

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSubscribeUnsubscribe(t *testing.T) {
	sm := NewSubscriberManager(100)
	ch := sm.Subscribe()
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	if sm.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", sm.SubscriberCount())
	}
	sm.Unsubscribe(ch)
	if sm.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers after unsubscribe, got %d", sm.SubscriberCount())
	}
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after unsubscribe")
	}
}

func TestBroadcast(t *testing.T) {
	sm := NewSubscriberManager(100)
	ch := sm.Subscribe()
	defer sm.Unsubscribe(ch)

	ev := SSEEvent{Type: "turn", Data: `{"turn":1}`, Timestamp: time.Now()}
	sm.Broadcast(ev)

	select {
	case got := <-ch:
		if got.Type != "turn" {
			t.Fatalf("expected type turn, got %s", got.Type)
		}
		if got.Data != `{"turn":1}` {
			t.Fatalf("expected data {\"turn\":1}, got %s", got.Data)
		}
		if got.ID != "0" {
			t.Fatalf("expected ID 0, got %s", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broadcast event")
	}
}

func TestBroadcastDropsSlowSubscriber(t *testing.T) {
	sm := NewSubscriberManager(100)
	ch := make(chan SSEEvent, 1)
	sm.mu.Lock()
	sm.subscribers[ch] = struct{}{}
	sm.mu.Unlock()

	fastCh := sm.Subscribe()
	defer sm.Unsubscribe(fastCh)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			sm.Broadcast(SSEEvent{Type: "turn", Data: fmt.Sprintf(`{"i":%d}`, i)})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("broadcast blocked on slow subscriber")
	}

	sm.mu.Lock()
	delete(sm.subscribers, ch)
	close(ch)
	sm.mu.Unlock()
}

func TestReplaySince(t *testing.T) {
	sm := NewSubscriberManager(100)

	sm.Broadcast(SSEEvent{Type: "turn", Data: `{"n":1}`, Timestamp: time.Now()})
	sm.Broadcast(SSEEvent{Type: "transition", Data: `{"n":2}`, Timestamp: time.Now()})
	sm.Broadcast(SSEEvent{Type: "delivery", Data: `{"n":3}`, Timestamp: time.Now()})

	replayed := sm.ReplaySince("0")
	if len(replayed) != 2 {
		t.Fatalf("expected 2 replayed events, got %d", len(replayed))
	}
	if replayed[0].ID != "1" {
		t.Fatalf("expected first replayed ID 1, got %s", replayed[0].ID)
	}
	if replayed[1].ID != "2" {
		t.Fatalf("expected second replayed ID 2, got %s", replayed[1].ID)
	}
}

func TestReplaySinceEmpty(t *testing.T) {
	sm := NewSubscriberManager(100)

	replayed := sm.ReplaySince("999")
	if len(replayed) != 0 {
		t.Fatalf("expected 0 replayed events, got %d", len(replayed))
	}

	sm.Broadcast(SSEEvent{Type: "turn", Data: `{}`, Timestamp: time.Now()})
	replayed = sm.ReplaySince("999")
	if len(replayed) != 0 {
		t.Fatalf("expected 0 replayed events for future lastID, got %d", len(replayed))
	}
}

func TestSubscriberCount(t *testing.T) {
	sm := NewSubscriberManager(100)

	ch1 := sm.Subscribe()
	ch2 := sm.Subscribe()
	if sm.SubscriberCount() != 2 {
		t.Fatalf("expected 2 subscribers, got %d", sm.SubscriberCount())
	}

	sm.Unsubscribe(ch1)
	if sm.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", sm.SubscriberCount())
	}

	sm.Unsubscribe(ch2)
	if sm.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers, got %d", sm.SubscriberCount())
	}
}

func TestRingBufferWraparound(t *testing.T) {
	sm := NewSubscriberManager(5)

	for i := 0; i < 8; i++ {
		sm.Broadcast(SSEEvent{Type: "turn", Data: fmt.Sprintf(`{"i":%d}`, i), Timestamp: time.Now()})
	}

	replayed := sm.ReplaySince("4")
	if len(replayed) != 3 {
		t.Fatalf("expected 3 replayed events after wraparound, got %d", len(replayed))
	}

	if replayed[0].ID != "5" {
		t.Fatalf("expected first replayed ID 5, got %s", replayed[0].ID)
	}
	if replayed[2].ID != "7" {
		t.Fatalf("expected last replayed ID 7, got %s", replayed[2].ID)
	}

	replayedAll := sm.ReplaySince("")
	if len(replayedAll) != 5 {
		t.Fatalf("expected 5 total events in ring, got %d", len(replayedAll))
	}
}

func TestEncodeSSEEvent(t *testing.T) {
	ev := SSEEvent{Type: "turn", Data: `{"turn":1}`, ID: "42", Timestamp: time.Now()}
	encoded := EncodeSSEEvent(ev)

	if !strings.HasPrefix(encoded, "event: turn\n") {
		t.Fatalf("expected event line, got %q", encoded)
	}
	if !strings.Contains(encoded, "data: {\"turn\":1}\n") {
		t.Fatalf("expected data line, got %q", encoded)
	}
	if !strings.Contains(encoded, "id: 42\n") {
		t.Fatalf("expected id line, got %q", encoded)
	}
	if !strings.HasSuffix(encoded, "\n\n") {
		t.Fatalf("expected double newline ending, got %q", encoded)
	}
}