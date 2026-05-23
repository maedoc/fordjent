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
	}
}

// SetExcludeTools sets which tools should be excluded from the LLM schema.
func (te *TurnExecutor) SetExcludeTools(names map[string]bool) {
	te.excludeTools = names
}

// RecordToolCall increments the call count for a tool.
func (te *TurnExecutor) RecordToolCall(name string) {
	te.toolCallCounts[name]++
}

// CurrentTurn returns the current turn number (1-based).
func (te *TurnExecutor) CurrentTurn() int {
	return te.turnCount
}

// MaxTurns returns the max turn budget.
func (te *TurnExecutor) MaxTurns() int {
	return te.maxTurns
}

// ApplySteering injects steering messages based on turn budget usage and inactivity detection.
// Called after each turn completes (tool execution finished, before next LLM call).
func (te *TurnExecutor) ApplySteering(messages []provider.Message) []provider.Message {
	current := te.turnCount
	max := te.maxTurns
	pct := float64(current) / float64(max) * 100

	// Steering thresholds (only fire once each)
	thresholds := map[int]string{
		40: fmt.Sprintf("[Turn %d/%d] You've used 40%% of your turn budget. If you haven't started implementing yet, consider doing so now.", current, max),
		60: fmt.Sprintf("[Turn %d/%d] 60%% of turns used. Prioritize completing your current task. Avoid further exploration or re-reading files you've already seen.", current, max),
		80: fmt.Sprintf("[Turn %d/%d] ⚠️ 80%% used. You MUST commit your work and create a PR within the next few turns. Stop exploring. If you have code, commit and create a PR now.", current, max),
		90: fmt.Sprintf("[Turn %d/%d] 🚨 Only %d turns remain. If you have code to submit, commit and create a PR IMMEDIATELY. If you're stuck, post a comment explaining what blocked you using forgejo_comment.", current, max, max-current),
	}

	// Check each threshold
	for threshold, msg := range thresholds {
		if pct >= float64(threshold) && !te.turnSteered[threshold] {
			te.turnSteered[threshold] = true
			// Inject steering message as a user message (system role breaks Scaleway API after tool messages)
			messages = append(messages, provider.Message{
				Role:    "user",
				Content: "[Fordjent Steering] " + msg,
			})
		}
	}

	// Inactivity detection for implementer role
	if te.role == "implementer" && current > 15 {
		writes, hasWrites := te.toolCallCounts["write_file"]
		prs, hasPRs := te.toolCallCounts["forgejo_create_pr"]
		if (!hasWrites || writes == 0) && (!hasPRs || prs == 0) {
			if !te.turnSteered[-1] { // use -1 as special key for inactivity
				te.turnSteered[-1] = true
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: "[Fordjent Steering] You are in implementer role but haven't written any code files yet (no write_file calls). If you understand the task, use write_file to create your first code file. If you're blocked, use forgejo_comment to explain what's blocking you.",
				})
			}
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
