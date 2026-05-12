# Fordjent Implementation Plan — Next Session

**Date:** 2026-05-12
**Based on:** Code review, ralph loop analysis (pi-ralph-wiggum), user feedback
**Prerequisites:** Go 1.22+ installed locally, Forgejo accessible (local or remote), OpenAI-compatible API key(s)

---

## Execution Order

```
Phase 1: Security Fixes (P0)                    ~30 min
Phase 2: Ralph Loop Resilience                   ~2 hrs
Phase 3: Local Deployment (macOS, no Docker)     ~1 hr
Phase 4: Strong/Fast Model Config                ~1.5 hrs
Phase 5: Issue Lifecycle FSM                     ~2 hrs
Phase 6: /automerge + Reviewer Auto-merge        ~1 hr
```

Total estimated: ~8 hours across 2-3 sessions.

---

## Phase 1: Security Fixes (P0)

### 1a. Path Traversal in `read_file`

**File:** `internal/tool/local_tools.go` — `readFileTool.readFile()` (~line 310)

**Current:** `absPath := filepath.Join(t.repoDir, path)` — no containment check. Model can pass `../../../../etc/passwd` and `filepath.Join` collapses dots silently.

**Fix:** After existing `repo/` prefix stripping, before `os.Open`:
```go
absPath := filepath.Join(t.repoDir, filepath.Clean(path))
repoClean := filepath.Clean(t.repoDir) + string(os.PathSeparator)
if !strings.HasPrefix(absPath, repoClean) {
    return "", fmt.Errorf("path escapes repository root: %s", path)
}
```

### 1b. Path Traversal in `write_file`

**File:** `internal/tool/local_tools.go` — `writeFileTool.Execute()` (~line 395)

**Current:** `absPath := filepath.Join(t.repoDir, params.Path)` then `os.MkdirAll` + `os.WriteFile` with no containment check. Worse than read — creates directories and writes files anywhere writable.

**Fix:** Same `filepath.Clean` + prefix check, before `os.MkdirAll`:
```go
absPath := filepath.Join(t.repoDir, filepath.Clean(params.Path))
repoClean := filepath.Clean(t.repoDir) + string(os.PathSeparator)
if !strings.HasPrefix(absPath, repoClean) {
    return "", fmt.Errorf("path escapes repository root: %s", params.Path)
}
```

### 1c. Stored XSS in `/activity`

**File:** `internal/webhook/router.go` — `handleActivity()` (~line 170)

**Current:** Raw DB values interpolated into HTML via `fmt.Fprintf`. A Forgejo username containing `<script>...</script>` executes on any operator viewing `/activity`.

**Fix:** Import `"html"`, apply `html.EscapeString()` to every interpolated value:
```go
import "html"
// In handleActivity:
fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td>%s</td></tr>\n",
    html.EscapeString(ts), html.EscapeString(et), html.EscapeString(act),
    html.EscapeString(repo), num, html.EscapeString(sender), html.EscapeString(status))
```

Same for the session transitions table below it.

### 1d. `--force-with-lease` in stale gate

**File:** `internal/stalegate/stalegate.go` (~line 75)

**Current:** `git push -f -u origin HEAD` — bare force-push overwrites remote regardless of new commits.

**Fix:** Replace `-f` with `--force-with-lease`:
```go
exec.Command("git", "-C", repoDir, "push", "--force-with-lease", "-u", "origin", "HEAD")
```

This causes push to fail if the remote has commits not in the local clone, preventing data loss.

### 1e. Tests for path traversal

**File:** `internal/tool/local_tools_test.go` (new tests)

```go
func TestReadFileTraversalBlocked(t *testing.T) {
    dir := t.TempDir()
    os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0644)
    tool := &readFileTool{repoDir: filepath.Join(dir, "repo")}
    os.MkdirAll(tool.repoDir, 0755)

    _, err := tool.readFile(context.Background(), "../../secret.txt", 0, 0)
    if err == nil {
        t.Fatal("expected error for path traversal")
    }
}

func TestWriteFileTraversalBlocked(t *testing.T) {
    dir := t.TempDir()
    tool := &writeFileTool{repoDir: filepath.Join(dir, "repo")}
    os.MkdirAll(tool.repoDir, 0755)

    args := json.RawMessage(`{"path":"../../evil.txt","content":"pwned"}`)
    _, err := tool.Execute(context.Background(), args)
    if err == nil {
        t.Fatal("expected error for path traversal")
    }
}
```

### 1f. Unbounded bash output cap

**File:** `internal/tool/local_tools.go` — `bashTool.Execute()` (~line 95)

**Current:** stdout + stderr captured into `strings.Builder` with no limit.

