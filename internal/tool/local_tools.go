package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/sandbox"
)

// bashTool executes shell commands in the repository root directory.
type bashTool struct {
	repoDir       string
	agentCfg      AgentConfig
	sandboxCfg    sandbox.Config
	violCounter   *sandbox.ViolationCounter
	sessionKey    string
}

const maxBashOutput = 64 * 1024

type limitedWriter struct {
	w         *strings.Builder
	remain    int
	truncated bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.remain <= 0 {
		lw.truncated = true
		return len(p), nil
	}
	if len(p) > lw.remain {
		p = p[:lw.remain]
		lw.truncated = true
	}
	n, err := lw.w.Write(p)
	lw.remain -= n
	return len(p), err
}

func NewBashTool(info SessionInfo, cfg AgentConfig) *bashTool {
	return &bashTool{repoDir: info.RepoDir(), agentCfg: cfg, sandboxCfg: sandbox.DefaultConfig(info.RepoDir())}
}

// SetSandboxConfig overrides the default sandbox configuration.
func (t *bashTool) SetSandboxConfig(cfg sandbox.Config) {
	t.sandboxCfg = cfg
}

// SetViolationCounter sets the violation counter for sandbox error tracking.
func (t *bashTool) SetViolationCounter(counter *sandbox.ViolationCounter, sessionKey string) {
	t.violCounter = counter
	t.sessionKey = sessionKey
}

