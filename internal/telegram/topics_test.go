package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewMappingStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	store, err := NewMappingStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("expected db file to be created")
	}
}

func TestCreateAndGetByThread(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	m := &TopicMapping{
		ChatID:      -1001234567890,
		ThreadID:   42,
		Repository: "org/repo",
		SessionKey: "org/repo/issues/42",
		IssueNumber: 42,
	}

	if err := store.CreateMapping(m); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	got, err := store.GetByThread(-1001234567890, 42)
	if err != nil {
		t.Fatalf("failed to get mapping: %v", err)
	}
	if got == nil {
		t.Fatal("expected mapping, got nil")
	}
	if got.SessionKey != "org/repo/issues/42" {
		t.Errorf("expected session key 'org/repo/issues/42', got %s", got.SessionKey)
	}
	if got.Repository != "org/repo" {
		t.Errorf("expected repository 'org/repo', got %s", got.Repository)
	}
	if got.IssueNumber != 42 {
		t.Errorf("expected issue number 42, got %d", got.IssueNumber)
	}
}

func TestGetByThreadNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	got, err := store.GetByThread(-100, 999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for non-existent mapping")
	}
}

func TestGetBySessionKey(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	m := &TopicMapping{
		ChatID:    -1001234567890,
		ThreadID:  7,
		Repository: "org/repo",
		SessionKey: "org/repo/pulls/7",
		PRNumber: 7,
	}
	store.CreateMapping(m)

	got, err := store.GetBySessionKey("org/repo/pulls/7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected mapping, got nil")
	}
	if got.ThreadID != 7 {
		t.Errorf("expected thread ID 7, got %d", got.ThreadID)
	}
	if got.PRNumber != 7 {
		t.Errorf("expected PR number 7, got %d", got.PRNumber)
	}
}

func TestGetBySessionKeyNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	got, err := store.GetBySessionKey("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for non-existent session key")
	}
}

func TestDeleteBySessionKey(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	store.CreateMapping(&TopicMapping{
		ChatID:    -100,
		ThreadID:  1,
		Repository: "org/repo",
		SessionKey: "org/repo/issues/1",
		IssueNumber: 1,
	})

	if err := store.DeleteBySessionKey("org/repo/issues/1"); err != nil {
		t.Fatalf("failed to delete: %v", err)
	}

	got, _ := store.GetBySessionKey("org/repo/issues/1")
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestUpsertMapping(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Create
	store.CreateMapping(&TopicMapping{
		ChatID:    -100,
		ThreadID:  1,
		Repository: "org/repo",
		SessionKey: "org/repo/issues/1",
		IssueNumber: 1,
	})

	// Upsert with same session key, different thread
	store.CreateMapping(&TopicMapping{
		ChatID:    -100,
		ThreadID:  99,
		Repository: "org/repo",
		SessionKey: "org/repo/issues/1",
		IssueNumber: 1,
	})

	got, _ := store.GetBySessionKey("org/repo/issues/1")
	if got == nil {
		t.Fatal("expected mapping after upsert")
	}
	if got.ThreadID != 99 {
		t.Errorf("expected updated thread ID 99, got %d", got.ThreadID)
	}
}

func TestMultipleMappings(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	for i := 1; i <= 5; i++ {
		store.CreateMapping(&TopicMapping{
			ChatID:    -100,
			ThreadID:  i,
			Repository: "org/repo",
			SessionKey: fmt.Sprintf("org/repo/issues/%d", i),
			IssueNumber: i,
		})
	}

	for i := 1; i <= 5; i++ {
		got, err := store.GetByThread(-100, i)
		if err != nil {
			t.Errorf("thread %d: unexpected error: %v", i, err)
		}
		if got == nil {
			t.Errorf("thread %d: expected mapping, got nil", i)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMappingStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create initial mapping
	store.CreateMapping(&TopicMapping{
		ChatID:    -100,
		ThreadID:  1,
		Repository: "org/repo",
		SessionKey: "org/repo/issues/1",
		IssueNumber: 1,
	})

	// Concurrent reads
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- true }()
			store.GetByThread(-100, 1)
			store.GetBySessionKey("org/repo/issues/1")
		}()
	}

	for i := 0; i < 10; i++ {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("timeout")
		}
	}
}