**Fix:** Add a configurable cap (default 64KB):
```go
const maxBashOutput = 64 * 1024

var stdout, stderr strings.Builder
cmd.Stdout = &limitedWriter{&stdout, maxBashOutput}
cmd.Stderr = &limitedWriter{&stderr, maxBashOutput}
// ...
if stdoutTruncated || stderrTruncated {
    output += "\n[output truncated at 65536 bytes; use offset/limit to page through results]"
}
```

Add a `limitedWriter` type:
```go
type limitedWriter struct {
    w       *strings.Builder
    remain  int
    truncated bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
    if lw.remain <= 0 {
        lw.truncated = true
        return len(p), nil
    }
    if len(p) > lw.remain {
        p = p[:lw.remain]
        lw.truncated = true
    }
    n, err := lw.w.Write(p)
    lw.remain -= n
    return len(p), err // report full length so cmd doesn't get EPIPE
}
```

---

## Phase 2: Ralph Loop Resilience

Adapted from pi-ralph-wiggum patterns. Ralph is synchronous (interactive loop, `ralph_done` tool, completion marker). Fordjent is async (webhook-driven, continuous for-loop per session). The key patterns that apply:

| Ralph Pattern | Fordjent Adaptation |
|---|---|
| `reflectEvery: N` | Inject reflection prompt every N turns in the agent loop |
| `maxIterations` | Already exists as `max_turns` |
| Completion marker | Agent stops when no tool calls returned |
| `ralph_done` tool | Not needed — Fordjent's loop is continuous |
| Stall detection | Track last N turn signatures, warn if identical |
| Turn-level retry | Don't die on single LLM failure — continue to next turn |

### 2a. Turn-level error resilience

**File:** `internal/session/agent.go` — `ProcessEvent()` turn loop (~line 146)

**Current:** First LLM error kills the session:
```go
if err != nil {
    slog.Error("LLM turn failed", ...)
    a.addReaction(ctx, evt, "x")
    return fmt.Errorf("turn %d failed: %w", turn, err)
}
```

**Fix:** Add consecutive error tracking and continue on retryable failures:
```go
consecutiveErrors := 0
maxConsecutiveErrors := 3

for turn := 0; turn < maxTurns; turn++ {
    result, updatedMessages, err := a.executor.Run(ctx, systemPrompt, messages)
    messages = updatedMessages

    if err != nil {
        consecutiveErrors++
        slog.Warn("turn failed",
            "session_key", a.sess.Key,
            "turn", turn,
            "consecutive_errors", consecutiveErrors,
            "error", err,
        )

        if consecutiveErrors >= maxConsecutiveErrors {
            a.addReaction(ctx, evt, "x")
            return fmt.Errorf("aborted after %d consecutive failures: %w", consecutiveErrors, err)
        }

        // Inject error as context so the model knows what happened
        messages = append(messages, provider.Message{
            Role:    "user",
            Content: fmt.Sprintf("[System] The previous turn failed: %s. Adjust your approach and try again.", err),
        })
        continue
    }

    consecutiveErrors = 0 // reset on success
    // ... rest of existing tool execution logic
}
```

### 2b. Reflection checkpoint every 5 turns

**File:** `internal/session/agent.go` — inside the turn loop, after successful turn

Ralph's reflection pattern: inject a structured prompt that forces the agent to assess progress. In Ralph this is configurable via `reflectEvery`. Fordjent hardcodes 5 (matching the AGENTS.md "reflection every 5 iterations" requirement).

**After `consecutiveErrors = 0` reset:**
```go
// Reflection checkpoint (Ralph loop pattern)
if turn > 0 && turn%5 == 0 {
    messages = append(messages, provider.Message{
        Role: "user",
        Content: `[System] REFLECTION CHECKPOINT

Pause and reflect on your progress:
1. What has been accomplished so far?
2. What's working well?
3. What's not working or blocking progress?
4. Should the approach be adjusted?
5. What are the next priorities?

Update the issue comment with your reflection, then continue working.`,
    })
    slog.Info("reflection checkpoint injected", "session_key", a.sess.Key, "turn", turn)
}
```

### 2c. Stall detection

**File:** `internal/session/agent.go` — inside the turn loop

Ralph doesn't have explicit stall detection (it relies on `itemsPerIteration` pacing). Fordjent needs it because the agent can get stuck calling the same tool with the same args.

**Add before the turn loop:**
```go
type turnSignature struct {
    tools string // sorted comma-joined tool names + arg hashes
}

recentSigs := make([]turnSignature, 0, 3)
```

