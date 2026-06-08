package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/fordjent/fordjent/internal/provider"
)

// ScopedWriter is implemented by tools that support path-based write restrictions.
type ScopedWriter interface {
	SetAllowedPrefixes(prefixes []string)
}

// ScopedPRCreator is implemented by the PR creation tool for scoped build/test gates.
type ScopedPRCreator interface {
	SetScopePkgs(pkgs []string)
}

// ScopedBasher is implemented by tools that support bash scope restrictions.
type ScopedBasher interface {
	SetScopePrefixes(prefixes []string)
}

// SetBashScope restricts bash file-writing commands to the given path prefixes.
func (r *Registry) SetBashScope(prefixes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tools["bash"]; ok {
		if sb, ok := t.(ScopedBasher); ok {
			sb.SetScopePrefixes(prefixes)
		}
	}
}

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

// SetWriteScope restricts write_file to the given path prefixes.
// No-op if write_file is not registered or doesn't implement ScopedWriter.
func (r *Registry) SetWriteScope(prefixes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tools["write_file"]; ok {
		if sw, ok := t.(ScopedWriter); ok {
			sw.SetAllowedPrefixes(prefixes)
		}
	}
}

// SetPRScope restricts the PR build/test gate to the given package paths.
// Converts "pkg/math/" to "./pkg/math" for Go import paths.
func (r *Registry) SetPRScope(scopePrefixes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tools["forgejo_create_pr"]; ok {
		if sp, ok := t.(ScopedPRCreator); ok {
			var pkgs []string
			for _, p := range scopePrefixes {
				pkgs = append(pkgs, "./"+strings.TrimSuffix(p, "/"))
			}
			sp.SetScopePkgs(pkgs)
		}
	}
}
