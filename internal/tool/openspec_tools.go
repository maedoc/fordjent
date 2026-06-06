package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fordjent/fordjent/internal/speccycle"
)

// ─── openspec_propose ────────────────────────────────────────────────

type openSpecProposeTool struct {
	sessionInfo SessionInfo
}

func NewOpenSpecProposeTool(info SessionInfo) *openSpecProposeTool {
	return &openSpecProposeTool{sessionInfo: info}
}

func (t *openSpecProposeTool) Name() string { return "openspec_propose" }

func (t *openSpecProposeTool) Description() string {
	return "Validate a change name and return instructions for creating OpenSpec artifacts (proposal, design, specs, tasks). After calling this, use write_file to create the spec files in openspec/changes/<name>/."
}

func (t *openSpecProposeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"change_name": map[string]interface{}{
				"type":        "string",
				"description": "Name of the change (kebab-case, e.g., 'user-auth')",
			},
			"description": map[string]interface{}{
				"type":        "string",
				"description": "Brief description of the change (optional)",
			},
		},
		"required": []string{"repository", "change_name"},
	}
}

func (t *openSpecProposeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository  string `json:"repository"`
		ChangeName  string `json:"change_name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if params.ChangeName == "" {
		return "", fmt.Errorf("change_name is required")
	}
	if strings.Contains(params.ChangeName, "/") || strings.Contains(params.ChangeName, "\\") {
		return "", fmt.Errorf("change_name must not contain path separators")
	}

	// Auto-create the change directory so the PM can immediately write files.
	repoDir := t.sessionInfo.RepoDir()
	if repoDir == "" {
		return "", fmt.Errorf("repo directory not available")
	}
	sm := speccycle.NewSpecManager(repoDir)
	if err := sm.CreateChange(params.ChangeName); err != nil {
		return "", fmt.Errorf("create change %q: %w", params.ChangeName, err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Creating change: %s\n\n", params.ChangeName))

	if params.Description != "" {
		sb.WriteString(fmt.Sprintf("Description: %s\n\n", params.Description))
	}

	sb.WriteString(fmt.Sprintf(`Create the following files in openspec/changes/%s/:

1. **proposal.md** — Explains WHY (problem, what changes, capabilities, impact)
2. **design.md** — Explains HOW (architecture, decisions) — optional for simple changes
3. **specs/<capability>/spec.md** — One spec per capability with requirements and scenarios
4. **tasks.md** — Implementation checklist with - [ ] checkboxes

For complex changes, include design.md. For simple changes, skip it.

After writing all files, use git tool to commit and create a spec PR:

1. git checkout -b spec/%s
2. git add openspec/
3. git commit -m "spec: %s proposal"
4. git push origin spec/%s
5. forgejo_create_pr(base=main, head=spec/%s)
`, params.ChangeName, params.ChangeName, params.ChangeName, params.ChangeName, params.ChangeName))

	return sb.String(), nil
}

// ─── openspec_get_tasks ──────────────────────────────────────────────

type openSpecGetTasksTool struct {
	sessionInfo SessionInfo
}

func NewOpenSpecGetTasksTool(info SessionInfo) *openSpecGetTasksTool {
	return &openSpecGetTasksTool{sessionInfo: info}
}

func (t *openSpecGetTasksTool) Name() string { return "openspec_get_tasks" }

func (t *openSpecGetTasksTool) Description() string {
	return "List all tasks for an OpenSpec change, showing completion status and parallel tags. Call this before starting implementation to see what needs to be done."
}

func (t *openSpecGetTasksTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"change_name": map[string]interface{}{
				"type":        "string",
				"description": "Name of the OpenSpec change (e.g., 'user-auth')",
			},
		},
		"required": []string{"repository", "change_name"},
	}
}

func (t *openSpecGetTasksTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		ChangeName string `json:"change_name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if params.ChangeName == "" {
		return "", fmt.Errorf("change_name is required")
	}

	repoDir := t.sessionInfo.RepoDir()
	if repoDir == "" {
		return "", fmt.Errorf("repo directory not available")
	}

	sm := speccycle.NewSpecManager(repoDir)
	tasks, err := sm.ParseTasks(params.ChangeName)
	if err != nil {
		return "", fmt.Errorf("parse tasks: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Change: %s\n", params.ChangeName))

	total := len(tasks)
	complete := 0
	for _, task := range tasks {
		if task.Done {
			complete++
		}
	}
	sb.WriteString(fmt.Sprintf("Tasks: %d total, %d complete, %d remaining\n\n", total, complete, total-complete))

	for _, task := range tasks {
		status := "[ ]"
		if task.Done {
			status = "[x]"
		}
		tag := ""
		if task.Parallel {
			tag = " [parallel]"
		}
		sb.WriteString(fmt.Sprintf("%s %d. %s%s\n", status, task.Index, task.Description, tag))
	}

	return sb.String(), nil
}

