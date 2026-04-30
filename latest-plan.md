# Fordjent Reliable Agent — Consolidation, Hardening & Refactor Plan

**Date:** 2026-04-30
**Branch:** `master` (ahead of origin by ~1 commit)
**Working tree:** Clean (13ce6cd consolidated)
**Go toolchain:** Unavailable on host — static analysis only; build via Docker verified

---

## Current State

Build passes via Docker multi-stage. ~4,333 lines of code in the reliable-agent layer are committed but not yet integrated (session store, cost tracking, turn executor, lifecycle, stale gate, merge queue, scaffold detection, scheduler).

The session manager at `internal/session/manager.go` is **784 lines**, containing both the event routing/session lifecycle logic AND the entire `Agent` struct with its 327-line `ProcessEvent` turn loop.

This file is doing too many things. The plan below fixes the immediate fragilities first, then extracts the Agent orchestration into its own package and file.

---

## PHASE 1: Fragility Fixes (P1)

### P1.1 — Sentinel Error for Max-Turns Classification

**Files:** `internal/agent/turn.go`, `internal/session/manager.go`

**Why:** `manager.go:375` uses `strings.Contains(err.Error(), "max turns")`. If `TurnExecutor.Run()` returns a wrapped or renamed error, this match silently fails and misclassifies max-turns as generic `failed_error`.

**What to change:**

In `internal/agent/turn.go`, add a package-level sentinel:
```go
var ErrMaxTurnsReached = errors.New("max turns reached")
```

In `internal/session/manager.go:631`, wrap the sentinel error:
```go
// BEFORE
return fmt.Errorf("max turns (%d) reached", a.cfg.Agent.MaxTurns)

// AFTER
return fmt.Errorf("%w: %d turns", agent.ErrMaxTurnsReached, a.cfg.Agent.MaxTurns)
```

In `internal/session/manager.go:375`, use `errors.Is`:
```go
// BEFORE
if strings.Contains(err.Error(), "max turns") {

// AFTER
if errors.Is(err, agent.ErrMaxTurnsReached) {
```

**Imports needed:** `errors` in manager.go (already imported implicitly via `fmt.Errorf` wrappers, but verify); `errors` in agent/turn.go.

**Risk:** Very low. Behavioral invariant changes only how the error is wrapped, not what it means.

---

### P1.2 — Bash Tool Test on Environments Without `/bin/bash`

**Files:** `internal/tool/local_tools.go`, `internal/tool/local_tools_test.go`

**Why:** Alpine (and some bookworm-slim variants) lacks `bash`. The tool hard-codes `exec.CommandContext(ctx, "bash", "-c", params.Command)` at line 63. The AGENTS.md explicitly says "TestBashToolSuccess still fails due to Alpine lacking bash."

**What to change:**

Two options — production tool should be robust, or test should skip.

**Option A (preferred — make tool robust):**
In `internal/tool/local_tools.go` (~line 63), prefer `bash` but fall back to `sh`:
```go
shell := "sh"
if _, err := exec.LookPath("bash"); err == nil {
    shell = "bash"
}
cmd := exec.CommandContext(ctx, shell, "-c", params.Command)
```

**Option B (minimal — skip tests):**
In `internal/tool/local_tools_test.go`, add a `skipIfNoBash` helper to each `TestBashTool*` test.

**Recommendation:** Do Option A. `sh` is POSIX and universally installed. Behavior is identical for the simple commands the model issues.

---

### P1.3 — Telegram Responder Context Cancellation

**Files:** `internal/telegram/responder.go` lines 28, 46, 68

**Why:** Three `// TODO: respect ctx cancellation` comments. `Acknowledge`, `SendResponse`, and `Error` all receive a `ctx context.Context` but never check `ctx.Done()` before calling `bot.Send`.

**What to change:**
Add a guard at the top of each public method:
```go
func (r *Responder) Acknowledge(ctx context.Context, evt *event.Event) error {
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }
    // ... rest of existing code
}
```

Repeat for `SendResponse` and `Error`.

**Note:** `telebot.v4` may not itself respect `context.Context` in its `Send` API (depends on telebot version). The guard at least prevents the Fordjent side from doing work on a cancelled context before calling the API.

---

## PHASE 2: Architecture Refactor (P2)

### P2.1 — Extract Agent from session/manager.go into internal/agent/agent.go

**Summary:** The `Agent` struct (lines 457-784 of manager.go) is 327 lines of pure orchestration logic sitting inside the session manager. The session manager should route events and manage the queue; the agent should own the turn loop, system prompt building, and tool execution.

**Why not a new package?** `manager.go` already imports `internal/agent` (for `agent.TurnExecutor` and `agent.ContextTracker`). If we moved `Agent` to `internal/agent` and still needed `Session` from `internal/session`, we'd have a circular import:
- `session` → imports `agent` (for TurnExecutor, context)
- `agent` → would need `session.Session` (for sess.WorkDir, sess.RepoDir, etc.)