**After tool execution, before next iteration:**
```go
// Build signature of this turn's tool calls
sig := buildTurnSignature(result.Response.ToolCalls)
recentSigs = append(recentSigs, sig)
if len(recentSigs) > 3 {
    recentSigs = recentSigs[1:]
}
if len(recentSigs) == 3 && allSameSignature(recentSigs) {
    messages = append(messages, provider.Message{
        Role:    "user",
        Content: "[System] WARNING: Your last 3 turns performed identical actions. You may be stuck in a loop. Try a completely different approach, or describe the blocker and stop.",
    })
    slog.Warn("stall detected — identical tool calls in 3 consecutive turns",
        "session_key", a.sess.Key, "turn", turn)
}
```

**Helper functions:**
```go
func buildTurnSignature(calls []provider.ToolCall) turnSignature {
    var parts []string
    for _, tc := range calls {
        parts = append(parts, tc.Function.Name+"("+shortHash(tc.Function.Arguments)+")")
    }
    sort.Strings(parts)
    return turnSignature{tools: strings.Join(parts, ",")}
}

func shortHash(s string) string {
    h := sha256.Sum256([]byte(s))
    return hex.EncodeToString(h[:4])
}

func allSameSignature(sigs []turnSignature) bool {
    if len(sigs) < 2 {
        return false
    }
    for i := 1; i < len(sigs); i++ {
        if sigs[i].tools != sigs[0].tools {
            return false
        }
    }
    return true
}
```

### 2d. Expose retry count in TurnResult

**File:** `internal/agent/turn.go` (~line 91)

**Current:** `// TODO: expose retry count from provider client if needed.`

**Fix:** Add `RetryCount` field to `TurnResult`. In `provider/retry.go`, expose the attempt count from `RetryError`. In `TurnExecutor.Run`, capture it:
```go
type TurnResult struct {
    // ... existing fields
    RetryCount int // number of LLM retries this turn
}
```

In `provider/client.go`, return retry count from `RetryPolicy.Retry`:
```go
// Change Retry signature to return (attempts int, err error)
func (r RetryPolicy) Retry(ctx context.Context, fn func() error) (int, error) {
    // ... existing logic
    // return retryErr.Attempts, retryErr on failure
    // return attempt+1, nil on success
}
```

Then in `TurnExecutor.Run`:
```go
response, usage, err := te.llm.Chat(ctx, systemPrompt, messages, te.tools.Tools())
// ... after call:
result.RetryCount = retryCount // from Chat return
```

### 2e. Configurable reflection interval

**File:** `internal/config/config.go`

Add to `AgentConfig`:
```go
ReflectionInterval int `yaml:"reflection_interval"` // inject reflection prompt every N turns (0 = disabled)
```

Default: 5. Set to 0 to disable. Wire into agent loop.

### 2f. Tests

**File:** `internal/session/agent_test.go` (new or extend)

- Test: `buildTurnSignature` with identical calls returns same signature
- Test: `allSameSignature` with 3 identical returns true, with 2 identical + 1 different returns false
- Test: consecutive error counter resets on success
- Test: reflection prompt injected at turn 5, 10, 15

---

## Phase 3: Local Deployment (macOS, No Docker)

### 3a. Create `Makefile`

**File:** `Makefile` (new)

```makefile
.PHONY: build run test lint clean build-fj

build:
	mkdir -p bin
	go build -o bin/fordjent ./cmd/fordjent
	go build -o bin/fj ./cmd/fj

run: build
	./bin/fordjent -config fordjent.local.yaml

test:
	go test ./... -count=1 -timeout 60s

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/
```

### 3b. Create `deploy-local.sh`

**File:** `deploy-local.sh` (new)

```bash
#!/bin/bash
set -euo pipefail

# Local deployment for macOS/Linux — no Docker required
# Usage: ./deploy-local.sh [--clean]

echo "== Checking prerequisites..."
command -v go >/dev/null || { echo "Go not found. Install: brew install go"; exit 1; }

echo "== Building fordjent..."
mkdir -p bin
go build -o bin/fordjent ./cmd/fordjent
go build -o bin/fj ./cmd/fj

echo "== Checking config..."
CONFIG="${FORDJENT_CONFIG:-fordjent.local.yaml}"
if [ ! -f "$CONFIG" ]; then
    echo "$CONFIG not found. Copy from fordjent.yaml and edit."
    exit 1
fi

# Source env if .env exists
if [ -f .env ]; then
    set -a
    source .env
    set +a
fi

# Create work directory
WORKDIR="${FORDJENT_WORKDIR:-$HOME/.fordjent/work}"
mkdir -p "$WORKDIR"

echo "== Starting fordjent (config: $CONFIG, workdir: $WORKDIR)..."
exec ./bin/fordjent -config "$CONFIG" "$@"
```

### 3c. Create `fordjent.macos.yaml`

**File:** `fordjent.macos.yaml` (new)

Config template for local macOS development. Uses cloud LLM providers (no local Ollama), points to localhost or remote Forgejo, relaxed settings for dev.