// ─── openspec_read_spec ──────────────────────────────────────────────

type openSpecReadSpecTool struct {
	sessionInfo SessionInfo
}

func NewOpenSpecReadSpecTool(info SessionInfo) *openSpecReadSpecTool {
	return &openSpecReadSpecTool{sessionInfo: info}
}

func (t *openSpecReadSpecTool) Name() string { return "openspec_read_spec" }

func (t *openSpecReadSpecTool) Description() string {
	return "Read a capability spec by name. Checks openspec/specs/<name>/spec.md first, then falls back to active changes. Use before implementing to understand requirements."
}

func (t *openSpecReadSpecTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"capability_name": map[string]interface{}{
				"type":        "string",
				"description": "Capability name (kebab-case, e.g., 'auth-oauth')",
			},
		},
		"required": []string{"repository", "capability_name"},
	}
}

func (t *openSpecReadSpecTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository     string `json:"repository"`
		CapabilityName string `json:"capability_name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if params.CapabilityName == "" {
		return "", fmt.Errorf("capability_name is required")
	}

	repoDir := t.sessionInfo.RepoDir()
	if repoDir == "" {
		return "", fmt.Errorf("repo directory not available")
	}

	content, err := speccycle.ReadSpecFile(repoDir, params.CapabilityName)
	if err != nil {
		// Provide a helpful error: list available specs
		var hint string
		sm := speccycle.NewSpecManager(repoDir)
		changes, listErr := sm.ListChanges()
		if listErr == nil && len(changes) > 0 {
			var names []string
			for _, ch := range changes {
				names = append(names, ch.Name)
			}
			hint = fmt.Sprintf("\nActive changes: %s", strings.Join(names, ", "))
		}
		return "", fmt.Errorf("spec %q not found in merged specs or active changes.%s", params.CapabilityName, hint)
	}

	return content, nil
}

// ─── openspec_read_change ─────────────────────────────────────────────

type openSpecReadChangeTool struct {
	sessionInfo SessionInfo
}

func NewOpenSpecReadChangeTool(info SessionInfo) *openSpecReadChangeTool {
	return &openSpecReadChangeTool{sessionInfo: info}
}

func (t *openSpecReadChangeTool) Name() string { return "openspec_read_change" }

func (t *openSpecReadChangeTool) Description() string {
	return "Read a file within an OpenSpec change directory (proposal.md, design.md, specs, tasks.md). Use this to read spec artifacts that you or others have written."
}

func (t *openSpecReadChangeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"change_name": map[string]interface{}{
				"type":        "string",
				"description": "Name of the OpenSpec change (e.g., 'user-auth')",
			},
			"file_path": map[string]interface{}{
				"type":        "string",
				"description": "Relative path within the change directory (e.g., 'proposal.md', 'design.md', 'specs/auth-core/spec.md')",
			},
		},
		"required": []string{"repository", "change_name", "file_path"},
	}
}

func (t *openSpecReadChangeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		ChangeName string `json:"change_name"`
		FilePath   string `json:"file_path"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if params.ChangeName == "" {
		return "", fmt.Errorf("change_name is required")
	}
	if params.FilePath == "" {
		return "", fmt.Errorf("file_path is required")
	}

	repoDir := t.sessionInfo.RepoDir()
	if repoDir == "" {
		return "", fmt.Errorf("repo directory not available")
	}

	content, err := speccycle.ReadChangeFile(repoDir, params.ChangeName, params.FilePath)
	if err != nil {
		return "", fmt.Errorf("read change file: %w", err)
	}

	return content, nil
}

