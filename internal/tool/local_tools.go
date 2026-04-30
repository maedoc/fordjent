package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// bashTool executes shell commands in the repository root directory.
type bashTool struct {
	repoDir string
}

func NewBashTool(info SessionInfo) *bashTool {
	return &bashTool{repoDir: info.RepoDir()}
}

func (t *bashTool) Name() string { return "bash" }

func (t *bashTool) Description() string {
	return "Execute a shell command in the repository working directory. Use for git operations, file inspection, building, testing."
}

func (t *bashTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "Shell command to execute",
			},
			"timeout": map[string]interface{}{
				"type":        "integer",
				"description": "Timeout in seconds (default 30)",
			},
		},
		"required": []string{"command"},
	}
}

func (t *bashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if params.Timeout == 0 {
		params.Timeout = 30
	}

	timeout := time.Duration(params.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := "bash"
	if _, lookErr := exec.LookPath("bash"); lookErr != nil {
		shell = "sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", params.Command)
	cmd.Dir = t.repoDir

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + stderr.String()
	}

	if err != nil {
		return fmt.Sprintf("Exit error: %s\n%s", err, output), nil
	}

	return output, nil
}

// readFileTool reads file contents from the repository.
type readFileTool struct {
	repoDir string
}

func NewReadFileTool(info SessionInfo) *readFileTool {
	return &readFileTool{repoDir: info.RepoDir()}
}

func (t *readFileTool) Name() string { return "read_file" }

func (t *readFileTool) Description() string {
	return "Read the contents of a file in the repository. Returns file content up to 2000 lines."
}

func (t *readFileTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file (relative to repository root)",
			},
			"offset": map[string]interface{}{
				"type":        "integer",
				"description": "Line number to start reading from (1-indexed)",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of lines to read (default 2000)",
			},
		},
		"required": []string{"path"},
	}
}

func (t *readFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if params.Limit == 0 {
		params.Limit = 2000
	}

	absPath := filepath.Join(t.repoDir, params.Path)

	// Sanitize: if model passed an absolute path containing repoDir, extract the relative part.
	if strings.HasPrefix(params.Path, t.repoDir) {
		rel, err := filepath.Rel(t.repoDir, params.Path)
		if err == nil {
			absPath = filepath.Join(t.repoDir, rel)
		}
	}

	// Sanitize: if model passed "repo/<file>" and repoDir already ends with "repo", strip the prefix.
	relClean := strings.TrimPrefix(params.Path, "repo/")
	relClean = strings.TrimPrefix(relClean, "/")
	if relClean != params.Path {
		candidate := filepath.Join(t.repoDir, relClean)
		if _, err := os.Stat(candidate); err == nil {
			absPath = candidate
		}
	}

	f, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	defer f.Close()

	var lines []string
	lineNum := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lineNum++
		if lineNum < params.Offset {
			continue
		}
		if len(lines) >= params.Limit {
			break
		}
		lines = append(lines, fmt.Sprintf("%6d\t%s", lineNum, scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	return strings.Join(lines, "\n"), nil
}

// writeFileTool writes content to a file in the repository.
type writeFileTool struct {
	repoDir      string
	commitPrefix string
}

func NewWriteFileTool(info SessionInfo, cfg AgentConfig) *writeFileTool {
	return &writeFileTool{
		repoDir:      info.RepoDir(),
		commitPrefix: cfg.CommitPrefix(),
	}
}

func (t *writeFileTool) Name() string { return "write_file" }

func (t *writeFileTool) Description() string {
	return "Write content to a file in the repository. Creates parent directories if needed."
}

func (t *writeFileTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file (relative to repository root)",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "File content to write",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *writeFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	absPath := filepath.Join(t.repoDir, params.Path)

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return "", fmt.Errorf("create directories: %w", err)
	}

	if err := os.WriteFile(absPath, []byte(params.Content), 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return fmt.Sprintf("Written %d bytes to %s", len(params.Content), params.Path), nil
}

// gitTool handles git operations in the session.
type gitTool struct {
	repoDir string
}

func NewGitTool(info SessionInfo) *gitTool {
	return &gitTool{
		repoDir: info.RepoDir(),
	}
}

func (t *gitTool) Name() string { return "git" }

func (t *gitTool) Description() string {
	return "Execute git operations in the repository: status, diff, add, commit, branch, checkout, log, fetch, pull, rebase. Note: push is blocked; use forgejo_create_pr tool instead. IMPORTANT: before creating a PR, run 'git fetch origin' then 'git rebase origin/main' (two separate calls)."
}

func (t *gitTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "Git subcommand and arguments (e.g., 'status', 'log --oneline -10', 'checkout -b feature/foo')",
			},
		},
		"required": []string{"command"},
	}
}

func (t *gitTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	// Security: block all push commands — agent must use forgejo_create_pr tool
	if strings.HasPrefix(strings.TrimSpace(strings.ToLower(params.Command)), "push") ||
		strings.HasPrefix(strings.TrimSpace(strings.ToLower(params.Command)), "git push") {
		return "", fmt.Errorf("git push is not allowed. Use the forgejo_create_pr tool to submit changes via pull request")
	}

	cmdStr := params.Command
	cmdLower := strings.TrimSpace(strings.ToLower(cmdStr))
	isCommit := strings.HasPrefix(cmdLower, "commit") || strings.HasPrefix(cmdLower, "git commit")

	// Sanitize: replace newlines in commit messages with spaces to avoid shell
	// treating them as argument separators
	if isCommit {
		cmdStr = strings.ReplaceAll(cmdStr, "\\n", " ")
		cmdStr = strings.ReplaceAll(cmdStr, "\n", " ")
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}

	// If the LLM included 'git' as the first token, strip it so we don't double-invoke
	if strings.ToLower(parts[0]) == "git" {
		parts = parts[1:]
	}

	cmd := exec.CommandContext(ctx, "git", parts...)
	cmd.Dir = t.repoDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("git error: %s\n%s", err, string(out)), nil
	}

	// Auto-push after successful commit so forgejo_create_pr never sees a
	// missing remote branch. Use -u origin HEAD to always push current branch.
	if isCommit {
		pushCtx, pushCancel := context.WithTimeout(ctx, 30*time.Second)
		defer pushCancel()
		pushCmd := exec.CommandContext(pushCtx, "git", "push", "-u", "origin", "HEAD")
		pushCmd.Dir = t.repoDir
		pushOut, pushErr := pushCmd.CombinedOutput()
		if pushErr != nil {
			return fmt.Sprintf("%s\n[auto-push warning] %s\n%s", string(out), pushErr, string(pushOut)), nil
		}
		out = append(out, []byte(fmt.Sprintf("\n[auto-push] %s", strings.TrimSpace(string(pushOut))))...)
	}

	return string(out), nil
}
