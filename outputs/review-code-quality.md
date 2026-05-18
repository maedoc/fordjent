# Code Quality Review — Fordjent

**Reviewer**: review-tuna  
**Date**: 2026-05-18  
**Scope**: cmd/fj/main.go, internal/tool/forgejo_tools.go, internal/tool/local_tools.go, internal/forgejo/client.go, internal/session/manager.go, internal/session/agent.go, internal/webhook/router.go, internal/sentinel/sentinel.go, internal/provider/retry.go, internal/stalegate/stalegate.go, internal/memory/memory.go, internal/provider/client.go

---

## 1. Error Handling Patterns

### BUG: `fmt.Sscanf` silently ignores parse failures (cmd/fj/main.go)

**Lines affected**: 369, 409, 422, 435, 484, 507, 555, 568, 611, 635, 754, 793, 800, 808, 827, 832

Every `fmt.Sscanf(args[N], "%d", &num)` call silently returns 0 if the argument is non-numeric. For example at line 369:

```go
var num int
fmt.Sscanf(args[1], "%d", &num)
```

If `args[1]` is `"abc"` or missing, `num` stays 0, which would act on issue/PR #0. **Fix**: use `strconv.Atoi` and handle the error:

```go
num, err := strconv.Atoi(args[1])
if err != nil {
    fmt.Fprintln(os.Stderr, "Error: invalid number:", args[1])
    os.Exit(1)
}
```

### WARNING: Errors returned as success strings (internal/tool/local_tools.go:145)

The bash tool converts execution errors into successful result strings:

```go
if err != nil {
    return fmt.Sprintf("Exit error: %s\n%s", err, output), nil
}
```

This prevents callers from using `errors.Is`/`errors.As` to detect failures. It's a deliberate design choice (don't kill agent on failed `rm`), but it means the compiler exit code 1 from `go build` or `go test` is opaque to tool callers. The agent gets the message as a tool result string, not as an error it can branch on.

### BUG: Silent error drops in block/unblock paths (internal/session/agent.go:307-308, manager.go:300)

When a session is blocked by the merge queue, both `PostIssueComment` and `AddIssueLabels` errors are silently dropped:

```go
_ = a.forgejo.PostIssueComment(ctx, evt.Repository, evt.IssueNumber, body)
_ = a.forgejo.AddIssueLabels(ctx, evt.Repository, evt.IssueNumber, []string{"blocked"})
```

If the bot has lost write permission (token expired, user removed from repo), these POSTs fail silently and the blocking rationale is never visible to the user. **Fix**: at minimum log the error.

Similarly in `manager.go:300`:

```go
_ = m.forgejoClient.PostIssueComment(schedCtx, evt.Repository, b.IssueNumber, body)
```

### BUG: `AddIssueLabels` has nested error swallowing (internal/forgejo/client.go:276-296)

The dedup-and-create-label logic has three levels of silent error handling:

- Line 276: `createErr := c.CreateLabel(...)` — error captured but never logged or returned; flow continues
- Line 288: `existing3, _ := c.ListLabels(ctx, repo)` — error from ListLabels explicitly discarded with `_`
- Lines 277-286: If `CreateLabel` fails, the code falls back to re-listing labels, but the fallback path also silently discards errors

If the Forgejo API is flaky during label creation, the function silently does nothing and returns `nil` (line 298: `if len(ids) == 0 { return nil }`). The caller thinks labels were added when they weren't.

### WARNING: `json.MarshalIndent` errors silently dropped

Scattered across `forgejo_tools.go` (lines 901, 992, 1156, 1205, 1249, 1310, 1407):

```go
result, _ := json.MarshalIndent(branches, "", "  ")
return string(result), nil
```

If marshaling fails (e.g., circular reference, type panic), this returns empty string with no error. In practice these are well-known types, but `json.Marshal` can panic on certain types (e.g., `chan`, `func`).

### WARNING: Dead variable assignment (internal/memory/memory.go:67)

```go
_ = noteMsg
```

`noteMsg` is constructed on line 63 but never used — only the side effect of the `git notes add` command matters. This is a dead code smell (the variable was probably intended for the git notes message).

---

## 2. Concurrency Correctness

### BUG: `manager.go` goroutines create independent contexts (lines 274, 312)

The `scheduler.OnPRMerged` and push handler goroutines use `context.WithTimeout(context.Background(), ...)` instead of propagating the parent context:

```go
go func() {
    schedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    ...
}()
```