// bashBlockedPatterns are command substrings that are always blocked for safety.
var bashBlockedPatterns = []string{
	"rm -rf /",
	"mkfs.",
	"dd if=",
	"shutdown",
	"reboot",
	"poweroff",
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

	// Sandbox: block dangerous commands
	cmdLower := strings.ToLower(params.Command)
	for _, pattern := range bashBlockedPatterns {
		if strings.Contains(cmdLower, strings.ToLower(pattern)) {
			return "", fmt.Errorf("command blocked by safety policy: contains %q", pattern)
		}
	}

	// Block git push to protected branches (main, master, etc.)
	// Agents must use feature branches + forgejo_create_pr for the PR workflow.
	// Scaffold sessions set allow_protected_push=true to bypass this.
	if t.agentCfg != nil && !t.agentCfg.AllowProtectedPush() && strings.Contains(cmdLower, "git push") {
		for _, branch := range t.agentCfg.ProtectedBranches() {
			// Match patterns like: git push origin main, git push origin HEAD:main, git push -u origin main
			if strings.Contains(cmdLower, " "+strings.ToLower(branch)) ||
				strings.Contains(cmdLower, ":"+strings.ToLower(branch)) ||
				strings.Contains(cmdLower, "head:"+strings.ToLower(branch)) {
				return "", fmt.Errorf("git push to protected branch %q is blocked. Use a feature branch and forgejo_create_pr instead. Only scaffold sessions may push to main.", branch)
			}
		}
	}

	timeout := time.Duration(params.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := "bash"
	if _, lookErr := exec.LookPath("bash"); lookErr != nil {
		shell = "sh"
	}

	if t.sandboxCfg.Enabled && (sandbox.IsAvailable() || sandbox.IsSandboxExecAvailable()) {
		out, err := sandbox.RunShell(ctx, t.sandboxCfg, params.Command)
		output := string(out)
		if err != nil {
			if sandboxErr, ok := err.(*sandbox.SandboxError); ok {
				if t.violCounter != nil && sandboxErr.Violated {
					t.violCounter.OnViolation(ctx, t.sessionKey, *sandboxErr)
				} else if t.violCounter != nil {
					t.violCounter.OnSuccess(t.sessionKey)
				}
			}
			return fmt.Sprintf("Exit error: %s\n%s", err, output), nil
		}
		if t.violCounter != nil {
			t.violCounter.OnSuccess(t.sessionKey)
		}
		return output, nil
	}

	slog.Warn("sandbox not available, running bash command unsandboxed", "cmd", params.Command)

	cmd := exec.CommandContext(ctx, shell, "-c", params.Command)
	cmd.Dir = t.repoDir

	var stdout, stderr limitedWriter
	stdout = limitedWriter{w: &strings.Builder{}, remain: maxBashOutput}
	stderr = limitedWriter{w: &strings.Builder{}, remain: maxBashOutput}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := stdout.w.String()
	if stderr.w.Len() > 0 {
		output += "\n[stderr]\n" + stderr.w.String()
	}
	if stdout.truncated {
		output += "\n[stdout truncated at 65536 bytes]"
	}
	if stderr.truncated {
		output += "\n[stderr truncated at 65536 bytes]"
	}

	if err != nil {
		return fmt.Sprintf("Exit error: %s\n%s", err, output), nil
	}

	return output, nil
}

// readFileTool reads file contents from the repository.
type readFileTool struct {
	repoDir string
	cache   sync.Map // path → string (simple file content cache)
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
		Path   string   `json:"path"`
		Paths  []string `json:"paths"`
		Offset int      `json:"offset"`
		Limit  int      `json:"limit"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	// Batch mode: if 'paths' is provided, read multiple files
	if len(params.Paths) > 0 {
		var results []string
		for _, p := range params.Paths {
			content, err := t.readFile(ctx, p, params.Offset, params.Limit)
			if err != nil {
				results = append(results, fmt.Sprintf("=== %s ===\nERROR: %s", p, err))
			} else {
				results = append(results, fmt.Sprintf("=== %s ===\n%s", p, content))
			}
		}
		return strings.Join(results, "\n\n"), nil
	}

	// Single file mode
	return t.readFile(ctx, params.Path, params.Offset, params.Limit)
}

func containsNullByte(s string) bool {
	return strings.ContainsRune(s, '\x00')
}

func (t *readFileTool) readFile(ctx context.Context, path string, offset, limit int) (string, error) {
	if containsNullByte(path) {
		return "", fmt.Errorf("path contains null bytes: %q", path)
	}

	// Cache check for full-file reads (offset=0, limit=default)
	if offset <= 1 && limit == 0 {
		if cached, ok := t.cache.Load(path); ok {
			return cached.(string), nil
		}
	}
	if limit == 0 {
		limit = 2000
	}

	// Pre-clean the path to normalize ../ and redundant separators before joining,
	// matching write_file's approach. This provides defense-in-depth.
	cleanPath := filepath.Clean(path)

	absPath := filepath.Join(t.repoDir, cleanPath)

	// Sanitize: if model passed an absolute path containing repoDir, extract the relative part.
	if strings.HasPrefix(path, t.repoDir) {
		rel, err := filepath.Rel(t.repoDir, path)
		if err == nil {
			absPath = filepath.Join(t.repoDir, rel)
		}
	}

	// Sanitize: if model passed "repo/<file>" and repoDir already ends with "repo", strip the prefix.
	relClean := strings.TrimPrefix(path, "repo/")
	relClean = strings.TrimPrefix(relClean, "/")
	if relClean != path {
		candidate := filepath.Join(t.repoDir, relClean)
		if _, err := os.Stat(candidate); err == nil {
			absPath = candidate
		}
	}

	// Containment check: ensure the resolved path does not escape the repository root.
	absPath = filepath.Clean(absPath)
	repoClean := filepath.Clean(t.repoDir) + string(os.PathSeparator)
	if !strings.HasPrefix(absPath, repoClean) {
		return "", fmt.Errorf("path escapes repository root: %s", path)
	}

	// Defense-in-depth: verify resolved path is within repo root using filepath.Rel.
	if rel, err := filepath.Rel(filepath.Clean(t.repoDir), absPath); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes repository root: %s", path)
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
		if lineNum < offset {
			continue
		}
		if len(lines) >= limit {
			break
		}
		lines = append(lines, fmt.Sprintf("%6d\t%s", lineNum, scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	result := strings.Join(lines, "\n")

	// Cache full-file reads
	if offset <= 1 && limit == 2000 {
		t.cache.Store(path, result)
	}

	return result, nil
}

// writeFileTool writes content to a file in the repository.
type writeFileTool struct {
	repoDir      string
	commitPrefix string
	dryRun       bool
}

func NewWriteFileTool(info SessionInfo, cfg AgentConfig) *writeFileTool {
	return &writeFileTool{
		repoDir:      info.RepoDir(),
		commitPrefix: cfg.CommitPrefix(),
		dryRun:       cfg.DryRun(),
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

	if containsNullByte(params.Path) {
		return "", fmt.Errorf("path contains null bytes: %q", params.Path)
	}

	if t.dryRun {
		return fmt.Sprintf("[dry-run] Would write %d bytes to %s", len(params.Content), params.Path), nil
	}

	absPath := filepath.Join(t.repoDir, filepath.Clean(params.Path))
	repoClean := filepath.Clean(t.repoDir) + string(os.PathSeparator)
	if !strings.HasPrefix(absPath, repoClean) {
		return "", fmt.Errorf("path escapes repository root: %s", params.Path)
	}

	// Defense-in-depth: verify resolved path is within repo root using filepath.Rel.
	if rel, err := filepath.Rel(filepath.Clean(t.repoDir), absPath); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes repository root: %s", params.Path)
	}

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
	repoDir     string
	agentCfg    AgentConfig
	sandboxCfg  sandbox.Config
	violCounter *sandbox.ViolationCounter
	sessionKey  string
}

func NewGitTool(info SessionInfo, cfg AgentConfig) *gitTool {
	return &gitTool{
		repoDir:    info.RepoDir(),
		agentCfg:   cfg,
		sandboxCfg: sandbox.DefaultConfig(info.RepoDir()),
	}
}

// SetSandboxConfig overrides the default sandbox configuration.
func (t *gitTool) SetSandboxConfig(cfg sandbox.Config) {
	t.sandboxCfg = cfg
}

// SetViolationCounter sets the violation counter for sandbox error tracking.
func (t *gitTool) SetViolationCounter(counter *sandbox.ViolationCounter, sessionKey string) {
	t.violCounter = counter
	t.sessionKey = sessionKey
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

	var out []byte
	if t.sandboxCfg.Enabled && (sandbox.IsAvailable() || sandbox.IsSandboxExecAvailable()) {
		sandboxOut, sandboxErr := sandbox.Run(ctx, t.sandboxCfg, "git", parts...)
		out = sandboxOut
		if sandboxErr != nil {
			if se, ok := sandboxErr.(*sandbox.SandboxError); ok {
				if t.violCounter != nil && se.Violated {
					t.violCounter.OnViolation(ctx, t.sessionKey, *se)
				} else if t.violCounter != nil {
					t.violCounter.OnSuccess(t.sessionKey)
				}
			}
			return fmt.Sprintf("git error: %s\n%s", sandboxErr, string(out)), nil
		}
		if t.violCounter != nil {
			t.violCounter.OnSuccess(t.sessionKey)
		}
	} else {
		slog.Warn("sandbox not available, running git command unsandboxed", "cmd", cmdStr)
		cmd := exec.CommandContext(ctx, "git", parts...)
		cmd.Dir = t.repoDir
		var err error
		out, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Sprintf("git error: %s\n%s", err, string(out)), nil
		}
	}

	// After successful commit, verify code compiles and tests pass BEFORE
	// pushing. This catches broken code early, not just at PR creation.
	if isCommit {
		verifyCtx, verifyCancel := context.WithTimeout(ctx, 60*time.Second)
		defer verifyCancel()

		buildCmd := exec.CommandContext(verifyCtx, "go", "build", "./...")
		buildCmd.Dir = t.repoDir
		if buildOut, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
			return fmt.Sprintf("%s\n[verify error] go build ./... failed:\n%s\n%s",
				string(out), buildErr, string(buildOut)), nil
		}

		testCmd := exec.CommandContext(verifyCtx, "go", "test", "./...", "-count=1")
		testCmd.Dir = t.repoDir
		if testOut, testErr := testCmd.CombinedOutput(); testErr != nil {
			return fmt.Sprintf("%s\n[verify error] go test ./... failed:\n%s\n%s",
				string(out), testErr, string(testOut)), nil
		}

		lintCmd := exec.CommandContext(verifyCtx, "golangci-lint", "run", "./...")
		lintCmd.Dir = t.repoDir
		if lintOut, lintErr := lintCmd.CombinedOutput(); lintErr != nil {
			// golangci-lint may not be installed — only fail if it IS installed and finds issues
			if !strings.Contains(lintErr.Error(), "executable file not found") {
				return fmt.Sprintf("%s\n[verify error] golangci-lint failed:\n%s", string(out), string(lintOut)), nil
			}
		}

		// Auto-push after successful commit so forgejo_create_pr never sees a
		// missing remote branch. Use -u origin HEAD to always push current branch.
		//
		// Guard: if the current branch is main/master, skip auto-push and warn.
		// Block auto-push if on a protected branch. Agents must use feature
		// branches + forgejo_create_pr for the PR-based workflow.
		branchCmd := exec.CommandContext(ctx, "git", "-C", t.repoDir, "rev-parse", "--abbrev-ref", "HEAD")
		branchCmd.Dir = t.repoDir
		branchOut, _ := branchCmd.CombinedOutput()
		currentBranch := strings.TrimSpace(string(branchOut))
		isProtected := false
		if t.agentCfg != nil {
			for _, pb := range t.agentCfg.ProtectedBranches() {
				if currentBranch == pb {
					isProtected = true
					break
				}
			}
		}
		if isProtected && (t.agentCfg == nil || !t.agentCfg.AllowProtectedPush()) {
			return "", fmt.Errorf("commit on protected branch %q blocked. Create a feature branch first (e.g., git checkout -b feature/my-feature). Only scaffold sessions may commit on main.", currentBranch)
		} else {
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
	}

	return string(out), nil
}
