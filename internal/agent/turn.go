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
}

// TurnExecutor runs the LLM loop for a session with compaction, retries, and cost tracking.
type TurnExecutor struct {
	cfg          *config.Config
	llm          provider.ChatCompleter
	tools        *tool.Registry
	tracker      *ContextTracker
	costTracker  *cost.Tracker
	sessionKey   string
	repository   string
}

func NewTurnExecutor(
	cfg *config.Config,
	llm provider.ChatCompleter,
	tools *tool.Registry,
	costTracker *cost.Tracker,
	sessionKey, repository string,
) *TurnExecutor {
	tracker := NewContextTracker(
		cfg.Agent.ContextWindow,
		cfg.Agent.CompactionThreshold,
		cfg.Agent.CompactionKeepTurns,
	)
	return &TurnExecutor{
		cfg:         cfg,
		llm:         llm,
		tools:       tools,
		tracker:     tracker,
		costTracker: costTracker,
		sessionKey:  sessionKey,
		repository:  repository,
	}
}

// Run executes one LLM turn: handles compaction before the call, records cost after.
func (te *TurnExecutor) Run(ctx context.Context, systemPrompt string, messages []provider.Message) (*TurnResult, []provider.Message, error) {
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
	response, usage, err := te.llm.Chat(ctx, systemPrompt, messages, te.tools.Tools())
	latency := time.Since(start)

	// Retry count not directly exposed by Client.Chat; we log what we can.
	// TODO: expose retry count from provider client if needed.

	if err != nil {
		return nil, messages, fmt.Errorf("LLM chat failed: %w", err)
	}

	te.tracker.Update(usage)

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
		"latency_ms", latency.Milliseconds(),
		"tokens_in", usage.PromptTokens,
		"tokens_out", usage.CompletionTokens,
		"cost_usd", costUSD,
		"tool_calls", toolCount,
		"compacted", compacted,
		"utilization", te.tracker.Utilization(messages),
	)

	result := &TurnResult{
		Turn:      te.tracker.TotalTurns(),
		Response:  response,
		Usage:     usage,
		CostUSD:   costUSD,
		Latency:   latency,
		ToolCalls: toolCount,
		Compacted: compacted,
	}

	return result, messages, nil
}
