package lifecycle

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLifecycleRecordAndGet(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lifecycle.db")

	// Pass nil forgejo client — we only test persistence here
	lc, err := New(dbPath, nil, nil)
	if err != nil {
		t.Fatalf("new lifecycle: %v", err)
	}

	if err := lc.RecordTransition(ctx, "test/session", "", StateWorking, "start"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := lc.RecordTransition(ctx, "test/session", StateWorking, StatePRCreated, "pr #1"); err != nil {
		t.Fatalf("record: %v", err)
	}

	state, err := lc.GetState(ctx, "test/session")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state != StatePRCreated {
		t.Fatalf("expected %s, got %s", StatePRCreated, state)
	}

	// Unknown session should return empty string
	unknown, err := lc.GetState(ctx, "no/such/session")
	if err != nil {
		t.Fatalf("get unknown: %v", err)
	}
	if unknown != "" {
		t.Fatalf("expected empty for unknown session, got %s", unknown)
	}
}

func TestLifecycleFailedSessions(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lifecycle.db")

	lc, err := New(dbPath, nil, nil)
	if err != nil {
		t.Fatalf("new lifecycle: %v", err)
	}

	_ = lc.RecordTransition(ctx, "s1", StateWorking, StateFailedError, "panic")
	_ = lc.RecordTransition(ctx, "s2", StateWorking, StateCompleted, "ok")
	_ = lc.RecordTransition(ctx, "s3", StateWorking, StateFailedMaxTurns, "exhausted")

	failed, err := lc.ListFailedSessions(ctx)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(failed) != 2 {
		t.Fatalf("expected 2 failed sessions, got %d", len(failed))
	}
	want := map[string]bool{"s1": true, "s3": true}
	for _, k := range failed {
		if !want[k] {
			t.Fatalf("unexpected failed session %s", k)
		}
	}
}

func TestLifecycleForgejoLabels(t *testing.T) {
	// This test would need a real or mocked Forgejo server.
	// Skipping integration test; covered by forgejo client tests.
	t.Skip("integration: requires Forgejo server")
}

func TestNewWithMissingDir(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "subdir", "lifecycle.db")

	// Create the parent dir first
	lc, err := New(dbPath, nil, nil)
	if err != nil {
		t.Fatalf("new lifecycle with nested dir: %v", err)
	}

	_ = lc.RecordTransition(ctx, "s1", StateCreated, StateWorking, "begin")
	state, err := lc.GetState(ctx, "s1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state != StateWorking {
		t.Fatalf("expected %s, got %s", StateWorking, state)
	}
}
