package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"

	"github.com/fordjent/fordjent/internal/ralph"
)

// ralphUpdateTool implements the 4-A protocol stage recording tool.
type ralphUpdateTool struct {
	tracker *ralph.Tracker
	guard   *ralph.Guard
	repoDir string
	prNum   int
	iterNum int
}

// NewRalphUpdateTool creates a ralph_update tool for the given tracker.
// The tracker is session-scoped (one per ralph iteration).
func NewRalphUpdateTool(tracker *ralph.Tracker, guard *ralph.Guard, repoDir string, prNum, iterNum int) *ralphUpdateTool {
	return &ralphUpdateTool{
		tracker: tracker,
		guard:   guard,
		repoDir: repoDir,
		prNum:   prNum,
		iterNum: iterNum,
	}
}

func (t *ralphUpdateTool) Name() string { return "ralph_update" }

func (t *ralphUpdateTool) Description() string {
	return `Record your progress through the 4-A ralph protocol. Call this tool as you complete each stage. Stages MUST be called in order: awareness → act → assert → append. 
- awareness: You have read and understood the context (git log, PR comments, spec files). 
- act: You have written/modified code. 
- assert: You have verified your changes (build, test). 
- append: You are ready to commit. This triggers auto-commit and push.`
}

func (t *ralphUpdateTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"stage": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"awareness", "act", "assert", "append"},
				"description": "Current protocol stage (must be called in order: awareness → act → assert → append)",
			},
			"message": map[string]interface{}{
				"type":        "string",
				"description": "Brief summary of what was done in this stage",
			},
		},
		"required": []string{"stage", "message"},
	}
}