```yaml
server:
  host: "127.0.0.1"
  port: 8080

webhook:
  secret: "${WEBHOOK_SECRET}"

forgejo:
  url: "${FORGEJO_URL}"
  token: "${FORGEJO_TOKEN}"
  admin_token: "${FORGEJO_ADMIN_TOKEN}"
  rate_limit: 30

agent:
  max_sessions: 5
  idle_timeout: "1h"
  workdir: "${HOME}/.fordjent/work"
  max_turns: 50
  max_turns_pm: 15
  max_turns_implementer: 40
  role_providers:
    pm: "fast"
    reviewer: "strong"
    implementer: "strong"
  fallback_provider: "fast"
  commit_prefix: "[fordjent]"
  context_window: 128000
  compaction_threshold: 0.85
  compaction_keep_turns: 8
  reflection_interval: 5
  enable_lifecycle: true
  enable_stale_gate: true
  enable_scaffold_detection: false
  enable_session_recovery: true
  enable_context_injection: true
  enable_auto_collaborator: false
  require_role_tag: true
  session_timeout: "30m"
  git_name: "Fordjent Dev"
  git_email: "fordjent-dev@local"

budget:
  enabled: false

providers:
  - name: "strong"
    api_base: "https://api.openai.com/v1"
    api_key: "${STRONG_API_KEY}"
    model: "gpt-4o"
    max_tokens: 16384
    request_timeout: "120s"
    max_retries: 3
    retry_base_delay: "2s"
    retry_max_delay: "30s"
    max_concurrent_llm_calls: 2
    tier: "strong"
    cost_per_1m_input_tokens: 2.50
    cost_per_1m_output_tokens: 10.00
  - name: "fast"
    api_base: "https://api.openai.com/v1"
    api_key: "${FAST_API_KEY}"
    model: "gpt-4o-mini"
    max_tokens: 8192
    request_timeout: "60s"
    max_retries: 5
    retry_base_delay: "2s"
    retry_max_delay: "30s"
    max_concurrent_llm_calls: 3
    tier: "fast"
    cost_per_1m_input_tokens: 0.15
    cost_per_1m_output_tokens: 0.60

events:
  - "issues"
  - "issue_comment"
  - "pull_request"
  - "pull_request_review_comment"

security:
  protected_branches: ["main", "master"]
  require_pr_for_workflows: true
  filter_agent_events: true

memory:
  enabled: true

database:
  path: ""

log_level: "info"
```

### 3d. Update `.env.example`

**File:** `.env.example` (rewrite)

```bash
# Fordjent environment variables
# Copy to .env and fill in values

# === Forgejo ===
FORGEJO_URL=http://localhost:3000
FORGEJO_TOKEN=your-forgejo-personal-access-token
FORGEJO_ADMIN_TOKEN=your-forgejo-admin-token

# === LLM Providers (OpenAI-compatible APIs) ===
# Strong model: high-quality for implementation and code review
STRONG_API_KEY=your-strong-model-api-key

# Fast model: cheap and quick for planning and simple tasks
FAST_API_KEY=your-fast-model-api-key

# === Webhook ===
WEBHOOK_SECRET=your-webhook-secret

# === Optional ===
LOG_LEVEL=info
FORDJENT_PORT=8080
FORDJENT_GIT_NAME="Fordjent Agent"
FORDJENT_GIT_EMAIL="fordjent@forgejo.local"
```

### 3e. Update README with local dev section

**File:** `README.md`

Add a section after Quick Start:

```markdown
## Local Development (macOS/Linux)

No Docker required. Requires Go 1.22+ and a running Forgejo instance.

### Quick setup

```bash
# 1. Install Go (if not installed)
brew install go

# 2. Copy and edit config
cp fordjent.macos.yaml fordjent.local.yaml
# Edit fordjent.local.yaml: set API keys, Forgejo URL

# 3. Copy and fill env
cp .env.example .env
# Edit .env: set STRONG_API_KEY, FAST_API_KEY, FORGEJO_TOKEN, WEBHOOK_SECRET

# 4. Build and run
make run

# Or just build:
make build
./bin/fordjent -config fordjent.local.yaml
```

### Forgejo setup (local, via Docker)

If you don't have Forgejo running:

```bash
docker run -d --name forgejo-local \
  -p 127.0.0.1:3000:3000 \
  -v forgejo-data:/data \
  -e USER_UID=$(id -u) \
  codeberg.org/forgejo/forgejo:9
```

Then visit http://localhost:3000 to complete setup.

### Using the `fj` CLI

```bash
# Build the CLI
go build -o bin/fj ./cmd/fj

# Detect repo from git remote
./bin/fj detect

# List issues
./bin/fj issue list

# Create a webhook
./bin/fj hook create --url http://localhost:8080/acp/v1/events --secret your-secret
```
```

