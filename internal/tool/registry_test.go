package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// mockSessionInfo implements SessionInfo for testing.
type mockSessionInfo struct {
	workDir string
	repoDir string
}

func (m *mockSessionInfo) WorkDir() string { return m.workDir }
func (m *mockSessionInfo) RepoDir() string  { return m.repoDir }

// mockAgentConfig implements AgentConfig for testing.
type mockAgentConfig struct {
	prefix    string
	protected []string
}

func (m *mockAgentConfig) CommitPrefix() string        { return m.prefix }
func (m *mockAgentConfig) ProtectedBranches() []string  { return m.protected }
func (m *mockAgentConfig) RequirePRForWorkflows() bool  { return true }

func TestRegistryRegisterAndGet(t *testing.T) {
	registry := NewRegistry()

	mockTool := &mockTool{
		name:        "test_tool",
		description: "A test tool",
		params: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"input": map[string]string{"type": "string"},
			},
		},
	}

	registry.Register(mockTool)

	got, ok := registry.Get("test_tool")
	if !ok {
		t.Error("expected tool to be found")
	}
	if got.Name() != "test_tool" {
		t.Errorf("expected test_tool, got %s", got.Name())
	}
}

func TestRegistryExecute(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&mockTool{
		name:        "echo",
		description: "Echo input",
		params:      map[string]interface{}{"type": "object"},
		executeFn: func(ctx context.Context, args json.RawMessage) (string, error) {
			return string(args), nil
		},
	})

	result, err := registry.Execute(context.Background(), "echo", `{"message": "hello"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"message": "hello"}` {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	registry := NewRegistry()
	_, err := registry.Execute(context.Background(), "nonexistent", "{}")
	if err == nil {
		t.Error("expected error for unknown tool")
	}
}

func TestRegistryToolsDefs(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&mockTool{name: "tool_a", description: "Tool A", params: map[string]interface{}{"type": "object"}})
	registry.Register(&mockTool{name: "tool_b", description: "Tool B", params: map[string]interface{}{"type": "object"}})

	defs := registry.Tools()
	if len(defs) != 2 {
		t.Errorf("expected 2 tool defs, got %d", len(defs))
	}

	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Function.Name] = true
	}
	if !names["tool_a"] || !names["tool_b"] {
		t.Errorf("expected tool_a and tool_b, got %v", names)
	}
}

func TestRegistryDescriptions(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&mockTool{name: "foo", description: "Does foo things", params: map[string]interface{}{}})

	desc := registry.Descriptions()
	if desc == "" {
		t.Error("expected non-empty descriptions")
	}
}

func TestRegistryList(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&mockTool{name: "a", description: "A", params: map[string]interface{}{}})
	registry.Register(&mockTool{name: "b", description: "B", params: map[string]interface{}{}})

	list := registry.List()
	if len(list) != 2 {
		t.Errorf("expected 2 tools, got %d", len(list))
	}
}

// mockTool is a test double for Tool interface.
type mockTool struct {
	name       string
	description string
	params     map[string]interface{}
	executeFn  func(ctx context.Context, args json.RawMessage) (string, error)
}

func (m *mockTool) Name() string                           { return m.name }
func (m *mockTool) Description() string                    { return m.description }
func (m *mockTool) Parameters() map[string]interface{}     { return m.params }
func (m *mockTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if m.executeFn != nil {
		return m.executeFn(ctx, args)
	}
	return "ok", nil
}
