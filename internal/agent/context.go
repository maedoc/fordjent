package agent

import (
	"log/slog"
	"math"

	"github.com/fordjent/fordjent/internal/provider"
)

// ContextTracker monitors context window usage and triggers compaction.
type ContextTracker struct {
	WindowSize        int
	CompactionThreshold float64 // e.g. 0.80
	KeepTurns         int       // Number of recent turns to preserve
	totalTokens       int
	totalTurns        int
}

// NewContextTracker creates a tracker with the given limits.
func NewContextTracker(windowSize int, threshold float64, keepTurns int) *ContextTracker {
	if windowSize <= 0 {
		windowSize = 128000
	}
	if threshold <= 0 || threshold > 1.0 {
		threshold = 0.80
	}
	if keepTurns <= 0 {
		keepTurns = 8
	}
	return &ContextTracker{
		WindowSize:          windowSize,
		CompactionThreshold: threshold,
		KeepTurns:           keepTurns,
	}
}

// TotalTurns returns the number of turns tracked.
func (t *ContextTracker) TotalTurns() int { return t.totalTurns }

// Update adds the latest usage to the tracker.
func (t *ContextTracker) Update(usage *provider.Usage) {
	if usage != nil {
		t.totalTokens += usage.TotalTokens
	}
	t.totalTurns++
}

// ShouldCompact returns true if the context should be compacted before the next call.
// It estimates tokens in the current message buffer.
func (t *ContextTracker) ShouldCompact(messages []provider.Message) bool {
	estimated := t.EstimateTokens(messages)
	thresholdTokens := int(float64(t.WindowSize) * t.CompactionThreshold)
	return estimated > thresholdTokens
}

// EstimateTokens performs a rough token estimate: ~4 chars ≈ 1 token.
func (t *ContextTracker) EstimateTokens(messages []provider.Message) int {
	var chars int
	for _, m := range messages {
		chars += len(m.Content)
		chars += len(m.Role)
		chars += len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Arguments)
			chars += len(tc.Function.Name)
		}
	}
	return chars / 4
}

// Compact drops old non-system messages, keeping the system prompt (implicitly at index 0
// if present) and the last KeepTurns messages. It injects a compaction marker.
func (t *ContextTracker) Compact(messages []provider.Message) []provider.Message {
	if len(messages) <= t.KeepTurns+1 {
		// Not enough to compact
		return messages
	}

	keepStart := len(messages) - t.KeepTurns
	if keepStart < 0 {
		keepStart = 0
	}

	// Build compacted context: drop old messages, keep compaction marker + recent turns.
	// Use "user" role for marker to avoid Scaleway API strictness about system-after-tool.
	var compacted []provider.Message
	compacted = append(compacted, provider.Message{
		Role:    "user",
		Content: "[Context Compacted] Earlier conversation history has been removed to stay within token limits. Continue from the latest context below.",
	})
	compacted = append(compacted, messages[keepStart:]...)

	newEstimate := t.EstimateTokens(compacted)
	slog.Info("compacted context",
		"before_messages", len(messages),
		"after_messages", len(compacted),
		"estimate_tokens", newEstimate,
		"window_size", t.WindowSize,
	)

	t.totalTokens = newEstimate // reset to estimated post-compaction
	return compacted
}

// Utilization returns the estimated fill ratio of the context window.
func (t *ContextTracker) Utilization(messages []provider.Message) float64 {
	estimated := t.EstimateTokens(messages)
	if t.WindowSize <= 0 {
		return 0
	}
	return math.Min(float64(estimated)/float64(t.WindowSize), 1.0)
}