---

## Phase 4: Strong/Fast Model Config

### 4a. Add `Tier` and `FallbackProvider` to config

**File:** `internal/config/config.go`

Add to `ProviderConfig`:
```go
Tier string `yaml:"tier"` // "strong" or "fast" (default: "strong")
```

Add to `AgentConfig`:
```go
FallbackProvider string `yaml:"fallback_provider"` // provider name to try if primary fails
```

Add validation in `validate()`:
```go
if c.Agent.FallbackProvider != "" {
    found := false
    for _, p := range c.Providers {
        if p.Name == c.Agent.FallbackProvider {
            found = true
            break
        }
    }
    if !found {
        errs = append(errs, fmt.Sprintf("agent.fallback_provider %q not found in providers", c.Agent.FallbackProvider))
    }
}
for i, p := range c.Providers {
    if p.Tier != "" && p.Tier != "strong" && p.Tier != "fast" {
        errs = append(errs, fmt.Sprintf("providers[%d].tier must be 'strong' or 'fast', got %q", i, p.Tier))
    }
}
```

### 4b. Create FallbackClient

**File:** `internal/provider/fallback.go` (new)

```go
package provider

import (
    "context"
    "log/slog"

    "github.com/fordjent/fordjent/internal/config"
)

// FallbackClient wraps a primary and fallback provider. If the primary
// fails with a retryable error after all retries, the fallback is tried.
type FallbackClient struct {
    primary  *Client
    fallback *Client
}

func NewFallbackClient(primary, fallback *Client) *FallbackClient {
    return &FallbackClient{primary: primary, fallback: fallback}
}

func (fc *FallbackClient) Chat(ctx context.Context, systemPrompt string, messages []Message, tools []ToolDef) (*Response, *Usage, error) {
    resp, usage, err := fc.primary.Chat(ctx, systemPrompt, messages, tools)
    if err == nil {
        return resp, usage, nil
    }

    // Only fall back on retryable errors
    var httpErr *HTTPError
    if errors.As(err, &httpErr) && httpErr.StatusCode >= 500 {
        slog.Warn("primary provider failed, falling back",
            "primary", fc.primary.Cfg().Name,
            "fallback", fc.fallback.Cfg().Name,
            "error", err,
        )
        return fc.fallback.Chat(ctx, systemPrompt, messages, tools)
    }

    var retryErr *RetryError
    if errors.As(err, &retryErr) {
        slog.Warn("primary provider exhausted retries, falling back",
            "primary", fc.primary.Cfg().Name,
            "fallback", fc.fallback.Cfg().Name,
            "attempts", retryErr.Attempts,
        )
        return fc.fallback.Chat(ctx, systemPrompt, messages, tools)
    }

    return nil, nil, err
}

func (fc *FallbackClient) Cfg() *config.ProviderConfig {
    return fc.primary.Cfg()
}

// FallbackCfg returns the fallback provider config (for cost tracking).
func (fc *FallbackClient) FallbackCfg() *config.ProviderConfig {
    return fc.fallback.Cfg()
}
```

### 4c. Wire fallback into agent construction

**File:** `internal/session/agent.go` — `NewAgent()` (~line 41)

Current:
```go
prov := cfg.ProviderForRole(role)
llmClient := provider.NewClient(prov)
```

New:
```go
prov := cfg.ProviderForRole(role)
llmClient := provider.NewClient(prov)

// Wrap with fallback if configured
if cfg.Agent.FallbackProvider != "" {
    fallbackProv := cfg.ProviderByName(cfg.Agent.FallbackProvider)
    if fallbackProv != nil && fallbackProv.Name != prov.Name {
        fallbackClient := provider.NewClient(fallbackProv)
        llmClient = provider.NewFallbackClient(llmClient, fallbackClient)
    }
}
```

**File:** `internal/config/config.go` — add helper:
```go
func (c *Config) ProviderByName(name string) *ProviderConfig {
    for _, p := range c.Providers {
        if p.Name == name {
            return &p
        }
    }
    return nil
}
```

### 4d. Update provider client interface

**File:** `internal/provider/client.go`

The `FallbackClient` needs to satisfy the same interface as `Client`. Currently `Chat` returns `(*Response, *Usage, error)`. Make sure `FallbackClient` matches.

If `TurnExecutor` holds `*provider.Client` directly, change the field type to an interface:
```go
type ChatCompleter interface {
    Chat(ctx context.Context, systemPrompt string, messages []Message, tools []ToolDef) (*Response, *Usage, error)
    Cfg() *config.ProviderConfig
}
```

Then `TurnExecutor.llm` becomes `ChatCompleter`. Both `Client` and `FallbackClient` satisfy it.

