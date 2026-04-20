package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

func TestRecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Agent: config.AgentConfig{
			CommitPrefix: "[agent-automation]",
		},
		Memory: config.MemoryConfig{
			Enabled:        true,
			CompactionPath: "docs/issues",
		},
	}

	mem := New(cfg, dir, nil)

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 42, 0, "alice", "created")
	evt.SessionKey = "org/repo/issues/42"

	// Record a response
	mem.Record(context.Background(), evt, "I analyzed the code and found...", 0)

	// Record a tool call
	mem.RecordToolCall(context.Background(), evt, "read_file", `{"path": "main.go"}`, "package main...")

	// Query should return recent activity
	summary, err := mem.Query(context.Background(), evt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary == "" {
		t.Error("expected non-empty summary")
	}
}

func TestRecordJSONL(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Agent:  config.AgentConfig{CommitPrefix: "[agent-automation]"},
		Memory: config.MemoryConfig{Enabled: true, CompactionPath: "docs/issues"},
	}

	mem := New(cfg, dir, nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 1, 0, "bob", "opened")
	evt.SessionKey = "org/repo/issues/1"

	mem.Record(context.Background(), evt, "response text", 0)
	mem.Record(context.Background(), evt, "another response", 1)

	// Verify JSONL file exists
	jsonlPath := filepath.Join(dir, "memory.jsonl")
	if _, err := os.Stat(jsonlPath); os.IsNotExist(err) {
		t.Fatal("expected memory.jsonl to exist")
	}

	// Verify we can read it back
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("failed to read JSONL: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty JSONL file")
	}
}

func TestQueryEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Agent:  config.AgentConfig{CommitPrefix: "[agent-automation]"},
		Memory: config.MemoryConfig{Enabled: true, CompactionPath: "docs/issues"},
	}

	mem := New(cfg, dir, nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 99, 0, "charlie", "opened")
	evt.SessionKey = "org/repo/issues/99"

	summary, err := mem.Query(context.Background(), evt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "" {
		t.Errorf("expected empty summary for unknown session, got: %s", summary)
	}
}
