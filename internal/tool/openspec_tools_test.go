package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenSpecProposeTool verifies that openspec_propose returns proper instructions.
func TestOpenSpecProposeTool(t *testing.T) {
	tool := NewOpenSpecProposeTool()

	if tool.Name() != "openspec_propose" {
		t.Fatalf("expected openspec_propose, got %s", tool.Name())
	}

	result, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "user-auth"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(result, "Creating change: user-auth") {
		t.Fatalf("expected result to start with 'Creating change: user-auth', got %q", result[:min(len(result), len("Creating change:"))])
	}
	if !strings.Contains(result, "openspec/changes/user-auth") {
		t.Fatal("expected result to mention openspec/changes/user-auth")
	}
	if !strings.Contains(result, "proposal.md") {
		t.Fatal("expected result to mention proposal.md")
	}
	if !strings.Contains(result, "tasks.md") {
		t.Fatal("expected result to mention tasks.md")
	}
	if !strings.Contains(result, "git checkout") {
		t.Fatal("expected result to include git workflow instructions")
	}
}

func TestOpenSpecProposeTool_EmptyName(t *testing.T) {
	tool := NewOpenSpecProposeTool()
	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": ""
	}`))
	if err == nil {
		t.Fatal("expected error for empty change_name")
	}
}

func TestOpenSpecProposeTool_PathSeparator(t *testing.T) {
	tool := NewOpenSpecProposeTool()
	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "foo/bar"
	}`))
	if err == nil {
		t.Fatal("expected error for path separator in change_name")
	}
}

// TestOpenSpecGetTasksTool tests parsing tasks from a temp repo.
func TestOpenSpecGetTasksTool(t *testing.T) {
	repoDir := t.TempDir()

	changeDir := filepath.Join(repoDir, "openspec", "changes", "user-auth")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}
	tasksContent := `## 1. Auth Module

- [x] Create auth module structs
- [ ] Implement OAuth flow [parallel]
- [ ] Write integration tests

## 2. Deployment

- [ ] Deploy to staging
`
	if err := os.WriteFile(filepath.Join(changeDir, "tasks.md"), []byte(tasksContent), 0644); err != nil {
		t.Fatal(err)
	}

	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecGetTasksTool(info)

	result, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "user-auth"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "4 total") {
		t.Fatalf("expected '4 total' in result, got: %s", result)
	}
	if !strings.Contains(result, "1 complete") {
		t.Fatalf("expected '1 complete' in result, got: %s", result)
	}
	if !strings.Contains(result, "Implement OAuth flow") {
		t.Fatalf("expected task description in result, got: %s", result)
	}
}

// TestOpenSpecReadSpecTool tests reading a spec file.
func TestOpenSpecReadSpecTool(t *testing.T) {
	repoDir := t.TempDir()

	specDir := filepath.Join(repoDir, "openspec", "specs", "auth-oauth")
	if err := os.MkdirAll(specDir, 0755); err != nil {
		t.Fatal(err)
	}
	specContent := `## ADDED Requirements

### Requirement: OAuth login
The system SHALL support OAuth 2.0 login.
`
	if err := os.WriteFile(filepath.Join(specDir, "spec.md"), []byte(specContent), 0644); err != nil {
		t.Fatal(err)
	}

	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecReadSpecTool(info)

	result, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"capability_name": "auth-oauth"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "OAuth 2.0") {
		t.Fatalf("expected spec content in result, got: %s", result)
	}
}

func TestOpenSpecReadSpecTool_NotFound(t *testing.T) {
	repoDir := t.TempDir()
	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecReadSpecTool(info)

	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"capability_name": "nonexistent"
	}`))
	if err == nil {
		t.Fatal("expected error for nonexistent spec")
	}
}

// TestOpenSpecMarkTaskTool tests marking a task complete.
func TestOpenSpecMarkTaskTool(t *testing.T) {
	repoDir := t.TempDir()

	changeDir := filepath.Join(repoDir, "openspec", "changes", "user-auth")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}
	tasksContent := `## Tasks

- [ ] Task one
- [ ] Task two [parallel]
- [x] Task three
`
	if err := os.WriteFile(filepath.Join(changeDir, "tasks.md"), []byte(tasksContent), 0644); err != nil {
		t.Fatal(err)
	}

	initGitRepo(t, repoDir)

	info := &mockSessionInfo{repoDir: repoDir}
	cfg := &mockAgentConfig{}
	tool := NewOpenSpecMarkTaskTool(info, cfg)

	result, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "user-auth",
		"task_index": 2
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "Task 2 marked complete") {
		t.Fatalf("expected success message, got: %s", result)
	}

	data, err := os.ReadFile(filepath.Join(changeDir, "tasks.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, "- [x] Task two") {
		t.Fatalf("expected task 2 to be marked [x], got:\n%s", content)
	}
	if !strings.Contains(content, "- [x] Task three") {
		t.Fatalf("expected task 3 to remain [x], got:\n%s", content)
	}
}

func TestOpenSpecMarkTaskTool_OutOfRange(t *testing.T) {
	repoDir := t.TempDir()

	changeDir := filepath.Join(repoDir, "openspec", "changes", "user-auth")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}
	tasksContent := `## Tasks

- [ ] Task one
`
	if err := os.WriteFile(filepath.Join(changeDir, "tasks.md"), []byte(tasksContent), 0644); err != nil {
		t.Fatal(err)
	}

	initGitRepo(t, repoDir)

	info := &mockSessionInfo{repoDir: repoDir}
	cfg := &mockAgentConfig{}
	tool := NewOpenSpecMarkTaskTool(info, cfg)

	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "user-auth",
		"task_index": 99
	}`))
	if err == nil {
		t.Fatal("expected error for out-of-range task index")
	}
}

func initGitRepo(t *testing.T, repoDir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "add", "-A"},
		{"git", "commit", "-m", "initial", "--allow-empty"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("%v: %s (non-fatal)", args, string(out))
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
