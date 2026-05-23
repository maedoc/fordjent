package agent

import (
	"testing"

	"github.com/fordjent/fordjent/internal/provider"
)

func TestContextTrackerEstimateTokens(t *testing.T) {
	ct := NewContextTracker(1000, 0.8, 8)
	msgs := []provider.Message{
		{Role: "system", Content: "You are an agent."},
		{Role: "user", Content: "Hello world"},
	}
	est := ct.EstimateTokens(msgs)
	// "You are an agent." = 17 chars, "Hello world" = 11 chars, roles ~24 chars
	// total ~52 chars / 4 = ~13 tokens
	if est < 3 || est > 20 {
		t.Fatalf("unexpected estimate: %d", est)
	}
}

func TestContextTrackerShouldCompact(t *testing.T) {
	ct := NewContextTracker(100, 0.8, 2) // threshold at 80 tokens
	// Message with 400 chars = ~100 tokens, over 80 threshold
	msgs := []provider.Message{
		{Role: "user", Content: string(make([]byte, 400))},
	}
	if !ct.ShouldCompact(msgs) {
		t.Fatal("expected compaction for large message")
	}

	// Small messages should not compact
	msgs2 := []provider.Message{
		{Role: "user", Content: "hi"},
	}
	if ct.ShouldCompact(msgs2) {
		t.Fatal("unexpected compaction for tiny message")
	}
}

func TestContextTrackerCompactKeepsRecent(t *testing.T) {
	ct := NewContextTracker(1000, 0.8, 3)
	msgs := []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "msg1"},
		{Role: "assistant", Content: "resp1"},
		{Role: "user", Content: "msg2"},
		{Role: "assistant", Content: "resp2"},
		{Role: "user", Content: "msg3"},
		{Role: "assistant", Content: "resp3"},
	}
	compact := ct.Compact(msgs)
	// Should keep compaction marker + last 3 messages (no more preserving old system message)
	if len(compact) != 4 {
		t.Fatalf("expected 4 messages after compact, got %d", len(compact))
	}
	if compact[0].Content != "[Context Compacted] Earlier conversation history has been removed to stay within token limits. Continue from the latest context below." {
		t.Fatal("expected compaction marker")
	}
	// Last message should be resp3
	if compact[len(compact)-1].Content != "resp3" {
		t.Fatalf("expected last message 'resp3', got %s", compact[len(compact)-1].Content)
	}
}

func TestContextTrackerCompactNotEnoughMessages(t *testing.T) {
	ct := NewContextTracker(1000, 0.8, 8)
	msgs := []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "msg1"},
	}
	compact := ct.Compact(msgs)
	if len(compact) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(compact))
	}
}
