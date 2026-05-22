package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/fordjent/fordjent/internal/provider"
)

// Tool is the interface that all agent tools must implement.
type Tool interface {
	// Name returns the tool's identifier (e.g., "forgejo_comment").
	Name() string
	// Description returns the tool description shown to the LLM.
	Description() string
	// Parameters returns the JSON Schema for the tool's parameters.
	Parameters() map[string]interface{}
	// Execute runs the tool with the given arguments.
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry holds all registered tools and provides lookup and execution.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
	slog.Info("registered tool", "name", t.Name())
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Execute looks up and runs a tool by name.
func (r *Registry) Execute(ctx context.Context, name string, rawArgs string) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Execute(ctx, json.RawMessage(rawArgs))
}

// Tools returns all registered tools as LLM tool definitions.
func (r *Registry) Tools() []provider.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var defs []provider.ToolDef
	for _, t := range r.tools {
		defs = append(defs, provider.ToolDef{
			Type: "function",
			Function: provider.FunctionDef{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return defs
}

// Descriptions returns a formatted string of all tools for the system prompt.
func (r *Registry) Descriptions() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result string
	for _, t := range r.tools {
		result += fmt.Sprintf("- **%s**: %s\n", t.Name(), t.Description())
	}
	return result
}

// List returns all registered tool names.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var names []string
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// ToolsExcluding returns tool definitions excluding the named tools.
// Used to hide tools that the agent should not call (e.g., forgejo_comment after limit).
func (r *Registry) ToolsExcluding(exclude map[string]bool) []provider.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var defs []provider.ToolDef
	for _, t := range r.tools {
		if exclude[t.Name()] {
			continue
		}
		defs = append(defs, provider.ToolDef{
			Type: "function",
			Function: provider.FunctionDef{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return defs
}