If the manager shuts down, these goroutines still run to completion (30s). During shutdown storm (e.g., many PRs merged simultaneously), goroutines accumulate without bound. **Impact**: memory pressure on shutdown+restart cycles. **Fix**: pass parent `ctx` (the manager's Run context) so cancellation propagates.

### RACE CONDITION: `reapIdle` TOCTOU on `sess.busy` (manager.go:747-765)

The reaper checks `sess.busy` under the session mutex, unlocks, then uses the value:

```go
sess.mu.Lock()
busy := sess.busy
sess.mu.Unlock()
if busy {
    continue
}
```

Between the unlock and the `if busy` check, the session could start processing a new event. The window is small, but the result is that an active session could be reaped. **Mitigation**: the reaper runs every 1 minute and the busy flag flips quickly, so this is low probability. **Fix**: check `sess.LastActive` instead (already a reliable staleness indicator).

### GOROUTINE LEAK: Restored session resume goroutines (manager.go:204-217)

When restoring sessions, each recently-active implementer session spawns a goroutine to post a resume comment:

```go
go func(repo string, issueNum int) {
    resumeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    ...
}(rec.Repository, rec.IssueNumber)
```

With 100+ restored sessions, 100 goroutines run concurrently for 15 seconds each. **Impact**: 100 goroutines is fine, but it's unbounded — if sessions grow to 1000+, this could be an issue.

### RACE CONDITION: `labelBoot` + `EnsureLabels` (manager.go:258-270)

```go
if _, ok := m.labelBoot.Load(evt.Repository); !ok {
    m.labelBoot.Store(evt.Repository, true)
    // EnsureLabels call
}
```

`LoadOrStore` makes the first-wins check atomic, but `EnsureLabels` runs concurrently if two events for the same repo arrive simultaneously. Forgejo's label create API returns 422 on duplicate, so this is safe in practice. **Suggestion**: it works, but deserves a comment explaining why the race is harmless.

### OK: Semaphore-based LLM throttling (provider/client.go:190-196)

The `sema chan struct{}` pattern correctly limits concurrent LLM calls. Acquire respects context cancellation. Good.

### OK: Session event channel (manager.go:628)

Channel buffer of 64 for `sess.events` is reasonable. The `select` with `default` at line 514-525 drops overflow rather than blocking. This is appropriate (better to drop events than deadlock the manager).

---

## 3. Code Duplication and DRY Violations

### CRITICAL: 17 near-identical tool types in forgejo_tools.go (1409 lines)

Every tool follows the exact same template:
1. `type forgejoXxxTool struct { adapter *ForgejoAdapter }`
2. `func NewXxxTool(...) *forgejoXxxTool`
3. `func (t *...) Name() string`
4. `func (t *...) Description() string`
5. `func (t *...) Parameters() map[string]interface{}`
6. `func (t *...) Execute(...) (string, error)`

~80% of the tool code is boilerplate per-tool. A generic `APITool` factory would reduce this:

```go
type APITool struct {
    name        string
    description string
    params      func() map[string]interface{}
    exec        func(ctx context.Context, args json.RawMessage) (string, error)
}
```

~400 lines → ~100 lines.

### CRITICAL: URL construction pattern duplicated ~40 times

`path.Join("/api/v1/repos", escapeRepoPath(repo), ...)` appears ~40 times across `client.go` and `forgejo_tools.go`. A helper reduces this:

```go
func (c *Client) apiPath(repo string, parts ...string) string {
    return path.Join(append([]string{"/api/v1/repos", escapeRepoPath(repo)}, parts...)...)
}
```

### DUPLICATE: Human approval gate logic duplicated (forgejo_tools.go:728-781)

The approval gate in `forgejoMergePRTool.Execute` has an `if/else` block where both branches contain identical logic:

Branch 1 (lines 736-758): PR author is human → check reviews
Branch 2 (lines 759-781): PR author unknown → check reviews

The duplicated block is ~23 lines of review-checking code. Extract to a helper method.

### DUPLICATE: Verify gate logic duplicated (local_tools.go:440-461 vs forgejo_tools.go:471-496)

Both `gitTool.Execute` (after commit) and `forgejoCreatePRTool.Execute` (before PR creation) run the same sequence:
1. `go build ./...`
2. `go test ./... -count=1`
3. `golangci-lint run ./...`

This is ~25 lines duplicated. Extract to `verifyGate()` helper in a shared package.

### PATTERN DUPLICATION: Error handling in cmd/fj/main.go

Every command handler follows this exact pattern (12+ times):

```go
result, err := client.SomeMethod(...)
if err != nil {
    fmt.Fprintln(os.Stderr, "Error:", err)
    os.Exit(1)
}
```

Could be a `must()` helper. Simple but reduces visual noise.

---

## 4. Idiomatic Go Patterns

### Non-idiomatic: `fmt.Sscanf` for integer parsing

See bug report above. Standard library offers `strconv.Atoi` and `strconv.ParseInt`. The `Sscanf` pattern is borrowed from C and doesn't integrate with Go's error handling.

### Non-idiomatic: Value receiver on `User.String()` (client.go:71)

```go
func (u User) String() string { return u.Login }
```

All other methods use pointer receivers. Consistency: use `(u *User)`.

### Non-idiomatic: `sync.Map` used as atomic bool (manager.go:85-86)

`labelBoot` and `issueStates` use `sync.Map` but have simple access patterns:
- `labelBoot`: LoadOrStore once per repo, never modified after
- `issueStates`: Load and Store from a single goroutine

A plain `map[string]bool` with `sync.Mutex` would be simpler and faster for these specific use patterns.

### Non-idiomatic: Raw `go build` calls instead of build/test packages

The verify gates use `exec.CommandContext(ctx, "go", "build", "./...")` and `exec.CommandContext(ctx, "go", "test", "./...", "-count=1")`. This works, but is fragile — relies on `go` being in PATH, and doesn't use Go's programmatic build APIs (`golang.org/x/tools/go/packages` or `go test` as a library). For the agent use case, this is acceptable, but worth noting.

### OK: Sentinel error pattern (sentinel.go)

The `sentinel.ErrXxx` pattern with `errors.Is` / `errors.As` is idiomatic and well-implemented. The `IsRetryable` and `IsClientError` helpers are correctly composed. Good code.

### OK: Interface segregation

`ChatCompleter` interface (provider/client.go:18-21) is minimal and well-defined. `MergeGate` interface (forgejo_tools.go:318-320) is equally clean. Good.

---

## 5. Large File Cohesion

### forgejo_tools.go (1409 lines) — Should be split

**Current structure**: All 17 tool implementations in one file, plus `ForgejoAdapter`, plus `MergeGate` interface, plus `escapeRepoPath`.

**Proposed split**:
- `internal/tool/forgejo_generic.go` — Generic `APITool` factory, `ForgejoAdapter`
- `internal/tool/forgejo_comments.go` — Comment, reaction tools
- `internal/tool/forgejo_prs.go` — PR creation, merge, list, review tools
- `internal/tool/forgejo_issues.go` — Issue creation, listing, getting tools
- `internal/tool/forgejo_admin.go` — Hook, branch, label, collab, token tools

Each would be 200-300 lines of cohesive code instead of 1409 lines of repetitive boilerplate.

### cmd/fj/main.go (1101 lines) — Acceptable as-is

A CLI dispatch table with ~14 commands, averaging ~70 lines per command. The `cmdFile` function (lines 842-975) is the longest at 133 lines due to complex repo/path detection logic. Extract `cmdFile`'s `getRepoAndPath()` helper (lines 860-892) into its own function.

### internal/session/manager.go (1038 lines) — Could be split

**Current scope** (7 distinct responsibilities):
1. Session creation/cloning (lines 541-659)
2. Event routing (lines 256-526)
3. FSM state management (lines 376-419)
4. Role detection (lines 884-952 passim)
5. Scaffold detection (lines 344-360)
6. Label bootstrapping (lines 258-270)
7. Idle reaping + cleanup (lines 747-836)

**Proposed split**:
- `manager.go` — Core lifecycle (session map, event dispatch, reaping)
- `fsm.go` — FSM label → state transitions + auto-close logic
- `roles.go` — `detectRoleFromIssue`, `detectRoleFromTitle`, `handleRoleAssignment`, `postRoleGuidance`

---

## 6. Code That Will Break Under Edge Cases

### BUG: `loadConfig()` walks up from potentially invalid CWD (cmd/fj/main.go:171)

```go
cwd, _ := os.Getwd()
for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
```

If CWD is deleted or inaccessible, `os.Getwd()` returns an error and `cwd` is empty string. The loop starts from "" which may never reach "/". On Windows, `dir != "/"` never terminates. **Fix**: check `Getwd` error and handle CWD-not-found.

### BUG: `isAncestor` treats missing `origin/<base>` as "not stale" (stalegate.go:80-85)

```go
refCmd := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "origin/"+base)
if refErr := refCmd.Run(); refErr != nil {
    return true, nil // treat as ancestor (not stale)
}
```

If the remote is unreachable (network down) or the ref was never fetched, this returns "not stale" even though the branch could be wildly out of date. The agent would create a PR from a stale branch. **Fix**: check whether the error is a genuine "ref not found" vs a connectivity issue.

### BUG: Git notes are not pushed (memory.go:60-70)

`git notes add --ref fordjent` adds a note locally, but notes are not pushed by default. If the workdir is cleaned up (manager.go:802), all git-note memory is permanently lost. The JSONL fallback (line 56-58) partially addresses this, but the git notes mechanism is fragile as a durable memory store. **Either** push notes explicitly, or document that JSONL is the source of truth.

### BUG: `hmac.Equal` bypass with empty secret (router.go:606-608)

```go
if r.cfg.Webhook.Secret == "" {
    return true // No secret configured, skip validation
}
```

Empty secret bypasses all webhook signature validation. This is a reasonable dev-mode behavior, but in production the log should emit a warning on startup if the secret is empty.

### BUG: `bashTool` shell fallback (local_tools.go:119-122)

```go
shell := "bash"
if _, lookErr := exec.LookPath("bash"); lookErr != nil {
    shell = "sh"
}
```

If neither `bash` nor `sh` exists (minimal container, scratch image), `exec.CommandContext` fails with an opaque error. **Fix**: check `LookPath("sh")` too, or return a clear error.

---

## 7. Maintainability Concerns

### High: Adding a new tool requires ~30 lines of boilerplate

Current pattern per tool:
- 1 struct type definition (3 lines)
- 1 constructor (3 lines)
- 1 `Name()` (2 lines)
- 1 `Description()` (3 lines)
- 1 `Parameters()` (15-25 lines)
- 1 `Execute()` (10-50 lines)

A factory-based approach would reduce the first five items to a single `NewTool(name, desc, paramsFn, execFn)` call.

### High: `ForgejoAdapter` wraps `forgejo.Client` for historical reasons

The adapter exists because some tools were written before `forgejo.Client` had all methods. Now both `adapter.doRequest(ctx, method, apiPath, body)` (for old-style tools) and `adapter.Client().ListBranches(ctx, repo)` (for new-style tools) are used inconsistently. Standardize on one pattern. The `adapter.doRequest` bypasses the typed `forgejo.Client` methods and constructs raw API paths — a maintenance risk.

### Medium: Event types as string constants (event/event.go)

Types like `event.Type("issues.opened")` and `event.Type("issue_comment.created")` are string constants. There's no compile-time checking for valid event types. A misspelled constant like `"issue_commment.created"` would silently fail at runtime.

### Medium: FSM state detection hits Forgejo API twice per event

In `manager.go:379-418` and `agent.go:643-656`, `detectIssueState` calls `GetIssue` which makes a Forgejo API call. This happens on every event. The `issueStates` sync.Map already caches the previous state — extending the cache to track the last-fetched issue labels would eliminate the redundant API call.

---

## Summary

### Critical (will cause bugs)

| # | Issue | Location | Fix |
|---|-------|----------|-----|
| 1 | `fmt.Sscanf` silently parses 0 for non-numeric arguments | cmd/fj/main.go:369+ | Replace with `strconv.Atoi` + error handling |
| 2 | Approval gate logic duplicated 23 lines | forgejo_tools.go:728-781 | Extract to helper method |
| 3 | `isAncestor` treats remote errors as "not stale" | stalegate.go:80-85 | Check error type before returning |
| 4 | Goroutines use `Background()` instead of parent ctx | manager.go:274, 312 | Propagate parent context |

### Medium (bad practice, may cause issues)

| # | Issue | Location |
|---|-------|----------|
| 5 | URL construction duplicated ~40 times | client.go + forgejo_tools.go |
| 6 | Verify gate logic duplicated | local_tools.go + forgejo_tools.go |
| 7 | Silent error drops in blocked-session path | agent.go:307-308 |
| 8 | `sync.Map` overuse for simple use cases | manager.go:85-86 |
| 9 | `AddIssueLabels` nested error swallowing | client.go:276-296 |
| 10 | forgejo_tools.go 1409 lines, needs splitting | forgejo_tools.go |

### Low (style/consistency)

| # | Issue | Location |
|---|-------|----------|
| 11 | Dead assignment `_ = noteMsg` | memory.go:67 |
| 12 | Value vs pointer receiver inconsistency | client.go:71 |
| 13 | `loadConfig` CWD edge case | cmd/fj/main.go:171 |
| 14 | Manager.go 7 responsibilities in one file | manager.go |
| 15 | Missing startup warning for empty webhook secret | router.go |
