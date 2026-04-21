package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

var (
	eventsTotal    atomic.Int64
	sessionsTotal  atomic.Int64
	sessionsActive atomic.Int64
	toolCallsTotal atomic.Int64
	llmCallsTotal  atomic.Int64
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

// IncLLMCalls increments the total LLM calls counter.
func IncLLMCalls() {
	llmCallsTotal.Add(1)
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
	}
}
