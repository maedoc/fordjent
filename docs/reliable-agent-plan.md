# Reliable Agent Layer — Implementation Plan

## Goal
Replace Fordjent's brittle "fire and forget" LLM loop with a resilient, observable, cost-aware agent layer that stays up under load, manages context windows automatically, and tracks spend.

## Current State (Brittle)

| Component | Current Behavior | Problem |
|-----------|---------------|---------|
| `provider.Client.Chat()` | 120s timeout, single attempt | Parallel wave → all 5 sessions hit `context deadline exceeded` simultaneously |
| Error handling | `err != nil` → hard abort | No retry on 529/503/rate-limit. Session just dies. |
| Token usage | Parsed from response, then discarded | No cost tracking. No early-warning when context is full. |
| Message buffer | Grows unbounded | Issue #19 maxed at turn 49 because context got too long? No, turns maxed. But long traces compound. |
| Memory compaction | Only via `memory.go` git-notes | Not integrated into the LLM loop. No automatic context reduction. |
| Metrics | Simple counters (events, sessions, tool calls, LLM calls) | No cost, no token usage, no latency histograms |

## Proposed Changes

---

## Phase 1: Provider Resilience (Retry + Exponential Backoff)

### Why First
If the provider flakes, nothing else matters. The parallel wave died here.

### Changes

**`internal/config/config.go`**
- Add `ProviderConfig.RequestTimeout` (default: 120s)
- Add `ProviderConfig.MaxRetries` (default: 3)
- Add `ProviderConfig.RetryBaseDelay` (default: 2s)
- Add `ProviderConfig.RetryMaxDelay` (default: 30s)

**`internal/provider/client.go`**
- Add `RetryPolicy` struct + `ExponentialBackoff`
- Retry on these errors (configurable regex/list):
  - `context deadline exceeded` (timeout)
  - HTTP 503, 529, 502 (server overload/bad gateway)
  - HTTP 429 (rate limit) — with longer delay via `Retry-After` header
