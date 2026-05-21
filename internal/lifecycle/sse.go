package lifecycle

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type SSEEvent struct {
	Type      string
	Data      string
	ID        string
	Timestamp time.Time
}

type SubscriberManager struct {
	mu          sync.RWMutex
	subscribers map[chan SSEEvent]struct{}
	ring        []SSEEvent
	ringSize    int
	ringIdx     int
	ringCount   int
	nextID      atomic.Int64
}

func NewSubscriberManager(ringSize int) *SubscriberManager {
	if ringSize <= 0 {
		ringSize = 100
	}
	return &SubscriberManager{
		subscribers: make(map[chan SSEEvent]struct{}),
		ring:        make([]SSEEvent, ringSize),
		ringSize:    ringSize,
	}
}

func (sm *SubscriberManager) Subscribe() chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	sm.mu.Lock()
	sm.subscribers[ch] = struct{}{}
	sm.mu.Unlock()
	return ch
}

func (sm *SubscriberManager) Unsubscribe(ch chan SSEEvent) {
	sm.mu.Lock()
	delete(sm.subscribers, ch)
	sm.mu.Unlock()
	close(ch)
}

func (sm *SubscriberManager) Broadcast(event SSEEvent) {
	id := sm.nextID.Add(1)
	event.ID = fmt.Sprintf("%d", id-1)
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	sm.mu.Lock()
	sm.ring[sm.ringIdx%sm.ringSize] = event
	sm.ringIdx++
	if sm.ringCount < sm.ringSize {
		sm.ringCount++
	}
	for ch := range sm.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
	sm.mu.Unlock()
}

func (sm *SubscriberManager) ReplaySince(lastID string) []SSEEvent {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.ringCount == 0 {
		return nil
	}

	var last int64
	fmt.Sscanf(lastID, "%d", &last)

	var result []SSEEvent
	start := sm.ringIdx - sm.ringCount
	for i := start; i < sm.ringIdx; i++ {
		ev := sm.ring[i%sm.ringSize]
		var evID int64
		fmt.Sscanf(ev.ID, "%d", &evID)
		if evID > last {
			result = append(result, ev)
		}
	}
	return result
}

func (sm *SubscriberManager) SubscriberCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.subscribers)
}

func EncodeSSEEvent(event SSEEvent) string {
	return fmt.Sprintf("event: %s\ndata: %s\nid: %s\n\n", event.Type, event.Data, event.ID)
}