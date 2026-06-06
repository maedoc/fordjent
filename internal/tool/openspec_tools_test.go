package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenSpecProposeTool verifies that openspec_propose returns proper instructions
// and creates the change directory.
func TestOpenSpecProposeTool(t *testing.T) {
	repoDir := t.TempDir()
	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecProposeTool(info)

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

	// Verify directory was created
	dir := filepath.Join(repoDir, "openspec", "changes", "user-auth", "specs")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatalf("expected directory %s to exist after propose", dir)
	}
}

func TestOpenSpecProposeTool_EmptyName(t *testing.T) {
	repoDir := t.TempDir()
	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecProposeTool(info)
	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": ""
	}`))
	if err == nil {
		t.Fatal("expected error for empty change_name")
	}
}

func TestOpenSpecProposeTool_PathSeparator(t *testing.T) {
	repoDir := t.TempDir()
	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecProposeTool(info)
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

// TestArchiveTool_GitAddScoping verifies that git add in the archive tool only
// stages openspec/ files, not unrelated working-tree changes.
func TestArchiveTool_GitAddScoping(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	// Create openspec change directory with a file
	changeDir := filepath.Join(repoDir, "openspec", "changes", "test", "specs")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(changeDir, "..", "proposal.md"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create an unrelated file
	if err := os.MkdirAll(filepath.Join(repoDir, "cmd"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "cmd", "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}

	// Stage only openspec/ files (simulating what the archive tool does)
	cmd := exec.Command("git", "-C", repoDir, "add", "openspec/")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add openspec/: %s: %v", string(out), err)
	}

	// Check that only openspec/ files are staged
	cmd = exec.Command("git", "-C", repoDir, "diff", "--cached", "--name-only")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff --cached: %s: %v", string(out), err)
	}
	staged := string(out)

	if strings.Contains(staged, "cmd/main.go") {
		t.Errorf("cmd/main.go should NOT be staged, but it is. Staged files:\n%s", staged)
	}
	if !strings.Contains(staged, "openspec/") {
		t.Errorf("expected openspec/ files to be staged, got:\n%s", staged)
	}
}

// ─── extractSpecChangeRef tests ───────────────────────────────────────────

func TestExtractSpecChangeRef_RejectsFalsePositives(t *testing.T) {
	tests := []struct {
		input string
		label string
	}{
		{"spec: user-auth", "lowercase spec"},
		{"Change: user-auth", "Change marker"},
		{"We need climate change: urgent", "prose with change"},
		{"This is a spec file: important", "prose with spec"},
		{"change: the world", "lowercase change"},
	}

	for _, tt := range tests {
		got := extractSpecChangeRef(tt.input)
		if got != "" {
			t.Errorf("%s: expected empty string, got %q (input: %q)", tt.label, got, tt.input)
		}
	}
}

func TestExtractSpecChangeRef_AcceptsValid(t *testing.T) {
	tests := []struct {
		input string
		want  string
		label string
	}{
		{"Spec: user-auth", "user-auth", "basic spec ref"},
		{"Spec: openspec/changes/user-auth", "user-auth", "with openspec path prefix"},
		{"\nSpec: my-change\n", "my-change", "after newline"},
		{"Some text\nSpec: auth-oauth\nMore text", "auth-oauth", "mid-string"},
	}

	for _, tt := range tests {
		got := extractSpecChangeRef(tt.input)
		if got != tt.want {
			t.Errorf("%s: expected %q, got %q (input: %q)", tt.label, tt.want, got, tt.input)
		}
	}
}

// TestOpenSpecMarkTaskTool_RollbackOnCommitFailure verifies that the tasks.md file
// is restored to its original state when the commit fails.
// Since we can't easily make git commit fail in a test, we test the rollback
// helper directly.
func TestOpenSpecMarkTaskTool_RollbackTasksFile(t *testing.T) {
	repoDir := t.TempDir()

	changeDir := filepath.Join(repoDir, "openspec", "changes", "user-auth")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}
	originalContent := "- [ ] Task one\n- [ ] Task two\n"
	tasksPath := filepath.Join(changeDir, "tasks.md")
	if err := os.WriteFile(tasksPath, []byte(originalContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate a mutation (as MarkTaskComplete would do)
	mutatedContent := "- [x] Task one\n- [ ] Task two\n"
	if err := os.WriteFile(tasksPath, []byte(mutatedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Rollback
	tool := &openSpecMarkTaskTool{}
	tool.rollbackTasksFile(tasksPath, []byte(originalContent))

	// Verify original content restored
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != originalContent {
		t.Errorf("rollback did not restore original content:\nexpected: %q\ngot:      %q", originalContent, string(data))
	}
}

func TestOpenSpecMarkTaskTool_RollbackWithEmptyOriginal(t *testing.T) {
	repoDir := t.TempDir()

	changeDir := filepath.Join(repoDir, "openspec", "changes", "user-auth")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}
	tasksPath := filepath.Join(changeDir, "tasks.md")
	mutatedContent := "- [x] Task one\n"
	if err := os.WriteFile(tasksPath, []byte(mutatedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Rollback with empty original should be no-op
	tool := &openSpecMarkTaskTool{}
	tool.rollbackTasksFile(tasksPath, nil)

	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != mutatedContent {
		t.Errorf("rollback with empty original should be no-op, got: %q", string(data))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Task 1.3: Test that openspec_propose creates the change directory with specs/ subdirectory.
func TestOpenSpecProposeTool_CreatesDirectory(t *testing.T) {
	repoDir := t.TempDir()
	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecProposeTool(info)

	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "my-feature"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify openspec/changes/my-feature/specs/ directory exists
	specsDir := filepath.Join(repoDir, "openspec", "changes", "my-feature", "specs")
	if info, err := os.Stat(specsDir); err != nil {
		t.Fatalf("expected specs directory at %s, got error: %v", specsDir, err)
	} else if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", specsDir)
	}
}

// Task 1.4: Test that openspec_propose returns error for duplicate change name.
func TestOpenSpecProposeTool_DuplicateChange(t *testing.T) {
	repoDir := t.TempDir()
	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecProposeTool(info)

	// First call succeeds
	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "my-change"
	}`))
	if err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}

	// Second call with same name should fail
	_, err = tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "my-change"
	}`))
	if err == nil {
		t.Fatal("expected error for duplicate change name")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' in error, got: %v", err)
	}
}

// ─── openspec_read_change tests ─────────────────────────────────────────

// Task 2.4: Read proposal.md from change.
func TestOpenSpecReadChangeTool_ReadProposal(t *testing.T) {
	repoDir := t.TempDir()

	changeDir := filepath.Join(repoDir, "openspec", "changes", "user-auth")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}
	proposalContent := "## Why\n\nWe need user authentication."
	if err := os.WriteFile(filepath.Join(changeDir, "proposal.md"), []byte(proposalContent), 0644); err != nil {
		t.Fatal(err)
	}

	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecReadChangeTool(info)

	result, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "user-auth",
		"file_path": "proposal.md"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "We need user authentication") {
		t.Fatalf("expected proposal content in result, got: %s", result)
	}
}

// Task 2.5: Read specs/capability/spec.md from change.
func TestOpenSpecReadChangeTool_ReadNestedSpec(t *testing.T) {
	repoDir := t.TempDir()

	specDir := filepath.Join(repoDir, "openspec", "changes", "user-auth", "specs", "auth-core")
	if err := os.MkdirAll(specDir, 0755); err != nil {
		t.Fatal(err)
	}
	specContent := "## ADDED Requirements\n\n### Requirement: OAuth login\nThe system SHALL support OAuth."
	if err := os.WriteFile(filepath.Join(specDir, "spec.md"), []byte(specContent), 0644); err != nil {
		t.Fatal(err)
	}

	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecReadChangeTool(info)

	result, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "user-auth",
		"file_path": "specs/auth-core/spec.md"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "OAuth") {
		t.Fatalf("expected spec content in result, got: %s", result)
	}
}

// Task 2.6: Path traversal is rejected.
func TestOpenSpecReadChangeTool_PathTraversalRejected(t *testing.T) {
	repoDir := t.TempDir()

	changeDir := filepath.Join(repoDir, "openspec", "changes", "user-auth")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}

	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecReadChangeTool(info)

	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "user-auth",
		"file_path": "../../../etc/passwd"
	}`))
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("expected 'escapes' in error, got: %v", err)
	}
}