### 4e. Update fordjent.local.yaml

**File:** `fordjent.local.yaml`

Add `tier` to each provider, add `fallback_provider` to agent section. See Phase 3c for the full config shape.

---

## Phase 5: Issue Lifecycle FSM

Label-driven FSM. Human labels move issues through states. Agents can also label (future). States derive from label sets.

### 5a. Define FSM states

**File:** `internal/lifecycle/fsm.go` (new)

```go
package lifecycle

import "strings"

// IssueState represents the lifecycle state of an issue.
type IssueState string

const (
    StateOpened       IssueState = "opened"
    StateNeedsRole    IssueState = "needs-role"
    StateReady        IssueState = "ready"
    StatePlanning     IssueState = "planning"
    StatePlanApproved IssueState = "plan-approved"
    StateImplementing IssueState = "implementing"
    StateBlocked      IssueState = "blocked"
    StateReview       IssueState = "review"
    StateMerging      IssueState = "merging"
    StateDone         IssueState = "done"
)

// statePriority defines the priority order when multiple labels match.
// Higher priority wins.
var statePriority = map[IssueState]int{
    StateDone:         100,
    StateMerging:      90,
    StateBlocked:      80,
    StateReview:       70,
    StateImplementing: 60,
    StatePlanApproved: 50,
    StatePlanning:     40,
    StateReady:        30,
    StateNeedsRole:    20,
    StateOpened:       0,
}

// labelToState maps Forgejo labels to FSM states.
var labelToState = map[string]IssueState{
    "done":           StateDone,
    "automerge":      StateMerging,
    "blocked":        StateBlocked,
    "review":         StateReview,
    "implementing":   StateImplementing,
    "plan-approved":  StatePlanApproved,
    "planning":       StatePlanning,
    "ready":          StateReady,
    "needs-role":     StateNeedsRole,
}

// allowedTransitions defines legal state transitions.
var allowedTransitions = map[IssueState][]IssueState{
    StateOpened:       {StateNeedsRole, StateReady, StatePlanning, StateBlocked},
    StateNeedsRole:    {StateReady, StatePlanning, StateBlocked},
    StateReady:        {StatePlanning, StateImplementing, StateBlocked},
    StatePlanning:     {StatePlanApproved, StateBlocked, StateDone},
    StatePlanApproved: {StateImplementing, StateBlocked},
    StateImplementing: {StateReview, StateBlocked, StateDone},
    StateBlocked:      {StateReady, StatePlanning, StateImplementing, StateReview, StateDone},
    StateReview:       {StateImplementing, StateMerging, StateDone, StateBlocked},
    StateMerging:      {StateDone, StateReview, StateBlocked},
    StateDone:         {StateReady, StateImplementing}, // reopen
}

// StateFromLabels derives the current state from an issue's label set.
func StateFromLabels(labels []string) IssueState {
    best := StateOpened
    bestPri := 0

    for _, label := range labels {
        name := strings.ToLower(strings.TrimSpace(label))
        if state, ok := labelToState[name]; ok {
            if pri := statePriority[state]; pri > bestPri {
                best = state
                bestPri = pri
            }
        }
    }
    return best
}

// IsTransitionValid checks whether a state transition is allowed.
func IsTransitionValid(from, to IssueState) bool {
    allowed, ok := allowedTransitions[from]
    if !ok {
        return false
    }
    for _, s := range allowed {
        if s == to {
            return true
        }
    }
    return false
}
```

### 5b. Wire FSM into session manager

**File:** `internal/session/manager.go` — `handleEvent()`

When `evt.Type == event.IssueLabelUpdated`:
1. Fetch the issue's current labels via `forgejo.GetIssue()`
2. Call `lifecycle.StateFromLabels(labels)` to get new state
3. Log the state transition to lifecycle DB
4. If transition triggers session action, handle it

```go
if evt.Type == event.IssueLabelUpdated && evt.IssueNumber > 0 {
    issue, err := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.IssueNumber)
    if err == nil && issue != nil {
        labelNames := make([]string, len(issue.Labels))
        for i, l := range issue.Labels {
            labelNames[i] = l.Name
        }
        newState := lifecycle.StateFromLabels(labelNames)
        slog.Info("issue state transition",
            "issue", evt.IssueNumber,
            "new_state", newState,
            "labels", labelNames,
        )
        // Trigger session actions based on state
        switch newState {
        case lifecycle.StatePlanApproved:
            // Remove needs-role if present, spawn implementer session
            // (existing handleRoleAssignment logic)
        case lifecycle.StateMerging:
            // Spawn reviewer agent with automerge directive
            // (Phase 6 logic)
        case lifecycle.StateDone:
            // Close issue if not already closed
        }
    }
}
```

### 5c. Expand EnsureLabels

