package event

import (
	"context"
	"testing"
	"time"
)

func TestNewEvent(t *testing.T) {
	evt := NewEvent(IssueCommentCreated, "org/repo", 42, 0, "alice", "created")
	if evt.ID == "" {
		t.Error("expected non-empty event ID")
	}
	if evt.Type != IssueCommentCreated {
		t.Errorf("expected type %s, got %s", IssueCommentCreated, evt.Type)
	}
	if evt.Repository != "org/repo" {
		t.Errorf("expected org/repo, got %s", evt.Repository)
	}
	if evt.IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", evt.IssueNumber)
	}
	if evt.Sender != "alice" {
		t.Errorf("expected sender alice, got %s", evt.Sender)
	}
}

func TestBusPublishSubscribe(t *testing.T) {
	bus := NewBus()
	ch := bus.Subscribe()

	evt := NewEvent(IssueOpened, "org/repo", 1, 0, "bob", "opened")
	ctx := context.Background()

	bus.Publish(ctx, evt)

	select {
	case received := <-ch:
		if received.ID != evt.ID {
			t.Errorf("expected event ID %s, got %s", evt.ID, received.ID)
		}
	case <-time.After(time.Second):
		t.Error("timed out waiting for event")
	}

	bus.Unsubscribe(ch)
}

func TestBusUnsubscribe(t *testing.T) {
	bus := NewBus()
	ch := bus.Subscribe()
	bus.Unsubscribe(ch)

	// Should not panic
	evt := NewEvent(Push, "org/repo", 0, 0, "charlie", "push")
	bus.Publish(context.Background(), evt)
}

func TestBusBackpressure(t *testing.T) {
	bus := NewBus()
	_ = bus.Subscribe() // buffered channel of 256

	ctx := context.Background()
	// Send more than buffer size
	for i := 0; i < 300; i++ {
		bus.Publish(ctx, NewEvent(Push, "org/repo", 0, 0, "bot", "push"))
	}
	// Should not block (events dropped)
}