func (t *ralphUpdateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Stage   string `json:"stage"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if err := t.tracker.RecordStage(params.Stage, params.Message); err != nil {
		return "", err
	}

	completed := t.tracker.CompletedStages()
	stageNames := make([]string, 0, len(completed))
	for _, s := range []string{"awareness", "act", "assert", "append"} {
		if completed[s] {
			stageNames = append(stageNames, s)
		}
	}

	slog.Info("ralph_update: stage recorded",
		"stage", params.Stage,
		"iteration", t.iterNum,
		"pr", t.prNum,
		"completed", strings.Join(stageNames, ","),
	)

	resp := fmt.Sprintf("Stage '%s' recorded. Completed stages: [%s]", params.Stage, strings.Join(stageNames, ", "))

	// If this is the append stage, trigger auto-commit
	if params.Stage == "append" && t.tracker.IsComplete() {
		resp += "\n\nAll 4 stages complete. Auto-commit will follow."

		// Check for uncommitted changes before attempting commit
		statusCmd := exec.Command("git", "-C", t.repoDir, "status", "--porcelain")
		statusOut, _ := statusCmd.CombinedOutput()
		if len(strings.TrimSpace(string(statusOut))) == 0 {
			resp += "\n[Auto-commit: working tree clean — nothing to commit]"
		} else {
			// Auto-commit changes to the PR branch
			addCmd := exec.Command("git", "-C", t.repoDir, "add", "-A")
			if out, err := addCmd.CombinedOutput(); err != nil {
				slog.Warn("ralph auto-commit: git add failed", "error", err, "output", string(out), "repoDir", t.repoDir)
				resp += "\n[Auto-commit: git add failed — please commit manually using git tool]"
			} else {
				commitMsg := t.buildCommitMessage()
				commitCmd := exec.Command("git", "-C", t.repoDir, "commit", "-m", commitMsg)
				if out, err := commitCmd.CombinedOutput(); err != nil {
					slog.Warn("ralph auto-commit: git commit failed", "error", err, "output", string(out))
					resp += "\n[Auto-commit: git commit failed — please retry using git tool]"
				} else {
					pushCmd := exec.Command("git", "-C", t.repoDir, "push", "-u", "origin", "HEAD")
					if out, err := pushCmd.CombinedOutput(); err != nil {
						slog.Warn("ralph auto-commit: git push failed", "error", err, "output", string(out))
						resp += "\n[Auto-commit: committed but push failed — please push using git tool]"
					} else {
						resp += "\n[Auto-commit: committed and pushed successfully]"
					}
				}
			}
		}

		// Signal session completion so the agent loop can exit early
		resp += "\n\n***RALPH ITERATION COMPLETE*** The fix has been committed and pushed. No further action needed."
	}

	return resp, nil
}

// buildCommitMessage constructs a git commit message that includes test results
// from the assert stage, enabling future iterations to learn from prior outcomes.
func (t *ralphUpdateTool) buildCommitMessage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ralph: iteration %d changes", t.iterNum)

	stageMsgs := t.tracker.StageMessages()

	// Include awareness summary (what was understood)
	if msg, ok := stageMsgs["awareness"]; ok && msg != "" {
		b.WriteString("\n\n## Awareness\n")
		b.WriteString(msg)
	}

	// Include act summary (what was changed)
	if msg, ok := stageMsgs["act"]; ok && msg != "" {
		b.WriteString("\n\n## Act\n")
		b.WriteString(msg)
	}

	// Include assert summary (test results — this is the learning signal)
	if msg, ok := stageMsgs["assert"]; ok && msg != "" {
		b.WriteString("\n\n## Assert (Test Results)\n")
		b.WriteString(msg)
	} else {
		b.WriteString("\n\n## Assert\nNo tests were run in this iteration.")
	}

	b.WriteString("\n")
	return b.String()
}

// ralphProgressTool writes progress files for ralph iterations.
type ralphProgressTool struct {
	guard   *ralph.Guard
	repoDir string
	prNum   int
	iterNum int
	tracker *ralph.Tracker
}

// NewRalphProgressTool creates a ralph_progress tool.
func NewRalphProgressTool(guard *ralph.Guard, tracker *ralph.Tracker, repoDir string, prNum, iterNum int) *ralphProgressTool {
	return &ralphProgressTool{
		guard:   guard,
		repoDir: repoDir,
		prNum:   prNum,
		iterNum: iterNum,
		tracker: tracker,
	}
}

func (t *ralphProgressTool) Name() string { return "ralph_progress" }

func (t *ralphProgressTool) Description() string {
	return "Write a progress file for this ralph iteration. Automatically writes to .ralph/progress/pr-{N}-iteration-{M}.md. Only available during ralph sessions."
}

func (t *ralphProgressTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"message": map[string]interface{}{
				"type":        "string",
				"description": "Summary of progress for this iteration",
			},
		},
		"required": []string{"message"},
	}
}

func (t *ralphProgressTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	stageMsgs := t.tracker.StageMessages()
	if params.Message != "" {
		stageMsgs["_summary"] = params.Message
	}

	path, err := ralph.WriteProgress(t.repoDir, t.prNum, t.iterNum, stageMsgs)
	if err != nil {
		return "", fmt.Errorf("write progress: %w", err)
	}

	return fmt.Sprintf("Progress written to %s", path), nil
}

// IsRalphSession checks if a session key indicates a ralph session.
// Ralph session keys match the pattern: repo/pulls/N-ralph-iM
func IsRalphSession(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) < 3 {
		return false
	}
	last := parts[len(parts)-1]
	return strings.Contains(last, "-ralph-i")
}

// ParseRalphSessionKey extracts the PR number and iteration number
// from a ralph session key like "fjadmin/testbed/pulls/42-ralph-i7".
func ParseRalphSessionKey(key string) (repo string, prNum int, iterNum int, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 4 {
		return "", 0, 0, false
	}

	// repo is parts[0]/parts[1]
	repo = parts[0] + "/" + parts[1]

	// Last segment: "42-ralph-i7"
	last := parts[len(parts)-1]
	ralphIdx := strings.Index(last, "-ralph-i")
	if ralphIdx < 0 {
		return "", 0, 0, false
	}

	prStr := last[:ralphIdx]
	iterStr := last[ralphIdx+len("-ralph-i"):]

	prNum, err := strconv.Atoi(prStr)
	if err != nil {
		return "", 0, 0, false
	}
	iterNum, err = strconv.Atoi(iterStr)
	if err != nil {
		return "", 0, 0, false
	}

	return repo, prNum, iterNum, true
}
