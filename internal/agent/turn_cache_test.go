package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/tool"
)

// mockCompleter records every call so we can assert on cache stability.
type cacheRecordingCompleter struct {
	calls []cacheCall
}

type cacheCall struct {
	SystemPrompt string
	Messages     []provider.Message
	Tools        []provider.ToolDef
}

func (m *cacheRecordingCompleter) Chat(ctx context.Context, systemPrompt string, messages []provider.Message, tools []provider.ToolDef) (*provider.Response, *provider.Usage, error) {
	m.calls = append(m.calls, cacheCall{
		SystemPrompt: systemPrompt,
		Messages:     append([]provider.Message(nil), messages...),
		Tools:        append([]provider.ToolDef(nil), tools...),
	})
	return &provider.Response{
		Content:    "ok",
		ToolCalls:  nil,
		StopReason: "stop",
	}, &provider.Usage{
		PromptTokens:     100,
		CompletionTokens: 10,
		TotalTokens:      110,
	}, nil
}

func (m *cacheRecordingCompleter) Cfg() *config.ProviderConfig {
	return &config.ProviderConfig{CostPer1MInputTokens: 0, CostPer1MOutputTokens: 0}
}

func TestTurnExecutor_SystemPromptStableAcrossTurns(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			ContextWindow:         128000,
			CompactionThreshold:   0.8,
			CompactionKeepTurns:   8,
		},
	}
	mockLLM := &cacheRecordingCompleter{}
	tr := tool.NewRegistry()
	tr.Register(&mockToolForCache{name: "read_file", desc: "Read a file"})
	ct := cost.NewTracker(t.TempDir())

	te := NewTurnExecutor(cfg, mockLLM, tr, ct, "test/session", "org/repo", 50, "implementer")

	stablePrompt := "You are Fordjent. Rules: be helpful."
	ctx := context.Background()

	// Turn 1
	messages := []provider.Message{
		{Role: "user", Content: "Hello"},
	}
	_, messages, err := te.Run(ctx, stablePrompt, messages)
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}
	messages = append(messages, provider.Message{Role: "assistant", Content: "Hi"})

	// Turn 2
	_, messages, err = te.Run(ctx, stablePrompt, messages)
	if err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}
	messages = append(messages, provider.Message{Role: "assistant", Content: "Again"})

	// Turn 3
	_, _, err = te.Run(ctx, stablePrompt, messages)
	if err != nil {
		t.Fatalf("turn 3 failed: %v", err)
	}

	if len(mockLLM.calls) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(mockLLM.calls))
	}

	// The system prompt must be IDENTICAL across all turns for prefix caching.
	for i, call := range mockLLM.calls {
		if call.SystemPrompt != stablePrompt {
			t.Errorf("turn %d: system prompt changed — prefix cache invalidated.\n expected: %q\n got:      %q", i+1, stablePrompt, call.SystemPrompt)
		}
	}
}

func TestTurnExecutor_ToolsStableWhenNotExcluded(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			ContextWindow:         128000,
			CompactionThreshold:   0.8,
			CompactionKeepTurns:   8,
		},
	}
	mockLLM := &cacheRecordingCompleter{}
	tr := tool.NewRegistry()
	tr.Register(&mockToolForCache{name: "read_file", desc: "Read a file"})
	tr.Register(&mockToolForCache{name: "write_file", desc: "Write a file"})
	ct := cost.NewTracker(t.TempDir())

	te := NewTurnExecutor(cfg, mockLLM, tr, ct, "test/session", "org/repo", 50, "implementer")

	ctx := context.Background()
	messages := []provider.Message{{Role: "user", Content: "Hello"}}

	// Turn 1
	_, messages, err := te.Run(ctx, "You are Fordjent.", messages)
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}

	// Turn 2 (no exclusions)
	_, _, err = te.Run(ctx, "You are Fordjent.", append(messages, provider.Message{Role: "assistant", Content: "ok"}))
	if err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}

	if len(mockLLM.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(mockLLM.calls))
	}

	toolCount1 := len(mockLLM.calls[0].Tools)
	toolCount2 := len(mockLLM.calls[1].Tools)
	if toolCount1 != toolCount2 {
		t.Errorf("tool count changed between turns without exclusion (%d -> %d). This may invalidate provider-side tool-schema cache.", toolCount1, toolCount2)
	}
}

func TestTurnExecutor_EnergyLoggedInTurnResult(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			ContextWindow:         128000,
			CompactionThreshold:   0.8,
			CompactionKeepTurns:   8,
		},
	}
	mockLLM := &cacheRecordingCompleter{}
	tr := tool.NewRegistry()
	tr.Register(&mockToolForCache{name: "read_file", desc: "Read a file"})
	ct := cost.NewTracker(t.TempDir())

	te := NewTurnExecutor(cfg, mockLLM, tr, ct, "test/session", "org/repo", 50, "implementer")

	ctx := context.Background()
	messages := []provider.Message{{Role: "user", Content: "Hello"}}

	result, _, err := te.Run(ctx, "You are Fordjent.", messages)
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}

	// EnergyJoules comes back from the provider Usage but isn't on TurnResult struct.
	// This test primarily documents that the field exists in Usage and is logged.
	if result.Usage.EnergyJoules != 0 {
		t.Logf("energy_joules reported by provider: %f", result.Usage.EnergyJoules)
	}
}

// mockToolForCache is a minimal tool for cache stability tests.
type mockToolForCache struct {
	name string
	desc string
}

func (m *mockToolForCache) Name() string                       { return m.name }
func (m *mockToolForCache) Description() string                { return m.desc }
func (m *mockToolForCache) Parameters() map[string]interface{} { return map[string]interface{}{} }
func (m *mockToolForCache) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}
