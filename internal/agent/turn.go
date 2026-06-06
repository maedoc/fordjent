package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/tool"
)

// ErrMaxTurnsReached is returned when the agent exhausts its turn budget.
var ErrMaxTurnsReached = errors.New("max turns reached")

// TurnResult captures the outcome of a single LLM turn.
type TurnResult struct {
	Turn          int
	Response      *provider.Response
	Usage         *provider.Usage
	CostUSD       float64
	Latency       time.Duration
	RetryCount    int
	ToolCalls     int
	Compacted     bool
	RequestCount  int
}

// TurnExecutor runs the LLM loop for a session with compaction, retries, and cost tracking.
type TurnExecutor struct {
	cfg            *config.Config
	llm            provider.ChatCompleter
	tools          *tool.Registry
	tracker        *ContextTracker
	costTracker    *cost.Tracker
	sessionKey     string
	repository     string
	requestCount   int
	excludeTools   map[string]bool // tools to exclude from LLM schema
	turnCount      int             // current turn number
	maxTurns       int             // max turn budget
	role           string          // agent role (implementer, reviewer, etc.)
	toolCallCounts map[string]int  // per-tool call counts
	turnSteered    map[int]bool    // tracks which steering thresholds have fired
	lastToolOutput map[string]string // last output per tool (for duplicate detection)
}

func NewTurnExecutor(
	cfg *config.Config,
	llm provider.ChatCompleter,
	tools *tool.Registry,
	costTracker *cost.Tracker,
	sessionKey, repository string,
	maxTurns int,
	role string,
) *TurnExecutor {
	tracker := NewContextTracker(
		cfg.Agent.ContextWindow,
		cfg.Agent.CompactionThreshold,
		cfg.Agent.CompactionKeepTurns,
	)
	return &TurnExecutor{
		cfg:            cfg,
		llm:            llm,
		tools:          tools,
		tracker:        tracker,
		costTracker:    costTracker,
		sessionKey:     sessionKey,
		repository:     repository,
		excludeTools:   make(map[string]bool),
		maxTurns:       maxTurns,
		role:           role,
		toolCallCounts: make(map[string]int),
		turnSteered:    make(map[int]bool),
		lastToolOutput: make(map[string]string),
	}
}

// SetExcludeTools sets which tools should be excluded from the LLM schema.
func (te *TurnExecutor) SetExcludeTools(names map[string]bool) {
	te.excludeTools = names
}

// RecordToolCall increments the call count for a tool and tracks output for duplicate detection.
func (te *TurnExecutor) RecordToolCall(name string, output string) {
	te.toolCallCounts[name]++
	// Track last output (truncated) for duplicate detection on bash/git
	if name == "bash" || name == "git" {
		if len(output) > 200 {
			output = output[:200]
		}
		te.lastToolOutput[name] = output
	}
}

// CurrentTurn returns the current turn number (1-based).
func (te *TurnExecutor) CurrentTurn() int {
	return te.turnCount
}

// MaxTurns returns the max turn budget.
func (te *TurnExecutor) MaxTurns() int {
	return te.maxTurns
}

// PerToolRepeatNudge returns a nudge message if a tool has been called too many times.
// Fires once per tool at the repeat threshold.
func (te *TurnExecutor) PerToolRepeatNudge() string {
	if te.role != "implementer" {
		return ""
	}
	thresholds := map[string]int{
		"git":                4,
		"bash":               5,
		"read_file":          3,
		"forgejo_get_issue":  2,
		"forgejo_list_files": 2,
		"forgejo_list_issues": 2,
	}
	for toolName, threshold := range thresholds {
		count := te.toolCallCounts[toolName]
		if count >= threshold {
			key := -100 - int([]byte(toolName)[0]) // unique key per tool
			if !te.turnSteered[key] {
				te.turnSteered[key] = true
				switch toolName {
				case "git", "bash":
					return fmt.Sprintf("You've called %s %d times. Stop exploring -- use write_file to write your code now.", toolName, count)
				case "read_file", "forgejo_list_files":
					return fmt.Sprintf("You've read files %d times. You have enough information -- use write_file to write your code now.", count)
				case "forgejo_get_issue", "forgejo_list_issues":
					return fmt.Sprintf("You've checked the issue %d times. Stop re-reading and use write_file to write your code.", count)
				}
			}
		}
	}
	return ""
}

// DuplicateOutputNudge returns a nudge if the last tool output matches the previous one.
func (te *TurnExecutor) DuplicateOutputNudge(toolName, output string) string {
	if toolName != "bash" && toolName != "git" {
		return ""
	}
	if len(output) > 200 {
		output = output[:200]
	}
	if last, ok := te.lastToolOutput[toolName]; ok && last == output {
		key := -200 - int([]byte(toolName)[0])
		if !te.turnSteered[key] {
			te.turnSteered[key] = true
			return fmt.Sprintf("Same output as previous %s call. You already have this information -- proceed to write_file.", toolName)
		}
	}
	return ""
}

// HardGateWriteEnforce removes exploration tools if the implementer hasn't written files by turn 15.
func (te *TurnExecutor) HardGateWriteEnforce() bool {
	if te.role != "implementer" {
		return false
	}
	if te.turnCount < 15 {
		return false
	}
	writes := te.toolCallCounts["write_file"]
	prCreates := te.toolCallCounts["forgejo_create_pr"]
	if writes > 0 || prCreates > 0 {
		return false
	}
	if te.turnSteered[-300] {
		return false
	}
	te.turnSteered[-300] = true

	// Gate: remove read-only exploration tools, keep write_file + bash + git + forgejo_create_pr
	gated := map[string]bool{
		"read_file":             true,
		"forgejo_get_issue":    true,
		"forgejo_list_files":   true,
		"forgejo_list_issues":  true,
		"forgejo_pr_files":     true,
		"forgejo_list_branches": true,
		"forgejo_comment":      true,
		"openspec_get_tasks":   true,
		"openspec_read_spec":   true,
		"openspec_read_change": true,
		"openspec_mark_task":   true,
	}
	te.excludeTools = gated
	slog.Warn("hard gate: removed exploration tools, keeping write_file + bash + git + forgejo_create_pr",
		"session_key", te.sessionKey, "turn", te.turnCount)
	return true
}

