package telegram

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

func TestStoreAccessors(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Verify Router.Store() returns the same store
	r := &Router{store: store}
	if r.Store() != store {
		t.Error("Store() should return the underlying MappingStore")
	}
}

// TestEventBusIntegration verifies that Telegram events flow through the bus.
func TestEventBusIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

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

	bus.Publish(ctx, evt)

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
	case <-ctx.Done():
		t.Fatal("timeout waiting for event")
	}
}

// --- NormalizeMessage tests ---

func TestNormalizeMessageNoThread(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	// No thread ID → no session → nil
	evt := NormalizeMessage(-100, 0, 1, "org/repo", "user", "hello", store)
	if evt != nil {
		t.Error("expected nil for message without thread ID")
	}
}

func TestNormalizeMessageUnmappedThread(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	// Thread ID exists but no mapping → nil
	evt := NormalizeMessage(-100, 42, 1, "org/repo", "user", "hello", store)
	if evt != nil {
		t.Error("expected nil for unmapped thread")
	}
}

func TestNormalizeMessageMappedThread(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	// Create a mapping for issue #42
	store.CreateMapping(&TopicMapping{
		ChatID:      -1001234567890,
		ThreadID:   42,
		Repository: "org/repo",
		SessionKey: "org/repo/issues/42",
		IssueNumber: 42,
	})

	evt := NormalizeMessage(-1001234567890, 42, 99, "org/repo", "testuser", "fix the bug", store)
	if evt == nil {
		t.Fatal("expected event, got nil")
	}

	// Verify all fields
	if evt.Type != event.TelegramMessage {
		t.Errorf("expected telegram.message, got %s", evt.Type)
	}
	if evt.SessionKey != "org/repo/issues/42" {
		t.Errorf("expected session key 'org/repo/issues/42', got %s", evt.SessionKey)
	}
	if evt.Repository != "org/repo" {
		t.Errorf("expected repository 'org/repo', got %s", evt.Repository)
	}
	if evt.IssueNumber != 42 {
		t.Errorf("expected issue number 42, got %d", evt.IssueNumber)
	}
	if evt.PRNumber != 0 {
		t.Errorf("expected PR number 0, got %d", evt.PRNumber)
	}
	if evt.Sender != "testuser" {
		t.Errorf("expected sender 'testuser', got %s", evt.Sender)
	}
	if evt.Action != "message" {
		t.Errorf("expected action 'message', got %s", evt.Action)
	}

	// Verify payload
	if evt.Payload["text"] != "fix the bug" {
		t.Errorf("expected text 'fix the bug', got %v", evt.Payload["text"])
	}
	if evt.Payload["from_user"] != "testuser" {
		t.Errorf("expected from_user 'testuser', got %v", evt.Payload["from_user"])
	}
	if evt.Payload["chat_id"] != "-1001234567890" {
		t.Errorf("expected chat_id '-1001234567890', got %v", evt.Payload["chat_id"])
	}
	if evt.Payload["thread_id"] != "42" {
		t.Errorf("expected thread_id '42', got %v", evt.Payload["thread_id"])
	}
	if evt.Payload["message_id"] != "99" {
		t.Errorf("expected message_id '99', got %v", evt.Payload["message_id"])
	}
	if evt.Payload["source"] != "telegram" {
		t.Errorf("expected source 'telegram', got %v", evt.Payload["source"])
	}
}

func TestNormalizeMessageMappedPR(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	store.CreateMapping(&TopicMapping{
		ChatID:    -100,
		ThreadID:  7,
		Repository: "org/repo",
		SessionKey: "org/repo/pulls/7",
		PRNumber:  7,
	})

	evt := NormalizeMessage(-100, 7, 10, "org/repo", "dev", "looks good", store)
	if evt == nil {
		t.Fatal("expected event, got nil")
	}
	if evt.PRNumber != 7 {
		t.Errorf("expected PR number 7, got %d", evt.PRNumber)
	}
	if evt.IssueNumber != 0 {
		t.Errorf("expected issue number 0, got %d", evt.IssueNumber)
	}
	if evt.SessionKey != "org/repo/pulls/7" {
		t.Errorf("expected session key 'org/repo/pulls/7', got %s", evt.SessionKey)
	}
}

func TestNormalizeMessageWrongChatID(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	// Mapping exists for chat -100, thread 42
	store.CreateMapping(&TopicMapping{
		ChatID:    -100,
		ThreadID:  42,
		Repository: "org/repo",
		SessionKey: "org/repo/issues/42",
		IssueNumber: 42,
	})

	// Query with different chat ID — thread 42 doesn't exist for chat -999
	evt := NormalizeMessage(-999, 42, 1, "org/repo", "user", "hello", store)
	if evt != nil {
		t.Error("expected nil for mismatched chat ID")
	}
}

func TestNormalizeMessageMultipleMappings(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	store.CreateMapping(&TopicMapping{
		ChatID: -100, ThreadID: 1, Repository: "org/repo",
		SessionKey: "org/repo/issues/1", IssueNumber: 1,
	})
	store.CreateMapping(&TopicMapping{
		ChatID: -100, ThreadID: 2, Repository: "org/repo",
		SessionKey: "org/repo/pulls/5", PRNumber: 5,
	})

	// Issue mapping
	evt1 := NormalizeMessage(-100, 1, 1, "org/repo", "user", "msg1", store)
	if evt1 == nil || evt1.SessionKey != "org/repo/issues/1" {
		t.Error("expected issue event")
	}

	// PR mapping
	evt2 := NormalizeMessage(-100, 2, 2, "org/repo", "user", "msg2", store)
	if evt2 == nil || evt2.SessionKey != "org/repo/pulls/5" {
		t.Error("expected PR event")
	}

	// Unmapped thread
	evt3 := NormalizeMessage(-100, 3, 3, "org/repo", "user", "msg3", store)
	if evt3 != nil {
		t.Error("expected nil for unmapped thread")
	}
}
