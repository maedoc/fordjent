package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
)

// Memory implements the git-log-as-memory pattern from the PLAN.md.
// It writes agent reasoning and actions to git notes and supports
// querying the git log for previous context.
type Memory struct {
	cfg     *config.Config
	workDir string
	forgejo *forgejo.Client
}

func New(cfg *config.Config, workDir string, forgejoClient *forgejo.Client) *Memory {
	return &Memory{
		cfg:     cfg,
		workDir: workDir,
		forgejo: forgejoClient,
	}
}

// Record writes a reasoning trace to the git log via git notes.
func (m *Memory) Record(ctx context.Context, evt *event.Event, response string, turn int) {
	entry := memoryEntry{
		Timestamp:   time.Now().UTC(),
		EventID:     evt.ID,
		EventType:   string(evt.Type),
		SessionKey:  evt.SessionKey,
		Repository:  evt.Repository,
		IssueNumber: evt.IssueNumber,
		Turn:        turn,
		Response:    truncate(response, 4000),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		slog.Error("failed to marshal memory entry", "error", err)
		return
	}

	// Write to local JSONL log
	if err := m.appendJSONL(entry); err != nil {
		slog.Warn("failed to append JSONL", "error", err)
	}

	// Write git notes if repo exists
	repoDir := filepath.Join(m.workDir, "repo")
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		noteMsg := fmt.Sprintf("[%s] %s turn=%d", m.cfg.Agent.CommitPrefix, evt.SessionKey, turn)
		cmd := exec.CommandContext(ctx, "git", "notes", "add", "-m", string(data), "--ref", "fordjent")
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Debug("git notes add failed", "error", err, "output", string(out))
		}
		_ = noteMsg
	}
}

// RecordToolCall writes a tool call record to the memory log.
func (m *Memory) RecordToolCall(ctx context.Context, evt *event.Event, toolName, args, result string) {
	entry := memoryEntry{
		Timestamp:   time.Now().UTC(),
		EventID:     evt.ID,
		EventType:   "tool_call",
		SessionKey:  evt.SessionKey,
		Repository:  evt.Repository,
		IssueNumber: evt.IssueNumber,
		ToolName:    toolName,
		ToolArgs:    truncate(args, 2000),
		ToolResult:  truncate(result, 2000),
	}

	if err := m.appendJSONL(entry); err != nil {
		slog.Warn("failed to append tool call JSONL", "error", err)
	}
}

// Query retrieves previous agent context for a session.
// It checks both the local JSONL log and any compacted summaries.
func (m *Memory) Query(ctx context.Context, evt *event.Event) (string, error) {
	var parts []string

	// Check for compacted summary
	repoDir := filepath.Join(m.workDir, "repo")
	summaryPath := filepath.Join(repoDir, m.cfg.Memory.CompactionPath,
		fmt.Sprintf("%04d-summary.md", evt.IssueNumber))
	if data, err := os.ReadFile(summaryPath); err == nil {
		parts = append(parts, fmt.Sprintf("## Previous Context (compacted)\n%s", string(data)))
	}

	// Query recent JSONL entries
	entries := m.queryJSONL(evt.SessionKey, 10)
	if len(entries) > 0 {
		var sb strings.Builder
		sb.WriteString("## Recent Agent Activity\n")
		for _, e := range entries {
			if e.ToolName != "" {
				sb.WriteString(fmt.Sprintf("- Tool `%s` at %s\n", e.ToolName, e.Timestamp.Format(time.RFC3339)))
			} else {
				sb.WriteString(fmt.Sprintf("- Turn %d at %s: %s\n", e.Turn, e.Timestamp.Format(time.RFC3339),
					truncate(e.Response, 200)))
			}
		}
		parts = append(parts, sb.String())
	}

	// Query git log for agent commits
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "log", "--all",
			"--grep="+evt.SessionKey,
			"--format=%H %s",
			"-n", "20")
		cmd.Dir = repoDir
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			parts = append(parts, fmt.Sprintf("## Git History\n```\n%s\n```", string(out)))
		}
	}

	if len(parts) == 0 {
		return "", nil
	}

	return strings.Join(parts, "\n\n"), nil
}

type memoryEntry struct {
	Timestamp   time.Time `json:"timestamp"`
	EventID     string    `json:"event_id"`
	EventType   string    `json:"event_type"`
	SessionKey  string    `json:"session_key"`
	Repository  string    `json:"repository"`
	IssueNumber int       `json:"issue_number"`
	Turn        int       `json:"turn,omitempty"`
	Response    string    `json:"response,omitempty"`
	ToolName    string    `json:"tool_name,omitempty"`
	ToolArgs    string    `json:"tool_args,omitempty"`
	ToolResult  string    `json:"tool_result,omitempty"`
}

func (m *Memory) jsonlPath() string {
	return filepath.Join(m.workDir, "memory.jsonl")
}

func (m *Memory) appendJSONL(entry memoryEntry) error {
	f, err := os.OpenFile(m.jsonlPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(f, "%s\n", data)
	return err
}

func (m *Memory) queryJSONL(sessionKey string, limit int) []memoryEntry {
	data, err := os.ReadFile(m.jsonlPath())
	if err != nil {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var entries []memoryEntry

	// Read from end (most recent first)
	for i := len(lines) - 1; i >= 0 && len(entries) < limit; i-- {
		if lines[i] == "" {
			continue
		}
		var entry memoryEntry
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			continue
		}
		if entry.SessionKey == sessionKey {
			entries = append(entries, entry)
		}
	}

	return entries
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