**File:** `internal/forgejo/client.go` — `EnsureLabels()` (~line 379)

Add the new FSM labels:
```go
{ Name: "planning",      Color: "#1d76db" }, // blue
{ Name: "plan-approved", Color: "#0e8a16" }, // green
{ Name: "implementing",  Color: "#5319e7" }, // purple
{ Name: "review",        Color: "#fbca04" }, // yellow
{ Name: "automerge",     Color: "#0e8a16" }, // green
{ Name: "done",          Color: "#ededed" }, // light gray
```

### 5d. Update agent behavior per state

**File:** `internal/session/agent.go` — `buildSystemPrompt()`

Add state-aware instructions:
```go
// In buildSystemPrompt, after role-specific instructions:
issueState := a.detectIssueState(ctx, evt)
switch issueState {
case lifecycle.StatePlanning:
    modeInstructions += `
## STATE: Planning
This issue is in planning mode. You MUST:
1. Read and understand the codebase
2. Propose a concrete implementation plan
3. Break into sub-issues if needed
4. Post a summary comment
5. STOP — do not write code`
case lifecycle.StateImplementing:
    modeInstructions += `
## STATE: Implementing
This issue is approved for implementation. Proceed with coding.`
case lifecycle.StateBlocked:
    modeInstructions += `
## STATE: Blocked
This issue is blocked. Check the issue body for 'Depends on: #N' to understand what's blocking it. Post a comment explaining the current blocker.`
}
```

Add helper:
```go
func (a *Agent) detectIssueState(ctx context.Context, evt *event.Event) lifecycle.IssueState {
    if evt.IssueNumber == 0 {
        return lifecycle.StateOpened
    }
    issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
    if err != nil || issue == nil {
        return lifecycle.StateOpened
    }
    labelNames := make([]string, len(issue.Labels))
    for i, l := range issue.Labels {
        labelNames[i] = l.Name
    }
    return lifecycle.StateFromLabels(labelNames)
}
```

### 5e. Tests

**File:** `internal/lifecycle/fsm_test.go` (new)

```go
func TestStateFromLabels(t *testing.T) {
    tests := []struct {
        labels []string
        want   IssueState
    }{
        {nil, StateOpened},
        {[]string{"needs-role"}, StateNeedsRole},
        {[]string{"ready"}, StateReady},
        {[]string{"blocked", "ready"}, StateBlocked}, // blocked wins (higher priority)
        {[]string{"implementing"}, StateImplementing},
        {[]string{"review", "automerge"}, StateMerging}, // automerge wins
        {[]string{"done"}, StateDone},
    }
    for _, tt := range tests {
        got := StateFromLabels(tt.labels)
        if got != tt.want {
            t.Errorf("StateFromLabels(%v) = %q, want %q", tt.labels, got, tt.want)
        }
    }
}

func TestIsTransitionValid(t *testing.T) {
    if !IsTransitionValid(StateReady, StateImplementing) {
        t.Error("ready -> implementing should be valid")
    }
    if IsTransitionValid(StateDone, StatePlanning) {
        t.Error("done -> planning should be invalid (use reopen)")
    }
}
```

---

## Phase 6: /automerge + Reviewer Auto-merge

### 6a. Register PullRequestLabelUpdated event

**File:** `internal/event/event.go`

Add:
```go
PullRequestLabelUpdated Type = "pull_request.label_updated"
```

**File:** `internal/webhook/router.go` — `normalizeEvent()`

Ensure `pull_request` events with `action: "labeled"` are handled:
```go
case "pull_request":
    typ = event.Type("pull_request." + action)
