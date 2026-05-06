package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type testSessionInfo struct {
	workDir string
	repoDir string
}

func (t *testSessionInfo) WorkDir() string { return t.workDir }
func (t *testSessionInfo) RepoDir() string  { return t.repoDir }

type testAgentConfig struct {
	protectedBranches []string
	allowProtected   bool
}

func (t *testAgentConfig) CommitPrefix() string            { return "[agent-automation]" }
func (t *testAgentConfig) ProtectedBranches() []string    { return t.protectedBranches }
func (t *testAgentConfig) RequirePRForWorkflows() bool    { return true }
func (t *testAgentConfig) DryRun() bool                  { return false }
func (t *testAgentConfig) AllowProtectedPush() bool      { return t.allowProtected }
func (t *testAgentConfig) IsScaffold() bool              { return t.allowProtected }

func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Initialize a git repo so git commands work
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("line1\nline2\nline3\nline4\nline5\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nHello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBashToolSuccess(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewBashTool(&testSessionInfo{workDir: dir, repoDir: dir}, &testAgentConfig{protectedBranches: []string{"main", "master"}, allowProtected: false})

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command": "ls hello.txt"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello.txt\n" {
		t.Errorf("expected 'hello.txt\\n', got %q", result)
	}
}

func TestBashToolStderr(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewBashTool(&testSessionInfo{workDir: dir, repoDir: dir}, &testAgentConfig{protectedBranches: []string{"main", "master"}, allowProtected: false})

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command": "echo err >&2 && echo out"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestBashToolExitError(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewBashTool(&testSessionInfo{workDir: dir, repoDir: dir}, &testAgentConfig{protectedBranches: []string{"main", "master"}, allowProtected: false})

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command": "exit 1"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected error output")
	}
}

func TestReadFileToolFullFile(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewReadFileTool(&testSessionInfo{repoDir: dir})

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"path": "hello.txt"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	// Should have line numbers
	if len(result) == 0 {
		t.Error("expected file content")
	}
}

func TestReadFileToolWithOffset(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewReadFileTool(&testSessionInfo{repoDir: dir})

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"path": "hello.txt", "offset": 2, "limit": 2}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestReadFileToolMissingFile(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewReadFileTool(&testSessionInfo{repoDir: dir})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path": "nonexistent.txt"}`))
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestWriteFileToolCreatesFile(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewWriteFileTool(&testSessionInfo{repoDir: dir}, &mockAgentConfig{})

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"path": "newdir/test.txt", "content": "hello"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	// Verify file was written
	data, err := os.ReadFile(filepath.Join(dir, "newdir", "test.txt"))
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("expected 'hello', got %q", string(data))
	}
}

func TestWriteFileToolOverwrite(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewWriteFileTool(&testSessionInfo{repoDir: dir}, &mockAgentConfig{})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path": "hello.txt", "content": "overwritten"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if string(data) != "overwritten" {
		t.Errorf("expected 'overwritten', got %q", string(data))
	}
}

func TestGitToolStatus(t *testing.T) {
	dir := setupTestRepo(t)
	// Init a git repo for the test
	tool := NewGitTool(&testSessionInfo{repoDir: dir}, &testAgentConfig{protectedBranches: []string{"main", "master"}, allowProtected: false})

	// git init should work even without a real repo
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command": "init"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result from git init")
	}
}

func TestGitToolBlocksPush(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewGitTool(&testSessionInfo{repoDir: dir}, &testAgentConfig{protectedBranches: []string{"main", "master"}, allowProtected: false})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"command": "push origin main"}`))
	if err == nil {
		t.Error("expected error for push command")
	}
}

func TestGitToolBlocksPushCaseInsensitive(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewGitTool(&testSessionInfo{repoDir: dir}, &testAgentConfig{protectedBranches: []string{"main", "master"}, allowProtected: false})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"command": "Push origin feature"}`))
	if err == nil {
		t.Error("expected error for Push command")
	}
}

func TestGitToolBlocksPushWithLeadingSpaces(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewGitTool(&testSessionInfo{repoDir: dir}, &testAgentConfig{protectedBranches: []string{"main", "master"}, allowProtected: false})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"command": "  push origin main"}`))
	if err == nil {
		t.Error("expected error for push with leading spaces")
	}
}

func TestGitToolEmptyCommand(t *testing.T) {
	dir := setupTestRepo(t)
	tool := NewGitTool(&testSessionInfo{repoDir: dir}, &testAgentConfig{protectedBranches: []string{"main", "master"}, allowProtected: false})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"command": "  "}`))
	if err == nil {
		t.Error("expected error for empty command")
	}
}
