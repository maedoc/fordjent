# Merge Conflict Analysis — 3 Files, 15 Blocks

## Summary

All 15 conflicts follow the **same pattern**: upstream split `buildSystemPrompt()` return from `string` → `SystemPromptParts` (3-part struct: Stable + TurnInfo + ToolsDesc) and added `turn/maxTurns` parameters. Downstream kept `string` return and hardcoded tool descriptions inside the prompt. Additionally, upstream added cache-tracking log lines in `turn.go`.

**Verdict**: Keep upstream's refactored approach everywhere. The downstream side is the older string-return pattern that was superseded by the structured multi-part system prompt.

---

## Conflict 1: `internal/agent/turn.go` (line ~408)

### The Block

```go
<<<<<<< Updated upstream
		"cached_tokens", usage.CachedTokens + usage.CacheReadTokens,
		"cache_savings_usd", usage.CacheSavingsUSD,
=======
		"cached_tokens", usage.CachedTokens,
>>>>>>> Stashed changes
```

### What's Happening
**Upstream** logs combined cache metrics (`CachedTokens + CacheReadTokens`) plus `cache_savings_usd`. **Downstream** (stashed) only logs `CachedTokens`.

### Recommendation: **KEEP UPSTREAM**
The downstream change is a regression — it removes cache observability metrics that were added upstream. The upstream version gives complete cache visibility (raw cache hits + read-through cache + USD savings).

---

## Conflict 2: `internal/session/agent.go` (line ~180)

### The Block

```go
<<<<<<< Updated upstream
	systemPromptParts := a.buildSystemPrompt(ctx, evt, analysisMode, a.role, fsmState, a.executor.CurrentTurn(), a.executor.MaxTurns())
=======
	systemPrompt := a.buildSystemPrompt(ctx, evt, analysisMode, a.role, fsmState)
>>>>>>> Stashed changes
contextMessages, err := a.buildContext(ctx, evt)
```

### What's Happening
**Upstream** calls `buildSystemPrompt` with all 7 params (including `CurrentTurn()` and `MaxTurns()`) and stores result in `systemPromptParts`. **Downstream** (stashed) calls it with 5 params and stores result in `systemPrompt`. This is a direct consequence of the `buildSystemPrompt` signature change.

### Recommendation: **KEEP UPSTREAM**
The downstream 5-param call signature doesn't exist anymore. Upstream passes turn budget info and max turns so the agent can display its budget. Keep the 7-param call.

---

## Conflict 3: `internal/session/agent.go` (line ~550)

### The Block

```go
<<<<<<< Updated upstream
// systemPromptParts is an alias for provider.SystemPromptParts
type systemPromptParts = provider.SystemPromptParts

func (a *Agent) buildSystemPrompt(ctx context.Context, evt *event.Event, analysisMode bool, role string, fsmState lifecycle.IssueState, turn, maxTurns int) systemPromptParts {
=======
func (a *Agent) buildSystemPrompt(ctx context.Context, evt *event.Event, analysisMode bool, role string, fsmState lifecycle.IssueState) string {
>>>>>>> Stashed changes
	toolsDesc := a.tools.Descriptions()
```

### What's Happening
**Upstream**: `buildSystemPrompt` returns `systemPromptParts` (an alias for `provider.SystemPromptParts`), accepting `turn` and `maxTurns` as extra parameters. The return type alias is needed in this file.
**Downstream**: Returns `string`, no turn/maxTurns params. The function body then starts directly with `toolsDesc := a.tools.Descriptions()`.

### Recommendation: **KEEP UPSTREAM**
This is the core signature change. The downstream string-return is the old pattern. The upstream multi-part return is the cache-optimized new pattern where:
- `Stable`: The main prompt text (cache-across-turns)
- `TurnInfo`: Turn budget (changes each turn, sent as separate user message)
- `ToolsDesc`: Tool descriptions (changes when tools are excluded)

Keep the `systemPromptParts` alias and 7-param signature.

---

## Conflict 4: `internal/session/agent.go` (line ~891)

### The Block