- Retry **NOT** on:
  - 400, 401, 403, 404 (client error)
  - Malformed JSON in response
  - Tool schema rejection (the model just can't do it)
- Log each retry attempt with `attempt`, `delay`, `error`
- Return final error with all attempts summarized

**`internal/provider/client_test.go`** (new)
- Mock server returning 503 twice then 200
- Mock server returning 429 with Retry-After
- Mock server returning 400 → no retry
- Verify backoff timing (jitter?)

### Milestone
Unit tests pass. Configurable retry policy. No code changes outside provider.

---

## Phase 2: Context Window Management + Auto-Compaction

### Why
The agent can burn 50+ turns. Each turn appends assistant message + tool results. Context window exhaustion causes silent truncation or API errors.

### Token Counting Approach
**Option A**: Local estimation (tiktoken equivalent in Go). Heavy dependency.
**Option B**: Server-reported usage from each `Chat()` call. Already available in `openAIResponse.Usage`. Simpler, slightly lagging (only counts *after* the call, not before).

**Selection: Option B** — use server-reported `Usage`. Track cumulative tokens per session. Before each `Chat()` call, estimate next prompt size = previous total + new messages. If estimated > threshold → compact.

### Changes

**`internal/provider/client.go`**
- `Chat()` returns `*Usage` alongside `*Response`:
  ```go
  type Usage struct {
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
  }
  ```

**`internal/agent/context.go`** (new package)
- `ContextTracker` struct:
  - `totalTokens int` — cumulative across all turns in session
  - `windowSize int` — from config or provider default (128k)
  - `compactionThreshold float64` — e.g. 0.80 (compact at 80%)

- `Check(contextMessages, newMessages) (shouldCompact bool, estimate int)`
- `Compact(messages []Message, systemPrompt string, summary string) []Message`
  - Strategy: drop old non-system messages except last N (configurable, e.g. keep last 8 turns)
  - Inject `[Context Compacted]` system note before compaction point

**`internal/agent/summarizer.go`** (new)
- `Summarize(messages []Message) (string, error)` — calls LLM with a special "compact this" prompt
  - Alternative: simpler strategy — just truncate. Keep system prompt + last N turns. No extra LLM call.
  - **Selection: Simple truncation first.** We can add LLM-based summarization later if truncation is too lossy.

**`internal/session/manager.go`**
- Before `a.llm.Chat()`:
  1. Estimate token count of `messages` (rough: 4 chars ≈ 1 token for estimation)
  2. If estimated > threshold:
     - Call compaction (drop messages[1 : len-keepN], inject summary marker)
     - Log: `"compacting context", "before": N, "after": M, "reason": "threshold"`

**`internal/config/config.go`**
- Add `AgentConfig.ContextWindow` (default: 128000)
- Add `AgentConfig.CompactionThreshold` (default: 0.80)
- Add `AgentConfig.CompactionKeepTurns` (default: 8)

### Milestone
Session with 50-turn synthetic workload doesn't crash. Compaction fires automatically. Token estimate within ±20% of actual.

---

## Phase 3: Cost Tracking + Budgets

### Why
Ollama Cloud is "free tier resets every 5 hours" — but other providers charge per token. Enterprise use needs visibility.

### Changes

**`internal/config/config.go`**
- Add `ProviderConfig.CostPer1MInputTokens` (float64, default 0)
- Add `ProviderConfig.CostPer1MOutputTokens` (float64, default 0)
- Add `ProviderConfig.CostPer1MCacheReadTokens` (float64, default 0)
- Add `ProviderConfig.CostPer1MCacheWriteTokens` (float64, default 0)
- Add `Config.Budget` struct:
  - `Enabled bool`
  - `MaxSessionCost float64` (abort session if exceeded)
  - `MaxMonthlyCost float64` (global circuit breaker)

**`internal/cost/cost.go`** (new package)
- `Tracker` struct:
  ```go
  type SessionCost struct {
    SessionKey   string
    Repository   string
    ProviderName string
    Model        string
    InputTokens  int64
    OutputTokens int64
    CacheRead    int64
    CacheWrite   int64
    TotalCost    float64
    CreatedAt    time.Time
  }
  ```
- `Record(usage Usage, provider ProviderConfig) SessionCost`
- `GetSessionCost(sessionKey) SessionCost`
- `GetRepoCost(repo) float64`
- `GetMonthlyCost() float64`
- Persist to SQLite (same DB as session store) via lightweight schema

**`internal/metrics/metrics.go`**
Add Prometheus-compatible metrics:
```
fordjent_cost_total{provider, model} gauge
fordjent_session_cost{session_key, repository} gauge
fordjent_tokens_total{provider, model, type="input|output"} counter
fordjent_turn_latency_seconds histogram
fordjent_retry_total{provider, status} counter
```

**`internal/session/manager.go`**
- After each `Chat()` call:
  1. Record cost via `cost.Tracker`
  2. Check budget limits
  3. If `MaxSessionCost` exceeded → abort with comment: *"Session budget ($N) exceeded. Pausing work."*
  4. Log per-turn cost: `turn=3 cost=$0.0012 tokens_in=4000 tokens_out=800`

### Milestone
Metrics endpoint shows cost per session. Budget enforcement works. SQLite has cost table.

---

## Phase 4: Observability + Turn Tracing

### Changes

**`internal/provider/client.go`**
- Add `ChatLatency` histogram metric
- Log request/response IDs if available (X-Request-ID header)

**`internal/session/manager.go`**
- Structured turn logs with these fields:
  ```json
  {"level":"info","msg":"turn complete","session_key":"x","turn":3,
   "latency_ms":4500,"tokens_in":12000,"tokens_out":400,
   "cost_usd":0.0032,"retry_count":1,"tool_calls":2}
  ```

**`internal/agent/agent.go`** (refactor)
Extract the turn loop from `ProcessEvent` into a dedicated `Agent.RunTurn()` method:
```go
func (a *Agent) RunTurn(ctx context.Context, systemPrompt string, messages []Message) (*TurnResult, error)
```
This makes testing, retry logic, and compaction easier to reason about.

### Milestone
Session logs are queryable with `jq`. Turn-level latency visible in metrics.

---

## Implementation Order
```
Phase 1  ──►  Phase 2  ──►  Phase 3  ──►  Phase 4
(retry)      (compact)     (cost)       (obs)
```

Each phase is independent, can be merged to `main` separately.

---

## Files to Create

| File | Phase | Purpose |
|------|-------|---------|
| `internal/provider/retry.go` | 1 | Retry policy + exponential backoff |
| `internal/provider/client_test.go` | 1 | Mock server tests |
| `internal/agent/context.go` | 2 | Token tracking + compaction logic |
| `internal/agent/context_test.go` | 2 | Unit tests |
| `internal/cost/cost.go` | 3 | Cost tracking + SQLite persistence |
| `internal/cost/cost_test.go` | 3 | Unit tests |
| `internal/agent/turn.go` | 4 | Extracted `RunTurn` method |

## Files to Modify

| File | Phase | Change |
|------|-------|--------|
| `internal/config/config.go` | 1,2,3 | New fields: `RequestTimeout`, `MaxRetries`, `ContextWindow`, `CompactionThreshold`, cost fields, budget fields |
| `internal/provider/client.go` | 1,3,4 | Retry logic, return `Usage`, latency metrics |
| `internal/session/manager.go` | 2,3,4 | Integrate compaction, cost tracking, structured logging |
| `internal/metrics/metrics.go` | 3,4 | Cost + latency + retry histograms |
| `fordjent.local.yaml` | 1,2,3 | New config values |

---

## Open Questions

1. **Local token estimation vs server-reported?** Server-reported is simpler but can't prevent *first* overflow. We estimate before call (rough: len/4 chars). Good enough.
2. **Compaction strategy?** Truncate old turns + keep system prompt + inject marker. No extra LLM call needed.
3. **Budget enforcement where?** `cost.CheckBudget()` before each turn. Abort if exceeded.
4. **Cost storage: SQLite or just metrics?** SQLite for persistence across restarts; Prometheus for dashboards.
5. **Jitter on backoff?** Yes. `delay = base * 2^attempt + rand(0, jitter)`. Prevents thundering herd.
