# Architecture Review — Fordjent

**Reviewer**: review-tuna  
**Date**: 2026-05-18  
**Scope**: Event-driven architecture, package boundaries, coordination layer, FSM state machine, provider abstraction

---

## 1. Package Boundaries and Dependencies

### Current Dependency Graph (simplified)

```
cmd/fordjent/main.go
  → internal/config
  → internal/event
  → internal/session  (depends on: forgejo, config, event, lifecycle, cost, mergequeue, scheduler, scaffold, tool, agent, sentinel, provider)
  → internal/webhook  (depends on: config, event, forgejo, lifecycle, metrics, webui)
      |
internal/provider   → internal/config, internal/sentinel
internal/forgejo    → internal/sentinel
internal/tool       → internal/forgejo, internal/sentinel, internal/stalegate
internal/agent      → internal/config, internal/provider, internal/cost, internal/tool, internal/memory, internal/sentinel
internal/mergequeue → internal/tool
internal/scheduler  → internal/tool, internal/forgejo
internal/lifecycle  → internal/cost, internal/forgejo
internal/scaffold   → internal/forgejo
internal/memory     → internal/config, internal/event, internal/forgejo
internal/cost       → internal/config
internal/sentinel   → (stdlib only)
```

### Finding: `internal/session` is a god-package with 11 direct dependencies

`internal/session` imports 11 internal packages directly (`forgejo`, `config`, `event`, `lifecycle`, `cost`, `mergequeue`, `scheduler`, `scaffold`, `tool`, `agent`, `sentinel`). This is the central hub through which all coordination flows. While this reflects the package being the "glue" layer, it creates tight coupling:

- Any change to `mergequeue.CheckGate` signature requires updating `session/manager.go`  
- Any change to `scaffold.CheckAndBlock` signature requires updating `session/manager.go`  
- Any change to `lifecycle` method signatures requires updating `session/manager.go`

**Risk**: The `session` package has no interface boundaries for these dependencies. It depends directly on concrete types from `mergequeue`, `scheduler`, `scaffold`, and `lifecycle`. If any of these packages need refactoring (e.g., `mergequeue` adds constructor parameters), `session/manager.go` changes.

**Recommendation**: Define thin interfaces in `session/` for each subsystem it depends on:

```go
// internal/session/ports.go
type MergeGater interface {
    CheckGate(ctx, repo, head, base) (blocked bool, msg string, err error)
}
type DependencyScheduler interface {
    OnPRMerged(ctx, repo, prNum) error
    CheckAndUnblock(ctx, repo) error
}
type Lifecycler interface {
    OnSessionStart(ctx, key)
    OnSessionComplete(ctx, key, repo, issue)
    OnSessionFailedMaxTurns(ctx, repo, issue, key)
    OnSessionFailedError(ctx, repo, issue, key, err)
    GetState(ctx, key) (string, error)
    RecordTurn(ctx, key, turn, toolCalls, latency, tin, tout, err)
}
```

Then `mergequeue.Client`, `scheduler.Scheduler`, and `lifecycle.Lifecycle` implement these interfaces implicitly. This lets session evolve independently and makes testing trivial (mock interfaces).

### Finding: Circular dependency risk via `forgejo_tools.go` → `stalegate` ← `forgejo_tools`

The import chain is: `internal/tool` → `internal/stalegate` → (nothing else). The stalegate package is a leaf — no circularity risk. Similarly `scheduler` → `tool` is one-way. The graph is a DAG, which is good.

### Finding: `ForgejoAdapter` is a leaky abstraction (tool/forgejo_tools.go:811-857)

The adapter wraps `forgejo.Client` but also exposes `BaseURL()`, `Token()`, and `HTTPClient()` for backward compatibility with `mergequeue` and `scheduler`. This means both `mergequeue.Client` and `scheduler.Scheduler` bypass the typed `forgejo.Client` API and make raw HTTP calls using the adapter's token/URL/HTTPClient:

