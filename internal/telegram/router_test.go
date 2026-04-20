package telegram

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

func TestNewRouterDisabled(t *testing.T) {
	cfg := &config.Config{
		Telegram: config.TelegramConfig{Enabled: false},
	}
	bus := event.NewBus()

	r, err := NewRouter(cfg, bus)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r != nil {
		t.Error("expected nil router when telegram disabled")
	}
}

func TestNewRouterNoToken(t *testing.T) {
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			Enabled: true,
			Token:   "",
		},
		Agent: config.AgentConfig{
			WorkDir: t.TempDir(),
		},
	}
	bus := event.NewBus()

	_, err := NewRouter(cfg, bus)
	if err == nil {
		t.Error("expected error for empty token")
	}
}

func TestRouterStore(t *testing.T) {
	// Verify that Store() and Bot() are accessible after creation
	// We can't easily test with a real bot token, so test the store directly
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Test basic CRUD through the store
	m := &TopicMapping{
		ChatID:      -1001234567890,
		ThreadID:   42,
		Repository: "org/repo",
		SessionKey: "org/repo/issues/42",
		IssueNumber: 42,
	}

	if err := store.CreateMapping(m); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.GetBySessionKey("org/repo/issues/42")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ThreadID != 42 {
		t.Errorf("expected thread 42, got %d", got.ThreadID)
	}
}

// TestEventBusIntegration verifies that Telegram events flow through the bus.
func TestEventBusIntegration(t *testing.T) {
	bus := event.NewBus()
	sub := bus.Subscribe()

	// Publish a Telegram event manually
	evt := event.NewEvent(event.TelegramMessage, "org/repo", 42, 0, "testuser", "message")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"source":    "telegram",
		"chat_id":   "-1001234567890",
		"thread_id": "42",
		"text":      "hello agent",
	}

	bus.Publish(context.Background(), evt)

	select {
	case received := <-sub:
		if received.Type != event.TelegramMessage {
			t.Errorf("expected telegram.message type, got %s", received.Type)
		}
		if received.SessionKey != "org/repo/issues/42" {
			t.Errorf("expected session key org/repo/issues/42, got %s", received.SessionKey)
		}
		if received.Sender != "testuser" {
			t.Errorf("expected sender testuser, got %s", received.Sender)
		}
	case <-context.Background().Done():
		t.Fatal("timeout waiting for event")
	}
}
