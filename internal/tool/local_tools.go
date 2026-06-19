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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/sandbox"
)

// ralphGuardChecker is implemented by ralph.Guard to check spec path immutability.
type ralphGuardChecker interface {
	IsSpecPath(p string) bool
}

// bashTool executes shell commands in the repository root directory.
type bashTool struct {
	repoDir       string
	agentCfg      AgentConfig
	sandboxCfg    sandbox.Config
	violCounter   *sandbox.ViolationCounter
	sessionKey    string
	scopePrefixes []string // if set, block file writes outside these paths
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

// SetScopePrefixes restricts bash file-writing commands to the given path prefixes.
func (t *bashTool) SetScopePrefixes(prefixes []string) {
	t.scopePrefixes = prefixes
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

	// Scope restriction: block file-writing bash commands outside allowed prefixes
	if len(t.scopePrefixes) > 0 {
		// Detect file-writing patterns: > file, >> file, cat > file, cat <<EOF > file,
		// tee file, cp src dst, mv src dst, dd of=, tar -C, rsync, ln -s
		writePatterns := []*regexp.Regexp{
			regexp.MustCompile(`>(?:>]){0,1}\s*([a-zA-Z0-9_./-]+)`),
			regexp.MustCompile(`(?:cat|tee)\s+(?:-[aAeE]+\s+)*([a-zA-Z0-9_./-]+)`),
			regexp.MustCompile(`(?:cp|mv|install)\s+(?:\S+\s+)+([a-zA-Z0-9_./-]+)$`),
			regexp.MustCompile(`dd\s+.*of=([a-zA-Z0-9_./-]+)`),
			regexp.MustCompile(`tar\s+.*-C\s+([a-zA-Z0-9_./-]+)`),
			regexp.MustCompile(`rsync\s+.*\s+([a-zA-Z0-9_./-]+)\s*$`),
			regexp.MustCompile(`ln\s+(?:-\w*\s*)?s\s+\S+\s+([a-zA-Z0-9_./-]+)`),
		}
		for _, pat := range writePatterns {
			matches := pat.FindAllStringSubmatch(params.Command, -1)
			for _, m := range matches {
				targetPath := filepath.ToSlash(filepath.Clean(m[1]))
				// Resolve symlinks on the bash write target to prevent
				// scope bypass via symlink pointing outside allowed paths.
				absTarget := filepath.Join(t.repoDir, targetPath)
				resolvedTarget, symErr := filepath.EvalSymlinks(absTarget)
				if symErr == nil {
					// Successfully resolved — rederive the relative path
					if rel, relErr := filepath.Rel(t.repoDir, resolvedTarget); relErr == nil {
						targetPath = filepath.ToSlash(rel)
					}
				}
				allowed := false
				for _, prefix := range t.scopePrefixes {
					if strings.HasPrefix(targetPath, prefix) {
						allowed = true
						break
					}
				}
				if !allowed {
					return "", fmt.Errorf("bash command writes to %s which is outside your allowed paths: %v. Use write_file instead, or stay within your assigned package.", targetPath, t.scopePrefixes)
				}
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

	slog.Info("shell access",
		"tool", "bash",
		"cmd", params.Command,
		"workdir", t.repoDir,
	)

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

// resolveAndCheckPath resolves symlinks and verifies the path stays within repoDir.
// If scopePrefixes is non-empty, also verifies the resolved relative path starts
// with one of them. This prevents symlink-based directory escapes where an agent
// creates a symlink inside the repo pointing to a file outside, then reads or writes
// through it.
func resolveAndCheckPath(repoDir, filename string, scopePrefixes []string) (string, error) {
	repoDir = filepath.Clean(repoDir)

	// Clean and join the path
	cleanPath := filepath.Clean(filename)
	absPath := filepath.Join(repoDir, cleanPath)

	// Resolve symlinks in the path. filepath.EvalSymlinks resolves ALL symlinks.
	// If the file doesn't exist yet (write_file creating a new file), we resolve
	// as much of the path as exists, then append the remaining components.
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolving path: %w", err)
		}
		// File doesn't exist yet — resolve the longest existing prefix of the path.
		// Walk from the deepest existing ancestor up to repoDir.
		dir := filepath.Dir(absPath)
		base := filepath.Base(absPath)
		var extraComponents []string
		for {
			rp, rpErr := filepath.EvalSymlinks(dir)
			if rpErr == nil {
				// Found an existing ancestor — resolve it and append remaining components.
				resolved = rp
				for _, c := range extraComponents {
					resolved = filepath.Join(resolved, c)
				}
				resolved = filepath.Join(resolved, base)
				break
			}
			if !os.IsNotExist(rpErr) {
				return "", fmt.Errorf("resolving path: %w", rpErr)
			}
			// This directory component doesn't exist either — remember it and try parent.
			extraComponents = append([]string{filepath.Base(dir)}, extraComponents...)
			parentDir := filepath.Dir(dir)
			if parentDir == dir || parentDir == "/" || parentDir == "." {
				// Reached root without finding an existing dir — can't resolve.
				// Use the cleaned absPath as-is; the containment check below still applies.
				resolved = absPath
				break
			}
			dir = parentDir
		}
	}

	// Verify the resolved path is within repoDir
	repoBase := repoDir + string(os.PathSeparator)
	if resolved != repoDir && !strings.HasPrefix(resolved, repoBase) {
		return "", fmt.Errorf("path escapes repository root via symlink: %s -> %s", filename, resolved)
	}

	// Defense-in-depth: also check with filepath.Rel
	if rel, err := filepath.Rel(repoDir, resolved); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes repository root via symlink: %s -> %s", filename, resolved)
	}

	// Check scope prefixes against the RESOLVED path (relative to repoDir)
	if len(scopePrefixes) > 0 {
		relPath, _ := filepath.Rel(repoDir, resolved)
		relPath = filepath.ToSlash(relPath)
		allowed := false
		for _, prefix := range scopePrefixes {
			if strings.HasPrefix(relPath, prefix) || relPath == strings.TrimSuffix(prefix, "/") {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("path prefix not allowed: %s (allowed: %v)", filename, scopePrefixes)
		}
	}

	return resolved, nil
}

// isReadFileOutput detects when the model has copied read_file output format
// into write_file content. read_file prefixes each line with "    N\t" where
// N is the line number. This pattern is distinctive: 5+ spaces, digits, tab.
var lineNumPrefix = regexp.MustCompile(`(?m)^ +\d+\t`)

func isReadFileOutput(content string) bool {
	// Match at least 3 lines with the line-number prefix
	matches := lineNumPrefix.FindAllString(content, -1)
	return len(matches) >= 2
}

// stripLineNumbers removes the "    N\t" line-number prefix from read_file output.
// This converts "     1\tline1\n     2\tline2" to "line1\nline2".
func stripLineNumbers(content string) string {
	return lineNumPrefix.ReplaceAllString(content, "")
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

	// Pre-clean the path to normalize ../ and redundant separators before joining.
	cleanPath := filepath.Clean(path)

	// Sanitize: if model passed an absolute path containing repoDir, extract the relative part.
	if filepath.IsAbs(cleanPath) && strings.HasPrefix(cleanPath, t.repoDir) {
		rel, err := filepath.Rel(t.repoDir, cleanPath)
		if err == nil {
			cleanPath = rel
		}
	}

	// Resolve symlinks and verify path containment.
	// This subsumes the old filepath.Clean + filepath.Join + prefix checks.
	resolvedPath, rerr := resolveAndCheckPath(t.repoDir, cleanPath, nil) // no scope check for reads
	if rerr != nil {
		return "", rerr
	}
	absPath := resolvedPath

	relForLog := path
	if rel, err := filepath.Rel(t.repoDir, absPath); err == nil {
		relForLog = rel
	}

	slog.Info("file access",
		"tool", "read_file",
		"path", relForLog,
		"abs_path", absPath,
		"repo_root", t.repoDir,
	)

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
	repoDir         string
	commitPrefix    string
	dryRun          bool
	allowedPrefixes []string // if non-empty, paths must start with one of these prefixes relative to repo root
	ralphGuard      ralphGuardChecker // if set, block writes to spec paths during ralph
	isRalphSession  bool              // true when this tool is in a ralph session
	isReviewerSync  bool              // true when reviewer is syncing spec TODOs with ralph-completed label
}

func NewWriteFileTool(info SessionInfo, cfg AgentConfig) *writeFileTool {
	return &writeFileTool{
		repoDir:      info.RepoDir(),
		commitPrefix: cfg.CommitPrefix(),
		dryRun:       cfg.DryRun(),
	}
}

// SetAllowedPrefixes restricts write_file to paths starting with the given prefixes.
// Each prefix is relative to the repo root (e.g., "openspec/changes/").
func (t *writeFileTool) SetAllowedPrefixes(prefixes []string) {
	t.allowedPrefixes = prefixes
}

// SetRalphGuard enables spec immutability enforcement during ralph sessions.
// When set, writes to spec paths (openspec/**/spec.md) are blocked.
func (t *writeFileTool) SetRalphGuard(guard ralphGuardChecker, isRalph, isReviewerSync bool) {
	t.ralphGuard = guard
	t.isRalphSession = isRalph
	t.isReviewerSync = isReviewerSync
}

func (t *writeFileTool) Name() string { return "write_file" }

func (t *writeFileTool) Description() string {
	return "REPLACE a file with new content. The ENTIRE file must be included — any content not in this call will be DELETED. Always include all existing unchanged lines plus your changes. Creates parent directories if needed."
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

	// Strip line numbers from content if the model copied read_file output format.
	// read_file returns lines like "     1\tcontent"; the model sometimes copies
	// this format into write_file instead of writing raw content.
	if isReadFileOutput(params.Content) {
		params.Content = stripLineNumbers(params.Content)
	}

	if containsNullByte(params.Path) {
		return "", fmt.Errorf("path contains null bytes: %q", params.Path)
	}

	if t.dryRun {
		return fmt.Sprintf("[dry-run] Would write %d bytes to %s", len(params.Content), params.Path), nil
	}

	// Ralph spec immutability: block writes to spec paths during ralph sessions.
	// Exception: reviewer sync (ralph-completed label) is allowed to check spec TODOs.
	if t.ralphGuard != nil && t.isRalphSession && !t.isReviewerSync {
		if t.ralphGuard.IsSpecPath(params.Path) {
			return "", fmt.Errorf("spec files are immutable during ralph mode: %s", params.Path)
		}
	}

	// Resolve symlinks and verify path containment + scope restrictions.
	// This subsumes the old filepath.Clean + filepath.Join + prefix checks.
	resolvedPath, err := resolveAndCheckPath(t.repoDir, params.Path, t.allowedPrefixes)
	if err != nil {
		return "", err
	}
	absPath := resolvedPath

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return "", fmt.Errorf("create directories: %w", err)
	}

	relForLog := params.Path
	if rel, err := filepath.Rel(t.repoDir, absPath); err == nil {
		relForLog = rel
	}

	slog.Info("file access",
		"tool", "write_file",
		"path", relForLog,
		"abs_path", absPath,
		"repo_root", t.repoDir,
		"bytes", len(params.Content),
	)

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
	gitUser     string // Forgejo username for remote URL credentials
	ralphGuard  ralphCommitChecker // if set, validate spec immutability during ralph
	isRalphSession bool             // true when in a ralph session
}

func NewGitTool(info SessionInfo, cfg AgentConfig) *gitTool {
	return &gitTool{
		repoDir:    info.RepoDir(),
		agentCfg:   cfg,
		sandboxCfg: sandbox.DefaultConfig(info.RepoDir()),
		gitUser:    cfg.GitUser(),
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

// ralphCommitChecker is implemented by ralph.Guard to validate commit diffs.
type ralphCommitChecker interface {
	ValidateCommitDiff(diff string) error
}

// SetRalphGuard enables spec immutability enforcement for git commits during ralph.
func (t *gitTool) SetRalphGuard(guard ralphCommitChecker, isRalph bool) {
	t.ralphGuard = guard
	t.isRalphSession = isRalph
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

	// Tolerate the common LLM mistake of passing the git subcommand as the
	// JSON key instead of under "command" (e.g. {"log": "--oneline -10"} or
	// {"status": ""}). If "command" is empty, scan for any other string-valued
	// field and treat key+value as the command. This prevents infinite loops
	// where the agent repeatedly gets "empty command" and never recovers.
	cmdStr := params.Command
	if strings.TrimSpace(cmdStr) == "" {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(args, &raw); err == nil {
			for k, v := range raw {
				if k == "command" {
					continue
				}
				var sv string
				if json.Unmarshal(v, &sv) == nil {
					sub := strings.TrimSpace(sv)
					if sub == "" {
						cmdStr = k // e.g. {"status": ""} -> "status"
					} else {
						cmdStr = k + " " + sub // e.g. {"log": "--oneline -10"} -> "log --oneline -10"
					}
					break
				}
			}
		}
	}

	// Security: block all push commands — agent must use forgejo_create_pr tool
	if strings.HasPrefix(strings.TrimSpace(strings.ToLower(cmdStr)), "push") ||
		strings.HasPrefix(strings.TrimSpace(strings.ToLower(cmdStr)), "git push") {
		return "", fmt.Errorf("git push is not allowed. Use the forgejo_create_pr tool to submit changes via pull request")
	}

	cmdLower := strings.TrimSpace(strings.ToLower(cmdStr))
	isCommit := strings.HasPrefix(cmdLower, "commit") || strings.HasPrefix(cmdLower, "git commit")

	// Pre-commit check: if this is a commit command and we're on a protected branch,
	// block it BEFORE the commit happens. The post-commit check below is a safety net
	// but the commit itself must be prevented to avoid polluting main.
	if isCommit && t.agentCfg != nil && !t.agentCfg.AllowProtectedPush() {
		branchCmd := exec.CommandContext(ctx, "git", "-C", t.repoDir, "rev-parse", "--abbrev-ref", "HEAD")
		branchCmd.Dir = t.repoDir
		branchOut, _ := branchCmd.CombinedOutput()
		currentBranch := strings.TrimSpace(string(branchOut))
		for _, pb := range t.agentCfg.ProtectedBranches() {
			if currentBranch == pb {
				return "", fmt.Errorf("commit on protected branch %q blocked. Create a feature branch first (e.g., git checkout -b feature/my-feature). Only scaffold sessions may commit on main.", currentBranch)
			}
		}
	}

	// Ralph guard: check staged diff for spec modifications before committing.
	// During ralph sessions, commits touching spec files are blocked.
	if isCommit && t.ralphGuard != nil && t.isRalphSession {
		diffCmd := exec.CommandContext(ctx, "git", "-C", t.repoDir, "diff", "--cached", "--name-only")
		diffCmd.Dir = t.repoDir
		diffOut, _ := diffCmd.CombinedOutput()
		if err := t.ralphGuard.ValidateCommitDiff(string(diffOut)); err != nil {
			return "", err
		}
	}

	// Sanitize: replace newlines in commit messages with spaces to avoid shell
	// treating them as argument separators
	if isCommit {
		cmdStr = strings.ReplaceAll(cmdStr, "\\n", " ")
		cmdStr = strings.ReplaceAll(cmdStr, "\n", " ")
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var parts []string
	if isCommit {
		parts = parseGitCommit(cmdStr)
	} else {
		// Compound commands (&&, ||, ;) require a shell which we avoid for security.
	// Instead, tell the agent to run them as separate git commands.
	if strings.Contains(cmdStr, "&&") || strings.Contains(cmdStr, "||") || strings.Contains(cmdStr, ";") {
		return "", fmt.Errorf("compound git commands with &&, ||, or ; are not supported. Please run each git command separately. For example, instead of 'add . && commit -m \"msg\"', run 'add .', then 'commit -m \"msg\"' as two separate calls.")
	}

	parts = strings.Fields(cmdStr)
	}
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

		slog.Info("shell access",
			"tool", "git",
			"cmd", cmdStr,
			"workdir", t.repoDir,
		)
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
			// Detect and fix token-only remote URLs (e.g., https://TOKEN@host).
			// Git needs USER:TOKEN@host format; token-only causes "could not read Password".
			if t.gitUser != "" {
				remoteCmd := exec.CommandContext(ctx, "git", "-C", t.repoDir, "remote", "get-url", "origin")
				remoteCmd.Dir = t.repoDir
				remoteOut, _ := remoteCmd.CombinedOutput()
				remoteURL := strings.TrimSpace(string(remoteOut))
				if strings.Contains(remoteURL, "@") {
					// Check if the part between :// and @ has no : (missing username).
					// Token-only format like https://TOKEN@host needs USER: prefix.
					schemeEnd := strings.Index(remoteURL, "://")
					atPos := strings.Index(remoteURL, "@")
					needsUser := schemeEnd < 0 || (atPos > schemeEnd && !strings.Contains(remoteURL[schemeEnd+3:atPos], ":"))
					if needsUser {
						fixedURL := remoteURL
						if schemeEnd >= 0 {
							fixedURL = remoteURL[:schemeEnd+3] + t.gitUser + ":" + remoteURL[schemeEnd+3:]
						} else {
							fixedURL = t.gitUser + ":" + remoteURL
						}
						setCmd := exec.CommandContext(ctx, "git", "-C", t.repoDir, "remote", "set-url", "origin", fixedURL)
						setCmd.Dir = t.repoDir
						if setOut, setErr := setCmd.CombinedOutput(); setErr != nil {
							slog.Warn("git auto-push: failed to fix remote URL", "error", setErr, "output", string(setOut))
						} else {
							slog.Info("git auto-push: fixed remote URL credentials", "remote", fixedURL)
						}
					}
				}
			}

			pushCtx, pushCancel := context.WithTimeout(ctx, 30*time.Second)
			defer pushCancel()
			pushCmd := exec.CommandContext(pushCtx, "git", "push", "-u", "origin", "HEAD")
			pushCmd.Dir = t.repoDir
			pushOut, pushErr := pushCmd.CombinedOutput()
			if pushErr != nil {
				return fmt.Sprintf("%s\n[auto-push warning] %s\n%s", string(out), pushErr, string(pushOut)), nil
			}
			pushSummary := []byte(fmt.Sprintf("\n[auto-push] %s", strings.TrimSpace(string(pushOut))))
			out = append(out, pushSummary...)
		}
	}

	return string(out), nil
}

// parseGitCommit parses a git commit command string into proper args.
// It handles -m "message with spaces" correctly by keeping the message as a single arg.
func parseGitCommit(cmdStr string) []string {
	// Strip leading "git " if present
	cmdStr = strings.TrimSpace(cmdStr)
	if strings.HasPrefix(strings.ToLower(cmdStr), "git ") {
		cmdStr = cmdStr[4:]
	}
	if strings.HasPrefix(strings.ToLower(cmdStr), "commit ") {
		cmdStr = cmdStr[7:]
	}

	// Find -m flag and extract the message as a single string
	var args []string
	args = append(args, "commit")

	for len(cmdStr) > 0 {
		cmdStr = strings.TrimSpace(cmdStr)
		if len(cmdStr) == 0 {
			break
		}
		if strings.HasPrefix(cmdStr, "-m") {
			// Everything after -m is the commit message (already newline-sanitized to spaces)
			msg := strings.TrimPrefix(cmdStr, "-m")
			msg = strings.TrimSpace(msg)
			// Strip surrounding quotes if present
			if (strings.HasPrefix(msg, "\"") && strings.HasSuffix(msg, "\"")) ||
				(strings.HasPrefix(msg, "'") && strings.HasSuffix(msg, "'")) {
				msg = msg[1 : len(msg)-1]
			}
			if msg != "" {
				args = append(args, "-m", msg)
			}
			break
		}
		// Other flags
		if cmdStr[0] == '-' {
			spaceIdx := strings.IndexByte(cmdStr, ' ')
			if spaceIdx < 0 {
				args = append(args, cmdStr)
				break
			}
			args = append(args, cmdStr[:spaceIdx])
			cmdStr = cmdStr[spaceIdx:]
		} else {
			break
		}
	}

	return args
}