**Resolution:** Put the Agent in the `session` package but in its own file: `internal/session/agent.go`. This is a same-package split — `Agent` keeps unexported access to `Session` etc., and `manager.go` shrinks to ~455 lines.

**What to move from manager.go → session/agent.go:**

From `internal/session/manager.go`, extract all of these to a new file:

```go
package session

import (
    "context"
    "encoding/json"
    "fmt"
    "log/slog"
    "os/exec"
    "strings"
    "time"

    "github.com/fordjent/fordjent/internal/agent"
    "github.com/fordjent/fordjent/internal/config"
    "github.com/fordjent/fordjent/internal/cost"
    "github.com/fordjent/fordjent/internal/event"
    "github.com/fordjent/fordjent/internal/forgejo"
    "github.com/fordjent/fordjent/internal/memory"
    "github.com/fordjent/fordjent/internal/mergequeue"
    "github.com/fordjent/fordjent/internal/metrics"
    "github.com/fordjent/fordjent/internal/provider"
    "github.com/fordjent/fordjent/internal/tool"
)

// Agent is the per-session agent that processes events via LLM + tools.
type Agent struct {
    cfg         *config.Config
    sess        *Session
    forgejo     *forgejo.Client
    llm         *provider.Client
    tools       *tool.Registry
    mem         *memory.Memory
    costTracker *cost.Tracker
    executor    *agent.TurnExecutor
}

func NewAgent(...) *Agent { /* ... existing constructor code ... */ }

func (a *Agent) ProcessEvent(ctx context.Context, evt *event.Event) error { /* ... */ }
func (a *Agent) addReaction(...) { /* ... */ }
func (a *Agent) buildSystemPrompt(...) string { /* ... */ }
func (a *Agent) targetDescription(...) string { /* ... */ }
func (a *Agent) buildContext(...) ([]provider.Message, error) { /* ... */ }
func (a *Agent) eventToUserMessage(...) string { /* ... */ }

// The two adapter types can stay where they are or move with Agent.
```

**What stays in manager.go:**
- `Session` struct definition
- `Manager` struct + constructor `NewManager`
- Event routing: `Run`, `handleEvent`, `getOrCreate`
- Reaping: `reapIdle`, `evictOldest`, `shutdownAll`
- Session persistence: `restoreSessions`
- `resolveDBPath`, `buildCloneURL`, `sessionInfoAdapter`, `agentConfigAdapter`

**Key concern:** `Agent` currently depends on `Session` fields like `WorkDir`, `RepoDir`, `Key`, `Repository`. It also depends on `mergequeue.Client` and `cost.Tracker` from the manager. The current constructor signature:

```go
func NewAgent(cfg *config.Config, sess *Session, mq *mergequeue.Client, ct *cost.Tracker) *Agent
```

This must remain unchanged after the file split — `Agent` still needs these types. The only change is which file holds the code.

**Build verification after P2:** `go build ./...` (or Docker build — verified to work). No behavioral changes.

---

## PHASE 3: Static Validation (P0 — what we can do without live tests)

Since `go` is missing from the host and Docker volume mounts were blocked, these are the static checks we already performed:

1. **Build passes:** `docker build -t fordjent:test-build .` succeeded (13 steps, all cached or built)
2. **No import cycles:** `internal/session` imports `internal/agent`; `internal/agent` does NOT import `internal/session` — confirms same-package split is safe
3. **No stale references to `ErrMaxTurnsReached`:** grep confirmed this symbol does not yet exist (ok to add)
4. **Agent struct is self-contained:** All its fields and methods reference types directly owned by the struct or passed in constructor — no globals, no package-level state — safe to move

---

## Execution Order

```
1. P1.1 — Sentinel error for max-turns (touch: agent/turn.go, session/manager.go)
2. P1.2 — Bash tool sh fallback           (touch: tool/local_tools.go, tool/local_tools_test.go)
3. P1.3 — Telegram ctx guards              (touch: telegram/responder.go)
4. Commit P1 changes
5. P2.1 — Extract Agent to session/agent.go (touch: session/manager.go, new session/agent.go)
6. Commit P2 changes
```

---

## Deferred / Out of Scope

- **P0 live integration:** Needs working Go + live Forgejo + Ollama Cloud (minimax-m2.5) stack. Scheduled after P1+P2.
- **Cost-per-token accuracy:** `ContextTracker.EstimateTokens()` does chars/4, ±30% potentially. LLM provider reports actual usage after each call. If needed, replace heuristic with provider-reported token counts accumulated per turn.
- **Turn count exposure:** `TurnExecutor.Run` does not expose retry count in `TurnResult` — line 91 of agent/turn.go has `// TODO: expose retry count`. Deferred.
