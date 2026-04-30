package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

var (
	eventsTotal      atomic.Int64
	sessionsTotal    atomic.Int64
	sessionsActive   atomic.Int64
	toolCallsTotal   atomic.Int64
	llmCallsTotal    atomic.Int64
	llmRetriesTotal  atomic.Int64
	totalCostUSD     atomic.Uint64 // store as micro-cents to avoid float issues in atomic
	totalInputTokens  atomic.Int64
	totalOutputTokens atomic.Int64
)

// IncEvents increments the total events counter.
func IncEvents() {
	eventsTotal.Add(1)
}

// IncSessions increments the total sessions created counter.
func IncSessions() {
	sessionsTotal.Add(1)
}

// SetActiveSessions sets the current number of active sessions.
func SetActiveSessions(n int64) {
	sessionsActive.Store(n)
}

// IncToolCalls increments the total tool calls counter.
func IncToolCalls() {
	toolCallsTotal.Add(1)
}

// IncLLMCalls increments the total LLM API calls counter.
func IncLLMCalls() {
	llmCallsTotal.Add(1)
}

// IncLLMRetries increments the retry counter.
func IncLLMRetries() {
	llmRetriesTotal.Add(1)
}

// AddTokens adds token counts to the totals.
func AddTokens(input, output int64) {
	totalInputTokens.Add(input)
	totalOutputTokens.Add(output)
}

// AddCost accumulates cost in USD.
func AddCost(usd float64) {
	// Store as micro-cents (1 USD = 100_000_000 micro-cents)
	microCents := uint64(usd * 100_000_000)
	totalCostUSD.Add(microCents)
}

// Snapshot returns a plain map of current metric values for JSON status endpoints.
func Snapshot() map[string]interface{} {
	usd := float64(totalCostUSD.Load()) / 100_000_000
	return map[string]interface{}{
		"events_total":      eventsTotal.Load(),
		"sessions_total":    sessionsTotal.Load(),
		"sessions_active":   sessionsActive.Load(),
		"tool_calls_total":  toolCallsTotal.Load(),
		"llm_calls_total":   llmCallsTotal.Load(),
		"llm_retries_total": llmRetriesTotal.Load(),
		"input_tokens":      totalInputTokens.Load(),
		"output_tokens":     totalOutputTokens.Load(),
		"cost_usd":          usd,
	}
}

// Handler returns an http.HandlerFunc that serves Prometheus text format metrics.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		fmt.Fprintf(w, "# HELP fordjent_events_total Total number of webhook events received.\n")
		fmt.Fprintf(w, "# TYPE fordjent_events_total counter\n")
		fmt.Fprintf(w, "fordjent_events_total %d\n", eventsTotal.Load())

		fmt.Fprintf(w, "# HELP fordjent_sessions_total Total number of sessions created.\n")
		fmt.Fprintf(w, "# TYPE fordjent_sessions_total counter\n")
		fmt.Fprintf(w, "fordjent_sessions_total %d\n", sessionsTotal.Load())

		fmt.Fprintf(w, "# HELP fordjent_sessions_active Number of currently active sessions.\n")
		fmt.Fprintf(w, "# TYPE fordjent_sessions_active gauge\n")
		fmt.Fprintf(w, "fordjent_sessions_active %d\n", sessionsActive.Load())

		fmt.Fprintf(w, "# HELP fordjent_tool_calls_total Total number of tool calls executed.\n")
		fmt.Fprintf(w, "# TYPE fordjent_tool_calls_total counter\n")
		fmt.Fprintf(w, "fordjent_tool_calls_total %d\n", toolCallsTotal.Load())

		fmt.Fprintf(w, "# HELP fordjent_llm_calls_total Total number of LLM API calls made.\n")
		fmt.Fprintf(w, "# TYPE fordjent_llm_calls_total counter\n")
		fmt.Fprintf(w, "fordjent_llm_calls_total %d\n", llmCallsTotal.Load())

		fmt.Fprintf(w, "# HELP fordjent_llm_retries_total Total number of LLM request retries.\n")
		fmt.Fprintf(w, "# TYPE fordjent_llm_retries_total counter\n")
		fmt.Fprintf(w, "fordjent_llm_retries_total %d\n", llmRetriesTotal.Load())

		fmt.Fprintf(w, "# HELP fordjent_tokens_total Total number of tokens consumed.\n")
		fmt.Fprintf(w, "# TYPE fordjent_tokens_total counter\n")
		fmt.Fprintf(w, "fordjent_tokens_total{type=\"input\"} %d\n", totalInputTokens.Load())
		fmt.Fprintf(w, "fordjent_tokens_total{type=\"output\"} %d\n", totalOutputTokens.Load())

		usd := float64(totalCostUSD.Load()) / 100_000_000
		fmt.Fprintf(w, "# HELP fordjent_cost_total_total Total cost in USD across all sessions.\n")
		fmt.Fprintf(w, "# TYPE fordjent_cost_total_total counter\n")
		fmt.Fprintf(w, "fordjent_cost_total_total %.6f\n", usd)
	}
}