```go
// mergequeue/queue.go:29-35
func NewClient(adapter *tool.ForgejoAdapter) *Client {
    return &Client{
        BaseURL: adapter.BaseURL(),
        Token:   adapter.Token(),
        HTTP:    adapter.HTTPClient(),
    }
}
```

**Consequence**: `mergequeue.Client` and `scheduler.Scheduler` each implement their own HTTP methods (`doGet`, `doPost`, `doDelete`) with duplicate authentication logic, error handling, and URL construction. This duplicates `forgejo.Client.doRequest()` logic. Changes to auth headers, error classification, or timeout configuration in `forgejo.Client` are NOT reflected in `mergequeue` or `scheduler`.

**Recommendation**: Have `mergequeue` and `scheduler` take a `*forgejo.Client` directly, or better, define a minimal interface:

```go
type ForgejoReader interface {
    GetIssue(ctx, repo, num) (*forgejo.Issue, error)
    ListIssues(ctx, repo, state, limit) ([]forgejo.Issue, error)
    AddIssueLabels(ctx, repo, num, labels) error
    RemoveIssueLabel(ctx, repo, num, label) error
    PostIssueComment(ctx, repo, num, body) error
    EnsureLabels(ctx, repo) error
}
```

---

## 2. Event-Driven Architecture

### Data Flow

```
Forgejo Webhook → Router.handleWebhook()
    → validateSignature()
    → normalizeEvent() (string → event.Type)
    → isAgentEvent() (filter agent-originated)
    → bus.Publish(evt)
        → session.Manager.handleEvent()
            → labelBoot, FSM, scaffold, role gate
            → getOrCreate() (clone repo, create session)
            → sess.events <- evt
                → runSession() → Agent.ProcessEvent()
```

### Finding: Event normalization is correct but fragile (router.go:622-708)

The `normalizeEvent` method uses inline closure functions (`extractRepo`, `extractSender`, `extractIssueNum`, `extractPRNum`) that capture `payload` as a closure. The `extractPRNum` function has special logic for `issue.is_pull_request` (line 657-663) which handles PR comments sent by Forgejo as issue_comment events with `is_pull_request: true`. This is correct behavior for Forgejo v9.x API semantics.

**Risk**: The event type mapping (lines 680-693) maps Forgejo event type strings to internal event types with a simple switch. New Forgejo event types (e.g., `pull_request_review`, `release`, `package`) would be silently dropped with "unsupported event type" logged at Warn level. This is fine architecturally (fall-through to ignore), but should be documented.

### Finding: Bus channel buffering and back-pressure (event.go:69, 90-101)

The bus has a `256`-message buffer per subscriber. `Publish` uses a non-blocking send with `default` fallback — if a subscriber's buffer is full, the event is dropped silently:

```go
select {
case sub <- evt:
case <-ctx.Done():
    return
default:
    // Drop event if subscriber is full (back-pressure)
}
```

**Risk**: If the session manager's event handler blocks (e.g., slow Forgejo API call in `handleEvent` before queuing to `sess.events`), the bus buffer can fill, and events are silently dropped. The only indicator is a missing webhook in the lifecycle DB.

**Impact**: With a burst of events (e.g., 10 issues created simultaneously + their label events), 256-buffer is fine. But a slow Forgejo API call (e.g., `EnsureLabels` taking 10 seconds) could cause drops during other concurrent activity.

**Recommendation**: Add a counter/metric for dropped events so operators can detect buffer exhaustion.

### Finding: Session event queue overflow (manager.go:514-525)

Session event buffers are 64 messages. When full, events are silently dropped with a `slog.Warn`:

```go
select {
case sess.events <- evt:
default:
    slog.Warn("session event queue full, dropping event", ...)
}
```

**Risk**: If a session is in its agent processing loop (which may take 30-90 seconds per turn) and another event arrives (e.g., a comment + label change + push), the buffer can fill. The dropped event is lost forever — no retry, no replay.