// ─── openspec_mark_task ──────────────────────────────────────────────

type openSpecMarkTaskTool struct {
	sessionInfo SessionInfo
	agentCfg    AgentConfig
}

func NewOpenSpecMarkTaskTool(info SessionInfo, cfg AgentConfig) *openSpecMarkTaskTool {
	return &openSpecMarkTaskTool{sessionInfo: info, agentCfg: cfg}
}

func (t *openSpecMarkTaskTool) Name() string { return "openspec_mark_task" }

func (t *openSpecMarkTaskTool) Description() string {
	return "Mark a task as complete in tasks.md. Call this after creating a PR for the task. Commits and pushes the updated tasks.md to the current branch."
}

func (t *openSpecMarkTaskTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"change_name": map[string]interface{}{
				"type":        "string",
				"description": "Name of the OpenSpec change (e.g., 'user-auth')",
			},
			"task_index": map[string]interface{}{
				"type":        "integer",
				"description": "Task index (1-based) to mark complete",
			},
		},
		"required": []string{"repository", "change_name", "task_index"},
	}
}

var (
	// gitAvailable checks once if git is on PATH.
	gitAvailable bool
)

func (t *openSpecMarkTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		ChangeName string `json:"change_name"`
		TaskIndex  int    `json:"task_index"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if params.ChangeName == "" {
		return "", fmt.Errorf("change_name is required")
	}
	if params.TaskIndex < 1 {
		return "", fmt.Errorf("task_index must be >= 1, got %d", params.TaskIndex)
	}

	repoDir := t.sessionInfo.RepoDir()
	if repoDir == "" {
		return "", fmt.Errorf("repo directory not available")
	}

	// Save original content for rollback if commit fails
	tasksFile := filepath.Join(repoDir, "openspec", "changes", params.ChangeName, "tasks.md")
	var originalContent []byte
	if data, err := os.ReadFile(tasksFile); err == nil {
		originalContent = data
	}

	// Mark the task as complete in tasks.md
	sm := speccycle.NewSpecManager(repoDir)
	if err := sm.MarkTaskComplete(params.ChangeName, params.TaskIndex); err != nil {
		return "", fmt.Errorf("mark task %d complete: %w", params.TaskIndex, err)
	}

	// Commit and push the updated tasks.md
	tasksRelPath := filepath.Join("openspec", "changes", params.ChangeName, "tasks.md")

	if err := t.gitAdd(ctx, repoDir, tasksRelPath); err != nil {
		slog.Warn("openspec_mark_task: git add failed", "error", err)
		t.rollbackTasksFile(tasksFile, originalContent)
		return fmt.Sprintf("Task %d marked complete in tasks.md, but git add failed: %s", params.TaskIndex, err), nil
	}

	if err := t.gitCommit(ctx, repoDir, fmt.Sprintf("task: mark task %d complete for %s", params.TaskIndex, params.ChangeName)); err != nil {
		slog.Warn("openspec_mark_task: git commit failed", "error", err)
		t.rollbackTasksFile(tasksFile, originalContent)
		return fmt.Sprintf("Task %d marked complete in tasks.md, but git commit failed: %s", params.TaskIndex, err), nil
	}

	if err := t.gitPush(ctx, repoDir); err != nil {
		slog.Warn("openspec_mark_task: git push failed", "error", err)
		return fmt.Sprintf("Task %d marked complete in tasks.md and committed, but push failed: %s", params.TaskIndex, err), nil
	}

	return fmt.Sprintf("Task %d marked complete in tasks.md and pushed.", params.TaskIndex), nil
}

func (t *openSpecMarkTaskTool) gitAdd(ctx context.Context, repoDir, path string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "add", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (t *openSpecMarkTaskTool) gitCommit(ctx context.Context, repoDir, msg string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "commit", "-m", msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (t *openSpecMarkTaskTool) gitPush(ctx context.Context, repoDir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "push", "-u", "origin", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// rollbackTasksFile restores the original tasks.md content after a commit failure.
func (t *openSpecMarkTaskTool) rollbackTasksFile(tasksFile string, originalContent []byte) {
	if len(originalContent) == 0 {
		return
	}
	if err := os.WriteFile(tasksFile, originalContent, 0644); err != nil {
		slog.Warn("openspec_mark_task: rollback failed, could not restore original tasks.md", "error", err)
		return
	}
	slog.Warn("openspec_mark_task: rolled back tasks.md after commit failure")
}

// ─── openspec_archive_change ─────────────────────────────────────────

type openSpecArchiveChangeTool struct {
	sessionInfo SessionInfo
	agentCfg    AgentConfig
}

func NewOpenSpecArchiveChangeTool(info SessionInfo, cfg AgentConfig) *openSpecArchiveChangeTool {
	return &openSpecArchiveChangeTool{sessionInfo: info, agentCfg: cfg}
}

func (t *openSpecArchiveChangeTool) Name() string { return "openspec_archive_change" }

func (t *openSpecArchiveChangeTool) Description() string {
	return "Archive a completed OpenSpec change. Moves openspec/changes/<name>/ to openspec/changes/archive/ and syncs delta specs to openspec/specs/<capability>/. Call this when all tasks are complete and all PRs are merged."
}

func (t *openSpecArchiveChangeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"change_name": map[string]interface{}{
				"type":        "string",
				"description": "Name of the OpenSpec change to archive (e.g., 'user-auth')",
			},
		},
		"required": []string{"repository", "change_name"},
	}
}

func (t *openSpecArchiveChangeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		ChangeName string `json:"change_name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if params.ChangeName == "" {
		return "", fmt.Errorf("change_name is required")
	}

	repoDir := t.sessionInfo.RepoDir()
	if repoDir == "" {
		return "", fmt.Errorf("repo directory not available")
	}

	sm := speccycle.NewSpecManager(repoDir)
	if err := sm.ArchiveChange(params.ChangeName); err != nil {
		return "", fmt.Errorf("archive change %q: %w", params.ChangeName, err)
	}

	// Detect if the archive created a new spec file that needs committing
	tasksPath, _ := filepath.Rel(repoDir, filepath.Join(repoDir, "openspec"))
	// Commit the change: git add openspec/ and commit
	if err := t.gitAddAll(ctx, repoDir); err != nil {
		slog.Warn("openspec_archive_change: git add failed", "error", err)
		return fmt.Sprintf("Change %q archived, but git add+commit failed: %s", params.ChangeName, err), nil
	}
	_ = tasksPath

	if err := t.gitCommit(ctx, repoDir, fmt.Sprintf("spec: archive %s", params.ChangeName)); err != nil {
		slog.Warn("openspec_archive_change: git commit failed", "error", err)
		return fmt.Sprintf("Change %q archived, but git commit failed: %s", params.ChangeName, err), nil
	}

	if err := t.gitPush(ctx, repoDir); err != nil {
		slog.Warn("openspec_archive_change: git push failed", "error", err)
		return fmt.Sprintf("Change %q archived and committed, but push failed: %s", params.ChangeName, err), nil
	}

	return fmt.Sprintf("Change %q archived, committed, and pushed.", params.ChangeName), nil
}

func (t *openSpecArchiveChangeTool) gitAddAll(ctx context.Context, repoDir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "add", "openspec/")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (t *openSpecArchiveChangeTool) gitCommit(ctx context.Context, repoDir, msg string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "commit", "-m", msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (t *openSpecArchiveChangeTool) gitPush(ctx context.Context, repoDir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "push", "-u", "origin", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// extractSpecChangeRef extracts an OpenSpec change name from an issue or PR body.
// Looks for patterns like "Spec: openspec/changes/<name>/..." or "Change: <name>"
// stored in the body by the scheduler when creating implementer issues.
var specChangeRefRegex = regexp.MustCompile(`(?:^|\n)Spec:\s*(?:openspec/changes/)?([a-z][a-z0-9-]+)`)

func extractSpecChangeRef(body string) string {
	matches := specChangeRefRegex.FindStringSubmatch(body)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}