```

This already works — `"labeled"` becomes `"pull_request.labeled"` which matches the new event type.

### 6b. Handle automerge label in session manager

**File:** `internal/session/manager.go` — `handleEvent()`

```go
if evt.Type == event.PullRequestLabelUpdated && evt.PRNumber > 0 {
    // Check if the PR has the automerge label
    issue, err := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.PRNumber)
    if err == nil && issue != nil {
        hasAutomerge := false
        for _, l := range issue.Labels {
            if l.Name == "automerge" {
                hasAutomerge = true
                break
            }
        }
        if hasAutomerge {
            // Create a synthetic event to spawn a reviewer session
            synthEvt := event.NewEvent(
                event.IssueCommentCreated,
                evt.Repository,
                evt.IssueNumber,
                evt.PRNumber,
                "automerge-trigger",
                "created",
            )
            synthEvt.SessionKey = fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber)
            synthEvt.Payload = map[string]interface{}{
                "comment": map[string]interface{}{
                    "body": "[System] This PR has the 'automerge' label. Review the code and merge if it passes all checks.",
                },
            }
            m.handleEvent(ctx, synthEvt)
        }
    }
}
```

### 6c. Update reviewer system prompt for automerge

**File:** `internal/session/agent.go` — `buildSystemPrompt()`

In the `case "reviewer":` block, add:
```go
// Check for automerge label
hasAutomerge := false
if evt.IssueNumber > 0 {
    issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
    if err == nil && issue != nil {
        for _, l := range issue.Labels {
            if l.Name == "automerge" {
                hasAutomerge = true
                break
            }
        }
    }
}
if hasAutomerge {
    modeInstructions += `
- This PR has the 'automerge' label. Review the diff, verify build and tests pass.
- If the code is correct and there are no conflicts, call forgejo_merge_pr immediately.
- If issues are found, post a comment describing them and remove the 'automerge' label.`
}
```

### 6d. Add `pull_request.label_updated` to default events

**File:** `internal/config/config.go` — default events slice

```go
Events: []string{"issues", "issue_comment", "pull_request", "pull_request_review_comment"},
```

This already includes `"pull_request"` which covers all PR sub-actions (opened, labeled, closed, etc.). No change needed.

### 6e. Tests

- Test: PR labeled with `automerge` triggers reviewer session
- Test: Reviewer with automerge calls `forgejo_merge_pr` when code is clean
- Test: Reviewer with automerge removes label and posts comment when code has issues

---

## Appendix: Ralph Loop Patterns Reference

Source: [pi-ralph-wiggum/index.ts](https://github.com/tmustier/pi-extensions/blob/main/pi-ralph-wiggum/index.ts)

| Ralph Concept | Ralph Implementation | Fordjent Adaptation |
|---|---|---|
| `reflectEvery: N` | Modulo check on iteration count: `(iteration - 1) % reflectEvery === 0` | `turn % 5 == 0` in agent loop (configurable via `reflection_interval`) |
| `maxIterations` | Hard stop, loop completes with banner | Already exists as `max_turns` / `max_turns_implementer` |
| Completion marker | `<promise>COMPLETE</promise>` in assistant text | Fordjent: agent returns no tool calls → session complete |
| `ralph_done` tool | Agent self-invokes to advance iteration | Not needed — Fordjent's for-loop is continuous |
| `itemsPerIteration` | Soft prompt hint: "process ~N items this turn" | Could add as system prompt hint, deferred |
| State persistence | JSON file per loop in `.ralph/` | Fordjent: SQLite session store + lifecycle DB |
| Pause/resume | Manual `/ralph stop` + `/ralph resume` | Fordjent: session timeout + `enable_session_recovery` |
| Prompt injection | `before_agent_start` event adds loop context to system prompt | Fordjent: reflection prompt injected as user message at turn N |
| Stall detection | Not implemented in Ralph (relies on `itemsPerIteration` pacing) | Fordjent: track last 3 turn signatures, warn if identical |
| Error resilience | Not implemented in Ralph (interactive — user sees error and redirects) | Fordjent: consecutive error counter, continue up to 3 failures |
| Task file | Markdown checklist the agent updates | Fordjent: issue body + comments serve as the task file |

The key insight: Ralph is interactive (user drives the loop, agent calls `ralph_done` to advance). Fordjent is autonomous (webhook triggers, agent runs to completion). Fordjent's adaptation focuses on the resilience patterns (reflection, stall detection, error recovery) rather than the interactive loop control.

---

## Appendix: Label Summary After All Phases

| Label | Color | Created By | FSM State | Meaning |
|---|---|---|---|---|
| `needs-role` | gray | session manager | `needs-role` | Issue needs a role label |
| `ready` | green | scheduler / human | `ready` | Dependencies met, ready to work |
| `planning` | blue | human | `planning` | Agent should plan, not implement |
| `plan-approved` | green | human | `plan-approved` | Plan reviewed, proceed to implementation |
| `implementing` | purple | agent / human | `implementing` | Active implementation in progress |
| `blocked` | red | lifecycle / scheduler / agent | `blocked` | Blocked by dependency or merge queue |
| `review` | yellow | agent / human | `review` | PR created, needs review |
| `automerge` | green | human | `merging` | PR approved for auto-merge |
| `done` | gray | agent / human | `done` | Issue completed |
| `scaffold` | yellow | scaffold detector | N/A | Repo needs project scaffold |
| `fordjent/failed:max-turns` | dark red | lifecycle | N/A | Session exhausted turn budget |
| `fordjent/failed:error` | purple | lifecycle | N/A | Session died from runtime error |
| `role:pm` | — | human | N/A | Triggers PM role |
| `role:reviewer` | — | human | N/A | Triggers reviewer role |
| `role:implementer` | — | human | N/A | Triggers implementer role |
| `role:devops` | — | human | N/A | Triggers devops role |
| `role:tester` | — | human | N/A | Triggers tester role |
