package telegram

import (
	"strings"
	"testing"
)

func TestSplitMessageShort(t *testing.T) {
	parts := splitMessage("hello", 4000)
	if len(parts) != 1 {
		t.Errorf("expected 1 part, got %d", len(parts))
	}
	if parts[0] != "hello" {
		t.Errorf("expected 'hello', got %s", parts[0])
	}
}

func TestSplitMessageExactLimit(t *testing.T) {
	msg := strings.Repeat("x", 4000)
	parts := splitMessage(msg, 4000)
	if len(parts) != 1 {
		t.Errorf("expected 1 part for exact limit, got %d", len(parts))
	}
}

func TestSplitMessageLong(t *testing.T) {
	msg := strings.Repeat("x", 10000)
	parts := splitMessage(msg, 4000)
	if len(parts) < 2 {
		t.Errorf("expected >= 2 parts, got %d", len(parts))
	}
	// Verify total length preserved
	total := 0
	for _, p := range parts {
		total += len(p)
		if len(p) > 4000 {
			t.Errorf("part too long: %d", len(p))
		}
	}
	if total != 10000 {
		t.Errorf("expected total 10000, got %d", total)
	}
}

func TestSplitMessageAtNewlines(t *testing.T) {
	// Build a message with newlines near split points
	line := strings.Repeat("x", 2000) + "\n"
	msg := strings.Repeat(line, 5) // 5 * 2001 = 10005 chars

	parts := splitMessage(msg, 4000)
	for i, p := range parts {
		if len(p) > 4000 {
			t.Errorf("part %d too long: %d", i, len(p))
		}
	}
}

func TestSplitMessageEmpty(t *testing.T) {
	parts := splitMessage("", 4000)
	if len(parts) != 1 {
		t.Errorf("expected 1 part for empty, got %d", len(parts))
	}
	if parts[0] != "" {
		t.Errorf("expected empty string, got %s", parts[0])
	}
}