**Recommendation**: Increase buffer to 256, or implement a FIFO with last-value semantics for non-critical events (label updates are idempotent; comment events are not).

---

## 3. FSM State Machine (internal/lifecycle/)

### Current States

```
created → working → pr_created → completed
                   → blocked → ... (auto-requeue)
                   → failed_max_turns
                   → failed_error
```

### Finding: Transition validation exists but is one-directional (manager.go:393)

`IsTransitionValid` is called in `manager.go:393` but is defined in `lifecycle` package. Looking at the code, transitions are validated per-label-change, but the validation only checks FSM label states (planning → blocked → plan-approved → implementing → review → done). The session-lifecycle states (created → working → completed) have NO explicit validation — they're recorded via `RecordTransition` in `lifecycle.go` without checking whether the transition is valid.

**Missing**: A `completed → working` transition is not explicitly prevented. If a completed session receives another event (e.g., a late webhook), `runSession` would record `OnSessionStart` again, transitioning from `completed` → `working`. The guard at `lifecycle.go:92-95` prevents double-start:

```go
current, _ := l.GetState(ctx, sessionKey)
if current == StateWorking {
    return
}
```

But it doesn't check for `StateCompleted`. If a completed session gets a new event, it would record `created → working` again (since `GetState` returns `completed`, not `""` — no wait, the guard checks `current == StateWorking`, so if current is `completed`, it would record `completed → working`).

**Recommendation**: Add guards in `OnSessionStart` for terminal states:

```go
func (l *Lifecycle) OnSessionStart(ctx context.Context, sessionKey string) {
    current, _ := l.GetState(ctx, sessionKey)
    if current == StateWorking || current == StateCompleted || strings.HasPrefix(current, "failed") {
        return
    }
    ...
}
```

### Finding: FSM state and lifecycle state are two different state machines

There are TWO state machines in the codebase:
1. **FSM issue state** (in labels: `planning`, `blocked`, `plan-approved`, `implementing`, `review`, `done`) — managed in `manager.go:376-419` via `StateFromLabels`
2. **Session lifecycle state** (in SQLite: `created`, `working`, `pr_created`, `blocked`, `completed`, `failed_*`) — managed in `lifecycle.go`