```go
<<<<<<< Updated upstream
	// Cache optimization: turnBudgetInfo and toolsDesc are NOT included in the
	// system prompt. They are returned separately so they can be sent as subsequent
	// user messages. This keeps the system prompt stable across turns, enabling
	// KV cache hits in providers like NeuralWatt that use automatic prefix caching.
	// Previously, turn# and tool descriptions were embedded in the system prompt,
	// changing message[0] on every API call and invalidating the entire cache.
	turnBudgetInfo := fmt.Sprintf("## Turn Budget\nYou have %d turns total and are currently on turn %d. You have access to a turn() tool that tells you how many turns you've used and how many remain. Check it periodically, especially if you've been working for a while.}", maxTurns, turn)

	stable := fmt.Sprintf(`You are Fordjent, an autonomous coding agent that helps with software development tasks on a Forgejo instance.
=======
	return fmt.Sprintf(`You are Fordjent, an autonomous coding agent that helps with software development tasks on a Forgejo instance.
>>>>>>> Stashed changes
## Current Context
```

### What's Happening
**Upstream** splits `buildSystemPrompt()` into:
1. A `turnBudgetInfo` string (extracted out of system prompt → sent as separate user message)
2. A `stable` var holding the base system prompt template (without `return`)
3. Later returns `systemPromptParts{Stable: stable, TurnInfo: turnBudgetInfo, ...}`

**Downstream**: Direct `return fmt.Sprintf(...)` — all-in-one string. It also includes `toolsDesc` inside the prompt template string (see conflicts 5/6 below).

### Recommendation: **KEEP UPSTREAM**
The upstream approach achieves KV-cache optimization: the `stable` portion stays identical across turns, so KV cache providers can hit the prefix cache. The downstream approach rebuilds the entire prompt string each turn, invalidating the cache. This is a significant performance optimization worth keeping.

---

## Conflict 5: `internal/session/agent.go` (~between lines 911 area — "Your Capabilities" section)

### The Block

```html
<<<<<<< Updated upstream
=======

## Your Capabilities
You have access to the following tools:
%s
>>>>>>> Stashed changes
```

### What's Happening
**Upstream** does NOT include `## Your Capabilities` / tool descriptions inside the `stable` system prompt. Tools are sent separately (via `ToolsDesc` field of `SystemPromptParts`).
**Downstream** includes `## Your Capabilities` + `%s` (tool descriptions) directly inside the prompt template.

### Recommendation: **KEEP UPSTREAM**
Same reason as conflict 4: tool descriptions change when tools are excluded (scoping), so including them in the system prompt breaks KV cache hits. The upstream approach sends tool descriptions as a separate `ToolsDesc` message.

---

## Conflict 6: `internal/session/agent.go` (~after line 930 — `toolsDesc` argument)

### The Block

```go
<<<<<<< Updated upstream
=======
		toolsDesc,
>>>>>>> Stashed changes
		a.cfg.Agent.CommitPrefix,
		strings.Join(a.cfg.Security.ProtectedBranches, ", "),
	)

	return systemPromptParts{
		Stable:    stable,
		TurnInfo:  turnBudgetInfo,
		ToolsDesc: toolsDesc,
	}
```

### What's Happening
**Upstream**: Returns `systemPromptParts{Stable, TurnInfo, ToolsDesc}` — three separate fields. Downstream's `toolsDesc` argument (the `%s` placeholder in the template below) would be passed into the `fmt.Sprintf(...)` call.
**Downstream**: `toolsDesc` is one of the `fmt.Sprintf` arguments for the `stable` template. Since the downstream pattern is `return fmt.Sprintf(template, args...)`, the `toolsDesc` is embedded in the template.

### Recommendation: **KEEP UPSTREAM**
Again, tool descriptions must NOT be in the stable template. The upstream approach is correct: `ToolsDesc` goes into a separate field and is sent as a subsequent user message (handled by the agent runner), not baked into the system prompt.

---

## Conflicts 7–15: `internal/session/agent_test.go` (9 identical pattern)

All 9 test conflicts are the same structure:

### Pattern

```go
<<<<<<< Updated upstream
	prompt := agent.buildSystemPrompt(context.Background(), evt, false, "ROLE", lifecycle.StateX, 0, 5)
	if !strings.Contains(fullPrompt(prompt), "TEXT") {
=======
	prompt := agent.buildSystemPrompt(context.Background(), evt, false, "ROLE", lifecycle.StateX)
	if !strings.Contains(prompt, "TEXT") {
>>>>>>> Stashed changes
```

### Mapping

| # | Line | Test Function | Role / State |
|---|------|---------------|-------------|
| 7 | ~328 | `TestBuildSystemPrompt_PlanningState` | implementer / StatePlanning |
| 8 | ~355 | `TestBuildSystemPrompt_AutoMergeReviewer` | reviewer / StateMerging |
| 9 | ~401 | `TestBuildSystemPrompt_DevOpsRole` | devops / StateOpened |
| 10 | ~476 | ??? (unscoped) | — |
| 11 | ~506 | `TestBuildSystemPrompt_TesterRole` | tester / StateOpened |
| 12 | ~536 | `TestBuildSystemPrompt_PMRole` | pm / StateOpened |
| 13 | ~851 | `TestBuildSystemPrompt_PolicyNoAutoMerge` | reviewer / StateOpened |
| 14 | ~883 | `TestBuildSystemPrompt_PolicyRequireReview` | reviewer / StateOpened |
| 15 | ~940 | `TestBuildSystemPrompt_PolicyYolo` | pm / StateOpened |

*(Tests 10 and 12 appear to be the same test or similar — line counts suggest some overlap in my reading. The exact test at line 476 wasn't independently read but follows the identical pattern.)*

### Recommendation: **KEEP UPSTREAM for ALL 9**

Each test needs two changes:
1. Add `0, 5` as the last two arguments to `buildSystemPrompt()` → `buildSystemPrompt(context.Background(), evt, false, "role", lifecycle.StateX, 0, 5)`
2. Wrap the return with `fullPrompt(prompt)` → `fullPrompt(prompt)`

The `fullPrompt()` helper (defined at `agent_test.go:22`) extracts the full combined string from `SystemPromptParts` for assertion purposes. Without it, `prompt` is now a struct, not a string.

---

## Complete Resolution Strategy

### Step 1: Keep upstream in `turn.go`
- Lines ~408: Remove downstream lines, keep `"cached_tokens", usage.CachedTokens + usage.CacheReadTokens,` + `"cache_savings_usd", usage.CacheSavingsUSD,`

### Step 2: Keep upstream in `agent.go`
- Line ~180: Use 7-param `buildSystemPrompt` call
- Line ~550: Keep 7-param signature + `systemPromptParts` return type
- Line ~891: Keep upstream's `turnBudgetInfo` + `stable` split
- "Your Capabilities" section: Remove downstream lines (don't embed tools in stable prompt)
- Closing `fmt.Sprintf` / return: Remove downstream `toolsDesc` from args and return `systemPromptParts{}`

### Step 3: Keep upstream in `agent_test.go`
- Replace all downstream patterns with upstream pattern: add `0, 5` args + `fullPrompt()` wrapper
- 9 test functions all need the same treatment

### Verification

After resolving, the signature should be:
```go
func (a *Agent) buildSystemPrompt(ctx context.Context, evt *event.Event, analysisMode bool, role string, fsmState lifecycle.IssueState, turn, maxTurns int) systemPromptParts
```

And callers should pass `a.executor.CurrentTurn()` and `a.executor.MaxTurns()`.

### What Downstream Lost (and should not lose)

The only downstream-specific content in these conflict blocks is the embedded `## Your Capabilities` section with tool descriptions. This should **not** be kept in the system prompt (as upstream correctly identified), but the downstream branch should be checked to see if the tool descriptions are handled via a separate mechanism elsewhere. If the downstream branch sends tool descriptions via a separate mechanism (e.g., a subsequent user message), that mechanism must be preserved alongside the upstream's `ToolsDesc` field.