// ApplySteering injects steering messages based on turn budget usage, per-tool repeat nudges,
// duplicate output detection, and hard gating.
func (te *TurnExecutor) ApplySteering(messages []provider.Message, lastToolName, lastToolOutput string) []provider.Message {
	current := te.turnCount
	max := te.maxTurns
	pct := float64(current) / float64(max) * 100

	// 1. Per-tool repeat nudge (concrete, tied to model's own actions)
	if nudge := te.PerToolRepeatNudge(); nudge != "" {
		messages = append(messages, provider.Message{
			Role:    "user",
			Content: "[Fordjent Steering] " + nudge,
		})
	}

	// 2. Duplicate output nudge (same bash/git result twice)
	if nudge := te.DuplicateOutputNudge(lastToolName, lastToolOutput); nudge != "" {
		messages = append(messages, provider.Message{
			Role:    "user",
			Content: "[Fordjent Steering] " + nudge,
		})
	}

	// 3. Hard gate: remove exploration tools if implementer hasn't written by turn 15
	if te.HardGateWriteEnforce() {
		messages = append(messages, provider.Message{
			Role:    "user",
			Content: "[Fordjent Steering] You've reached turn 15 without writing any code. Exploration tools have been removed. Use write_file to write your implementation now, then bash to test it.",
		})
	}

	// 4. Turn budget steering thresholds (only fire once each)
	thresholds := map[int]string{
		40: fmt.Sprintf("[Turn %d/%d] You've used 40%% of your turn budget. If you haven't started implementing yet, consider doing so now.", current, max),
		60: fmt.Sprintf("[Turn %d/%d] 60%% of turns used. Prioritize completing your current task. Avoid further exploration or re-reading files you've already seen.", current, max),
		80: fmt.Sprintf("[Turn %d/%d] 80%% used. You MUST commit your work and create a PR within the next few turns. Stop exploring. If you have code, commit and create a PR now.", current, max),
		90: fmt.Sprintf("[Turn %d/%d] Only %d turns remain. If you have code to submit, commit and create a PR IMMEDIATELY. If you're stuck, post a comment explaining what blocked you using forgejo_comment.", current, max, max-current),
	}

	for threshold, msg := range thresholds {
		if pct >= float64(threshold) && !te.turnSteered[threshold] {
			te.turnSteered[threshold] = true
			messages = append(messages, provider.Message{
				Role:    "user",
				Content: "[Fordjent Steering] " + msg,
			})
		}
	}

	return messages
}

// Run executes one LLM turn: handles compaction before the call, records cost after.
func (te *TurnExecutor) Run(ctx context.Context, systemPrompt string, messages []provider.Message) (*TurnResult, []provider.Message, error) {
	te.turnCount++
	start := time.Now()

	// Check budget before spending
	if te.costTracker != nil {
		allowed, reason := te.costTracker.CheckBudget(
			te.sessionKey,
			te.cfg.Budget.Enabled,
			te.cfg.Budget.MaxSessionCost,
			te.cfg.Budget.MaxMonthlyCost,
		)
		if !allowed {
			return nil, messages, fmt.Errorf("budget exceeded: %s", reason)
		}
	}

	// Compact context if needed
	compacted := false
	if te.tracker.ShouldCompact(messages) {
		messages = te.tracker.Compact(messages)
		compacted = true
	}

	// Call LLM (retry is handled inside Client.Chat)
	toolDefs := te.tools.ToolsExcluding(te.excludeTools)
	response, usage, err := te.llm.Chat(ctx, systemPrompt, messages, toolDefs)
	latency := time.Since(start)

	if err != nil {
		return nil, messages, fmt.Errorf("LLM chat failed: %w", err)
	}

	te.tracker.Update(usage)
	te.requestCount++

	var costUSD float64
	if usage != nil {
		costUSD = usage.Cost(te.llm.Cfg())
		if te.costTracker != nil {
			_ = te.costTracker.Record(&cost.UsageRecord{
				SessionKey:   te.sessionKey,
				ProviderName: te.llm.Cfg().Name,
				Model:        te.llm.Cfg().Model,
				Repository:   te.repository,
				InputTokens:  int64(usage.PromptTokens),
				OutputTokens: int64(usage.CompletionTokens),
				TotalTokens:  int64(usage.TotalTokens),
				CostUSD:      costUSD,
				Timestamp:    start,
			})
		}
	}

	toolCount := len(response.ToolCalls)

	slog.Info("turn complete",
		"session_key", te.sessionKey,
		"turn", te.turnCount,
		"latency_ms", latency.Milliseconds(),
		"tokens_in", usage.PromptTokens,
		"tokens_out", usage.CompletionTokens,
		"total_tokens", usage.TotalTokens,
		"cost_usd", costUSD,
		"tool_calls", toolCount,
		"tools_used", te.toolCallCounts,
		"compacted", compacted,
		"utilization", te.tracker.Utilization(messages),
		"request_count", te.requestCount,
	)

	result := &TurnResult{
		Turn:         te.turnCount,
		Response:     response,
		Usage:        usage,
		CostUSD:      costUSD,
		Latency:      latency,
		RetryCount:   0,
		ToolCalls:    toolCount,
		Compacted:    compacted,
		RequestCount: te.requestCount,
	}

	return result, messages, nil
}