These are conceptually related but implemented independently. `StateFromLabels` translates labels (FSM) to an `IssueState`; `RecordTransition` tracks session lifecycle. When a PR is merged (#6), the lifecycle transitions to `completed`, but there's no connection to the issue's FSM label state (e.g., a `blocked` label added after session completion would not trigger a lifecycle transition).

**This is not a bug** — they serve different purposes. But the duality should be documented clearly to prevent confusion.

### Finding: `blocked` label vs lifecycle `blocked` state collision

When the merge queue blocks a PR:
1. `lifecycle.OnSessionBlocked()` adds label `"blocked"` to the issue (line 123)
2. This emits a `label_updated` webhook
3. `manager.go:379-419` detects the label change → `StateFromLabels` → possibly triggers FSM logic
4. `manager.go:433-436` drops the label_updated event to prevent feedback loops

This is correct — the feedback loop is broken. But it means the FSM state machine never learns about the merge-queue's `blocked` label, and the lifecycle's `ResolveBlockedBranch` never triggers a label removal → the `blocked` label stays on the issue even after the branch is resolved.

**Actual behavior**: Looking at `manager.go:296-306`, when merge gate clears, the manager posts a comment but does NOT remove the `blocked` label. The agent is told it may retry, but must manually remove `blocked` via `forgejo_add_labels` or rely on `issueStateInstructions` (agent.go:759-771) to detect resolved deps and remove the label.

**This is correct by design** — the agent in FSM `blocked` state is instructed to verify dependencies and remove the label. But it's subtle.

---

## 4. Coordination Layer (mergequeue + scheduler + scaffold + stalegate)

### Composition Analysis

```
IssueOpened → scaffold.CheckAndBlock()
    → If repo empty: create scaffold issue, label this issue "blocked"
    → Return blocked=true → session creation skipped

PR Creation Path:
    stalegate.IsStale() → auto-rebase if stale
    → forgejo_create_pr auto-push
    → mergequeue.CheckGate() → block if file overlap
    → go build + go test + golangci-lint verify gate
    → POST PR

PR Merged → scheduler.OnPRMerged()
    → Scan open issues for "Depends on: #N"
    → Remove "blocked", add "ready", post unblock comment
    → mergegate re-check blocked branches → auto-nudge
```

### Finding: Scaffold and stalegate have a known composition gap

When scaffold creates the scaffold issue and labels the triggering issue as `blocked`, the scaffold issue itself gets processed independently. The scaffold session (agent working on the scaffold issue):
1. Creates go.mod, README, .gitignore
2. Commits and pushes (bypasses `forgejo_create_pr` per system prompt rule #5)
3. Push triggers push handler → closes scaffold issue → scheduler unblock check

**Gap**: The stalegate (`stalegate.go:37-40`) treats "remote has no base branch" as not stale and returns `sentinel.ErrNoRemoteRef`. This is correct — but if a scaffold session delays and a subsequent implementer session tries to create a PR before the push completes, the stalegate may behave unexpectedly. **Mitigation**: the issue is labeled `blocked` until the scaffold is complete.

### Finding: Merge queue bypasses on error (mergequeue.go:69, 79)

When `CheckGate` can't diff the branch (e.g., branch doesn't exist yet on remote), it logs a warning and returns `(false, "", nil)` — allowing the PR through. This is correct: better to allow a potentially conflicting PR than to block silently.

Similarly, when `listOpenPRs` fails (API error), it allows through. The net effect: the merge queue is best-effort, not a hard gate. This is appropriate.

### Finding: `stalegate.IsStale` modifies the repo as a side effect

`IsStale` (stalegate.go:22) runs `git rebase origin/main` and `git push --force-with-lease` as a side effect of checking staleness. This is unusual for a function named `IsStale` (which sounds like a query). Callers expect a check, not a mutation. Consider renaming to `EnsureUpToDate` or splitting into `IsStale()` (query) + `RebaseBranch()` (action).

### Finding: Scheduler double-checks deps across all open issues on every merge

On every PR merge, `OnPRMerged` scans ALL open issues in the repo and checks ALL their dependencies. With 100 open issues, this means up to 100 × N `GET /issues/{N}` API calls (one per dependency per issue). On busy repos, this could generate significant API load.

**Mitigation**: `EnsureLabels` is called on every scan (line 84-88), adding another N API calls. This is redundant — labels only need bootstrapping once.

---

## 5. Provider Abstraction

### Architecture

```
ChatCompleter (interface)
    ↑                    ↑
Client (retry+ sema)   FallbackClient (delegates to primary → fallback)
```

### Finding: `FallbackClient` only handles two providers (provider/client.go:52-57)

The fallback is constructed in `NewAgent` (agent.go:53-58):

```go
prov := cfg.ProviderForRole(role)
var llmClient provider.ChatCompleter = provider.NewClient(prov)
if cfg.Agent.FallbackProvider != "" {
    fallbackProv := cfg.ProviderByName(cfg.Agent.FallbackProvider)
    if fallbackProv != nil && fallbackProv.Name != prov.Name {
        fallbackClient := provider.NewClient(fallbackProv)
        llmClient = provider.NewFallbackClient(llmClient.(*provider.Client), fallbackClient)
    }
}
```

**Risk**: The type assertion `llmClient.(*provider.Client)` will panic if `NewClient` ever returns a different type. This is a latent bug waiting for future refactoring.

**Recommendation**: Make the fallback wrapping accept `ChatCompleter` on both sides, or add a `WrapFallback(primary, fallback ChatCompleter) ChatCompleter` factory.

### Finding: Retry and fallback don't compose correctly

The `Client` already has retry (via `c.retry.Retry`). The `FallbackClient` has its own retry. If the primary fails with a non-retryable error, the fallback wraps around and retries. If the primary fails with a retryable error, the primary retries first (3 attempts), then upon exhaustion the fallback kicks in and retries again (3 attempts). This means up to 9 attempts per LLM call (3 primary + 3 fallback + potentially 3 more from the fallback's own retry).

**Recommendation**: Document the retry chain clearly. Consider making retry a middleware that wraps around the fallback, not per-provider:

```
External Retry (3 attempts)
    → Primary Provider (no internal retry)
        → on failure → Fallback Provider (no internal retry)
```

### Finding: No circuit breaker or rate limiter at the provider level

The `sema` channel (provider/client.go:171) limits concurrent calls, but there's no circuit breaker. If the LLM provider is returning 503s, the retry policy will exhaust all 3+3+3=9 attempts before giving up, wasting time. A simple circuit breaker (e.g., fail fast after 5 consecutive errors in 30 seconds) would improve throughput during provider outages.

### Finding: Provider config uses stringly-typed tier (config.go:306-308)

```go
if p.Tier != "" && p.Tier != "strong" && p.Tier != "fast" {
```

The `Tier` field is a free-form string with only validation for `strong`/`fast`. But `AgentConfig.FastProvider` is deprecated in favor of `RoleProviders`. The `ProviderForRole` method (config.go:336-356) already handles the mapping. Recommend removing the deprecation cycle entirely:

```
Remove: FastProvider, Tier
Keep: RoleProviders (role → provider name)
```

---

## Summary

### Critical Architectural Issues

| # | Issue | Severity | Location |
|---|-------|----------|----------|
| 1 | `internal/session` has 11 direct concrete dependencies, no interfaces | High | session/manager.go |
| 2 | `forgejo.Client.doRequest()` logic duplicated in mergequeue + scheduler | High | mergequeue/queue.go, scheduler/scheduler.go |
| 3 | Fallback wrapping uses type assertion `.(*provider.Client)` that could panic | High | session/agent.go:57 |
| 4 | `OnSessionStart` allows re-entering `working` from `completed` state | Medium | lifecycle/lifecycle.go:91-97 |

### Medium Concerns

| # | Issue | Location |
|---|-------|----------|
| 5 | Bus drops events silently with no metric counter | event/event.go |
| 6 | Session channel drops events on buffer overflow | session/manager.go:514-525 |
| 7 | `stalegate.IsStale` has mutation side effects (rebase + push) | stalegate/stalegate.go |
| 8 | Two parallel state machines (FSM labels + lifecycle SQLite) with collision surface | lifecycle.go + manager.go |
| 9 | Provider retry chain could attempt 9+ retries per LLM call | provider/client.go + provider/retry.go |
| 10 | Scheduler scans ALL issues on every merge, redundant `EnsureLabels` | scheduler/scheduler.go |

### Recommendations

1. **Interface boundaries**: Define `MergeGater`, `Lifecycler`, `DependencyScheduler`, `ForgejoReader` interfaces in `internal/session/ports.go`
2. **Consolidate HTTP client**: Remove raw HTTP from `mergequeue` and `scheduler` — use `forgejo.Client` directly
3. **Defensive `OnSessionStart`**: Guard against re-entering `working` from terminal states
4. **Drop event metrics**: Add `metrics.DroppedEvents` counter
5. **Bus buffer monitoring**: Add a `PublishWithMetric` wrapper
6. **Rename `IsStale`** to `RebaseIfStale` to reflect mutation semantics
7. **Document two-state-machine model** in a README or architecture doc
8. **Fix fallback type assertion** to accept `ChatCompleter` on both sides