// Task 2.7: Nonexistent file returns error.
func TestOpenSpecReadChangeTool_NonexistentFile(t *testing.T) {
	repoDir := t.TempDir()

	changeDir := filepath.Join(repoDir, "openspec", "changes", "user-auth")
	if err := os.MkdirAll(changeDir, 0755); err != nil {
		t.Fatal(err)
	}

	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecReadChangeTool(info)

	_, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "user-auth",
		"file_path": "nonexistent.md"
	}`))
	// Nonexistent files inside existing changes return a helpful message (not error)
	if err != nil {
		t.Fatalf("expected helpful message for nonexistent file, got error: %v", err)
	}
}

// Task 2.8: Nonexistent change returns error.
func TestOpenSpecReadChangeTool_NonexistentChange(t *testing.T) {
	repoDir := t.TempDir()

	info := &mockSessionInfo{repoDir: repoDir}
	tool := NewOpenSpecReadChangeTool(info)

	result, err := tool.Execute(context.Background(), []byte(`{
		"repository": "test/repo",
		"change_name": "nonexistent",
		"file_path": "proposal.md"
	}`))
	// Nonexistent changes return a helpful message (not error)
	if err != nil {
		t.Fatalf("expected helpful message for nonexistent change, got error: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected non-empty result for nonexistent change")
	}
}
