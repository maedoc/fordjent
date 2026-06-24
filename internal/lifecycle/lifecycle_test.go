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

// TestReworkCounter covers the first/third/fourth attempt behavior of the
// per-PR rework counter: increments correctly, returns 0 for unknown keys,
// and ResetRework clears the row back to 0.
func TestReworkCounter(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lifecycle.db")
	lc, err := New(dbPath, nil, nil)
	if err != nil {
		t.Fatalf("new lifecycle: %v", err)
	}

	// Unknown PR returns 0.
	n, err := lc.GetRework(ctx, "fjadmin/testbed", 7)
	if err != nil {
		t.Fatalf("get rework unknown: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 attempts for unknown PR, got %d", n)
	}

	// First three attempts increment.
	caps := []int{1, 2, 3}
	for i, want := range caps {
		got, err := lc.IncrementRework(ctx, "fjadmin/testbed", 7)
		if err != nil {
			t.Fatalf("increment %d: %v", i+1, err)
		}
		if got != want {
			t.Errorf("increment %d: expected %d, got %d", i+1, want, got)
		}
	}
	n, _ = lc.GetRework(ctx, "fjadmin/testbed", 7)
	if n != 3 {
		t.Errorf("expected 3 attempts after 3 increments, got %d", n)
	}

	// A different PR has its own counter (key isolation).
	got2, err := lc.IncrementRework(ctx, "fjadmin/testbed", 9)
	if err != nil {
		t.Fatalf("increment for other PR: %v", err)
	}
	if got2 != 1 {
		t.Errorf("expected 1 for fresh PR, got %d", got2)
	}

	// ResetRework clears the counter.
	if err := lc.ResetRework(ctx, "fjadmin/testbed", 7); err != nil {
		t.Fatalf("reset: %v", err)
	}
	n, _ = lc.GetRework(ctx, "fjadmin/testbed", 7)
	if n != 0 {
		t.Errorf("expected 0 after reset, got %d", n)
	}
}

// TestReworkCounter_InvalidArguments verifies the input guards.
func TestReworkCounter_InvalidArguments(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lifecycle.db")
	lc, err := New(dbPath, nil, nil)
	if err != nil {
		t.Fatalf("new lifecycle: %v", err)
	}
	if _, err := lc.IncrementRework(ctx, "", 7); err == nil {
		t.Error("expected error for empty repo")
	}
	if _, err := lc.IncrementRework(ctx, "foo", 0); err == nil {
		t.Error("expected error for pr=0")
	}
}
