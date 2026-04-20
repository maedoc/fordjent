package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/fordjent/fordjent/internal/config"
)

// bashTool executes shell commands in the session's working directory.
type bashTool struct {
	workDir string
}

func NewBashTool(info SessionInfo) *bashTool {
	return &bashTool{workDir: info.WorkDir()}
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

	cmd := exec.CommandContext(ctx, "bash", "-c", params.Command)
	cmd.Dir = t.workDir

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

	var cmdStr string
	if params.Offset > 0 {
		cmdStr = fmt.Sprintf("cat -n '%s' | tail -n +%d | head -n %d", params.Path, params.Offset, params.Limit)
	} else {
		cmdStr = fmt.Sprintf("cat -n '%s' | head -n %d", params.Path, params.Limit)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", cmdStr)
	cmd.Dir = t.repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read file: %s", string(out))
	}
	return string(out), nil
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

	// Use a temp file to avoid heredoc escaping issues
	tmpScript := fmt.Sprintf(`mkdir -p "$(dirname '%s')" && cat > '%s' << 'FORDJENT_EOF%d'
%s
FORDJENT_EOF%d`, params.Path, params.Path, time.Now().UnixNano(), params.Content, time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", tmpScript)
	cmd.Dir = t.repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("write file: %s: %s", err, string(out))
	}

	return fmt.Sprintf("Written %d bytes to %s", len(params.Content), params.Path), nil
}

// gitTool handles git operations in the session.
type gitTool struct {
	repoDir           string
	protectedBranches []string
}

func NewGitTool(info SessionInfo, cfg *config.Config) *gitTool {
	return &gitTool{
		repoDir:           info.RepoDir(),
		protectedBranches: cfg.Security.ProtectedBranches,
	}
}

func (t *gitTool) Name() string { return "git" }

func (t *gitTool) Description() string {
	return "Execute git operations in the repository: status, diff, add, commit, push, branch, checkout, log, fetch, pull."
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

	// Security: block direct push to protected branches
	for _, branch := range t.protectedBranches {
		lowerCmd := strings.ToLower(params.Command)
		if strings.Contains(lowerCmd, "push") &&
			strings.Contains(lowerCmd, strings.ToLower(branch)) &&
			!strings.Contains(lowerCmd, "head:") {
			return "", fmt.Errorf("direct push to protected branch '%s' is not allowed. "+
				"Create a feature branch and submit a PR instead", branch)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	parts := strings.Fields(params.Command)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, "git", parts...)
	cmd.Dir = t.repoDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("git error: %s\n%s", err, string(out)), nil
	}

	return string(out), nil
}
