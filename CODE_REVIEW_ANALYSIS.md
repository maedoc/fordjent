# Fordjent Codebase Review — Full Analysis

**Date**: May 18, 2026
**Reviewers**: vino (architecture), rori (tools/security), mojo (lifecycle/coordination), rhea (verification)
**Method**: Three parallel agents reviewed distinct subsystems, followed by source-level verification of all claims.

---

## Executive Summary

Fordjent has solid layered architecture with good separation of concerns (webhook → event bus → session manager → agent → tools). The reliability layer (retry + compaction + cost tracking) is well-integrated, and the FSM state machine provides useful observability.

However, the review identified **3 critical bugs**, **7 high-severity issues**, **8 medium-severity issues**, and significant coordination weaknesses across roles. The most urgent fixes are a JSON field typo that silently breaks merge conflict detection, a fail-open merge queue that disables safety gates on API errors, and an approval gate that blocks bot PRs when Forgejo blips.

The coordination analysis reveals that Fordjent's multi-role workflow (PM → implementer → reviewer) has fundamental isolation problems: sibling sub-issues share zero context, human PRs get no reviewer, and the scheduler can deadlock on circular dependencies with no detection or recovery.

---

## 1. Critical Issues (P0)

### 1.1 `has_conflits` typo breaks merge conflict detection

| Attribute | Value |
|-----------|-------|
| File | `internal/forgejo/client.go:81` |
| Severity | Critical |
| Impact | `HasConflicts` always `false`; `forgejo_merge_pr` proceeds on conflicting PRs |
| Effort | 1 min |

**Current code:**
```go
HasConflicts bool `json:"has_conflits"` // NOTE: Forgejo API field may vary
```

**Fix:**
```go
HasConflicts bool `json:"has_conflicts"`
```

**Verification**: The `forgejo_merge_pr` tool at `forgejo_tools.go:721` checks `pr.HasConflicts` before merging. With the typo, the JSON key never matches, so the field stays `false` regardless of actual conflict state. This means the agent will attempt to merge PRs that have merge conflicts, relying solely on Forgejo's server-side rejection (409) rather than the client-side pre-check.

**Note**: The comment "Forgejo API field may vary" suggests uncertainty about the API field name. Verify against Forgejo API docs — the field is `has_conflicts` per the Gitea/Forgejo swagger spec.

---

### 1.2 Merge approval gate fails open on GetPR error

| Attribute | Value |
|-----------|-------|
| File | `internal/tool/forgejo_tools.go:728-772` |
| Severity | Critical |
| Impact | Bot PRs blocked by human approval gate when Forgejo API is unavailable |
| Effort | 10 min |

**Current code (simplified):**
```go
if !t.bypassHumanApproval {
    pr, err := t.adapter.Client().GetPR(ctx, params.Repository, params.PRNumber)
    if err == nil && pr.User != nil {
        login := strings.ToLower(pr.User.Login)
        if login == "fordjent-bot" || login == "fordjent[bot]" {
            // Bot PR — skip approval gate
        } else {
            // Human PR — require approval
        }
    } else {
        // Could not determine PR author — require approval as fallback
        // BUG: This path executes for BOTH bot and human PRs when GetPR fails
    }
}
```

**Problem**: When `GetPR()` returns an error (network blip, Forgejo restarting, rate limit), the `else` branch at line 759 executes, which requires human approval regardless of PR author. This means a transient Forgejo outage blocks all bot PRs from auto-merging.

**Fix**: Move bot-detection logic outside the `err == nil` block, or add a separate bot-sender check before calling `GetPR`:
```go
// Check sender before API call — if sender is bot, bypass immediately
if sess.Sender == "fordjent-bot" || sess.Sender == "fordjent[bot]" {
    // bypass
} else {
    pr, err := t.adapter.Client().GetPR(...)
    // ...
}
```

---

### 1.3 Merge queue fail-open on API errors

| Attribute | Value |
|-----------|-------|
| File | `internal/mergequeue/queue.go:68-71, 78-81` |
| Severity | Critical |
| Impact | Transient API failures silently disable the merge gate |
| Effort | 10 min |

**Current code:**
```go
func (c *Client) CheckGate(ctx context.Context, repo, headBranch, baseBranch string) (bool, string, error) {
    ourFiles, err := c.compareBranchFiles(ctx, repo, baseBranch, headBranch)
    if err != nil {
        // If we can't diff, don't block
        slog.Warn("mergequeue: failed to diff branch, allowing through", ...)
        return false, "", nil  // ← ALLOWS THROUGH ON ERROR
    }
    // ...
    openPRs, err := c.listOpenPRs(ctx, repo)
    if err != nil {
        slog.Warn("mergequeue: failed to list open PRs, allowing through", ...)
        return false, "", nil  // ← ALLOWS THROUGH ON ERROR
    }
```

**Problem**: Any API failure (network timeout, 503, rate limit) causes `CheckGate` to return "not blocked", allowing PRs through the merge gate without conflict checking. This is a fail-open design that defeats the purpose of the gate under exactly the conditions (high load, transient failures) where it's most needed.

**Fix**: Fail-closed:
```go
if err != nil {
    return true, fmt.Sprintf("Merge gate unavailable: %v. Try again later.", err), nil
}
```

---

## 2. High-Severity Issues (P1)

### 2.1 Protected branch: `git commit` not blocked

| Attribute | Value |
|-----------|-------|
| File | `internal/tool/local_tools.go:426` |
| Severity | High |
| Impact | Agent commits to main; push fails but dirty worktree remains on main |
| Effort | 15 min |

The `bash` tool blocks `git push origin main`, and the auto-push after commit checks `isProtected`. But `git commit` itself (via the `git` tool) is never checked against protected branches. The agent can commit to main, see a successful commit output, then the auto-push fails silently. The worktree is left dirty on main.

**Fix**: Add `isProtected` check before executing any `git commit` command in the `git` tool handler, similar to how the `bash` tool blocks push.

---

### 2.2 `LastActive` data race in `reapIdle` and `evictOldest`

| Attribute | Value |
|-----------|-------|
| File | `internal/session/manager.go:752, 821` |
| Severity | High |
| Impact | Undefined behavior under concurrent session access; potential crash or incorrect reaping |
| Effort | 20 min |

**`reapIdle` (line 752):**
```go
for key, sess := range m.sessions {
    if time.Since(sess.LastActive) > m.cfg.Agent.IdleTimeout {  // ← no lock
        sess.mu.Lock()
        busy := sess.busy
        sess.mu.Unlock()
```

**`evictOldest` (line 821):**
```go
if oldestKey == "" || sess.LastActive.Before(oldestTime) {  // ← no lock
```

`LastActive` is written in `runSession` under `sess.mu`, but read here without the lock. Go's race detector will flag this, and it can cause stale reads under concurrent access.

**Fix**: Read `LastActive` under `sess.mu.Lock()` in both functions.

---

### 2.3 Retry on 409 (conflict) in merge tool is pointless

| Attribute | Value |
|-----------|-------|
| File | `internal/tool/forgejo_tools.go:803-804` |
| Severity | High |
| Impact | Wastes retry budget; 3-second delay on unrecoverable state |
| Effort | 2 min |

```go
if errors.As(err, &apiErr) && (apiErr.StatusCode == 405 || apiErr.StatusCode == 409) {
    continue // 405 = try again later, 409 = conflict (may resolve)
}
```

A 409 means the PR has merge conflicts. Retrying will never resolve this — only code changes can. The comment "may resolve" is incorrect for conflict-based 409s.

**Fix**: Only retry on 405; return 409 immediately:
```go
if errors.As(err, &apiErr) && apiErr.StatusCode == 405 {
    continue
}
if errors.As(err, &apiErr) && apiErr.StatusCode == 409 {
    return "", fmt.Errorf("PR #%d has merge conflicts — resolve conflicts before merging", params.PRNumber)
}
```

---

### 2.4 `--force-with-lease` without refspec in stale gate

| Attribute | Value |
|-----------|-------|
| File | `internal/stalegate/stalegate.go:52` |
| Severity | High |
| Impact | Push may fail on some git versions; lease check can't verify remote state |
| Effort | 5 min |

```go
pushOut, pushErr := exec.Command("git", "-C", repoDir, "push", "--force-with-lease", "-u", "origin", "HEAD").CombinedOutput()
```

`--force-with-lease` without a refspec checks the remote ref for the current branch name. After rebase, the local branch may have a different name than what the remote expects. Some git versions require an explicit refspec for lease verification.

**Fix**: Use explicit refspec or fall back to `--force` in the rebase-push context (rebase-after-stale-detection is an intentional force scenario):
```go
branch := getCurrentBranch(repoDir) // helper to read HEAD
pushOut, pushErr := exec.Command("git", "-C", repoDir, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+branch).CombinedOutput()
```

---

### 2.5 Human PRs get no automatic reviewer

| Attribute | Value |
|-----------|-------|
| File | `internal/session/manager.go:666-681` |
| Severity | High |
| Impact | Human PRs sit unreviewed unless manually tagged `[review]` |
| Effort | 2 hrs |

```go
if isBotPR && (role == "implementer" || role == "") {
    role = "reviewer"
}
```

Only bot-created PRs get auto-assigned the reviewer role. Human-created PRs stay in whatever role was detected (or empty), meaning no reviewer session spawns. This is a significant gap: the system reviews its own work but not human work.

**Fix**: All PRs should spawn reviewer sessions on `pull_request.opened`, regardless of author.

---

### 2.6 Session timeout fallback inconsistent

| Attribute | Value |
|-----------|-------|
| File | `internal/session/agent.go:89-92` |
| Severity | High |
| Impact | Config default 60min but runtime fallback 30min — confusing behavior |
| Effort | 5 min |

```go
sessionTimeout := a.cfg.Agent.SessionTimeout
if sessionTimeout == 0 {
    sessionTimeout = 30 * time.Minute  // ← hard-coded fallback
}
```

If the config field is left at zero (default), the agent gets 30 minutes. But the config struct default and documentation likely say 60 minutes. This mismatch causes unexpected session termination.

**Fix**: Remove the hard-coded fallback; rely on config validation to set a proper default.

---

### 2.7 `detectAnalysisMode` makes API call every LLM turn

| Attribute | Value |
|-----------|-------|
| File | `internal/session/agent.go:440-449` |
| Severity | High |
| Impact | 100-500ms latency added to every turn for automerge label check |
| Effort | 1 hr |

```go
issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
if err == nil && issue != nil {
    for _, l := range issue.Labels {
        if l.Name == "automerge" {
            hasAutomerge = true
            break
        }
    }
}
```

This runs inside `buildSystemPrompt`, which is called every turn. The automerge label state doesn't change between turns, so this is wasted latency.

**Fix**: Cache `hasAutomerge` in the `Agent` struct; detect on first turn only.

---

## 3. Medium-Severity Issues (P2)

### 3.1 Sibling sub-issues have zero shared context

| Attribute | Value |
|-----------|-------|
| File | `internal/session/agent.go:550-573` |
| Severity | Medium (Critical for coordination) |
| Impact | Implementers work in isolation; duplication, inconsistency, conflicting approaches |
| Effort | 4 hrs |

`buildContext` fetches only the current issue body + comments + session memory. It never fetches the parent issue or sibling sub-issues. When a PM decomposes a feature into 5 sub-issues, each implementer sees only their slice.

**Fix**: If the issue body contains `Depends on: #N`, fetch the parent issue (#N) and include its body in context. Also fetch sibling issues that share the same parent.

---

### 3.2 No parent issue completion tracking

| Attribute | Value |
|-----------|-------|
| File | `internal/scheduler/scheduler.go` |
| Severity | Medium |
| Impact | PM creates sub-issues then stops; no mechanism to detect all children are done |
| Effort | 6 hrs |

The scheduler unblocks individual issues when dependencies are met, but there is no mechanism to:
1. Detect when ALL sub-issues of a parent are completed
2. Notify the PM that decomposition is fully implemented
3. Post a summary comment on the parent issue

This means the PM role has no closure — it creates work but never sees it finished.

**Fix**: On every PR merge, check if the merged PR's issue has a parent (via `Depends on:` in other issues). If all children of a parent are closed, post a completion comment on the parent.

---

### 3.3 Scheduler assumes not-closed on API errors

| Attribute | Value |
|-----------|-------|
| File | `internal/scheduler/scheduler.go:102-105` |
| Severity | Medium |
| Impact | Transient API errors keep issues blocked indefinitely |
| Effort | 2 hrs |

```go
isClosed, err := s.isIssueClosed(ctx, repo, depNum)
if err != nil {
    slog.Warn("scheduler: failed to check issue state, assuming not closed", ...)
    allSatisfied = false  // ← assumes not closed on error
    break
}
```

If the Forgejo API is temporarily unavailable, the scheduler assumes dependencies are not satisfied. Combined with the fact that the scheduler only runs on `pull_request.merged` webhooks, a missed check means the issue stays blocked until the next PR merge event.

**Fix**: Retry with backoff (2-3 attempts) before assuming not-closed. Or schedule a re-check via a deferred queue.

---

### 3.4 No circular dependency detection

| Attribute | Value |
|-----------|-------|
| File | `internal/scheduler/scheduler.go:92-114` |
| Severity | Medium |
| Impact | Mutual `Depends on:` deadlocks forever with no detection or warning |
| Effort | 3 hrs |

If issue #3 has `Depends on: #4` and #4 has `Depends on: #3`, neither will ever be unblocked. The scheduler has no cycle detection.

**Fix**: Before evaluating dependencies, build a dependency graph and check for cycles. If a cycle is detected, post a warning comment on the affected issues.

---

### 3.5 `escapeRepoPath` duplicated in 3 packages

| Attribute | Value |
|-----------|-------|
| Files | `internal/tool/forgejo_tools.go:22`, `internal/forgejo/client.go:176`, `internal/mergequeue/queue.go:216` |
| Severity | Medium |
| Impact | Divergent implementations over time; maintenance burden |
| Effort | 20 min |

Three identical copies of the same function. If a bug is fixed in one, the others may not be updated.

**Fix**: Move to `internal/forgejo/util.go` and export.

---

### 3.6 Label dedup creates labels with dummy color

| Attribute | Value |
|-----------|-------|
| File | `internal/forgejo/client.go:276` |
| Severity | Medium |
| Impact | Labels may fail to create on non-422 errors; silent failure |
| Effort | 15 min |

`AddIssueLabels` calls `CreateLabel` with color `"ededed"` as a fallback. If creation fails with a non-422 status, the error is silently swallowed.

**Fix**: Return error if label creation fails with a non-422 status.

---

### 3.7 `eventToUserMessage` truncates large payloads silently

| Attribute | Value |
|-----------|-------|
| File | `internal/session/agent.go:604-607` |
| Severity | Medium |
| Impact | Large PR diffs, issue bodies with attachments silently disappear from context |
| Effort | 30 min |

```go
payloadJSON, err := json.MarshalIndent(evt.Payload, "", "  ")
if err == nil && len(payloadJSON) < 5000 {
    sb.WriteString(...)
}
```

When payload exceeds 5000 bytes, it's dropped with no logging. The agent loses context about the event that triggered it.

**Fix**: Log when truncation occurs. Include a summary (first N chars) even when full payload is dropped.

---

### 3.8 `handleEvent` label bootstrapping is not atomic

| Attribute | Value |
|-----------|-------|
| File | `internal/session/manager.go:259` |
| Severity | Low |
| Impact | Redundant `EnsureLabels` calls under concurrent events; no data corruption |
| Effort | 5 min |

`Sync.Map.Load` then `Store` is not atomic. Two concurrent events on the same repo both see "not loaded" and both call `EnsureLabels`. This is idempotent (safe), but wasteful.

**Fix**: Use `LoadOrStore` — only first goroutine executes:
```go
if _, loaded := m.labelBoot.LoadOrStore(evt.Repository, true); !loaded {
    // First time for this repo — bootstrap labels
}
```

Note: The `EnableAutoCollaborator` warning at line 600-606 already uses `LoadOrStore` correctly. The label bootstrap at line 259 does not.

---

## 4. Coordination Analysis

This section analyzes how Fordjent's roles (PM, implementer, reviewer, devops, tester) coordinate — or fail to — across the issue lifecycle.

### 4.1 Current Role Flow

```
[PM Issue] → forgejo_create_issue → sub-issues with "Depends on: #N"
                                        ↓
                              scheduler blocks sub-issues
                                        ↓
                              PR merged → scheduler unblocks
                                        ↓
                              [Implementer Issue] → work → PR
                                        ↓
                              [Reviewer] → inspect → merge (if automerge)
                                        ↓
                              [Scheduler] → unblock next dependent
```

### 4.2 Coordination Failure Map

```
 ┌─────────┐     Creates sub-issues      ┌────────────┐
 │   PM    │ ──────────────────────────→ │ Implementer│
 │         │ ← NO feedback path          │            │
 └─────────┘                             └─────┬──────┘
       ↑                                       │
       │ NO completion notification            ↓
       │                                ┌────────────┐
       │                                │  Reviewer  │
       │                                │ (bot PRs   │
       │                                │  only)     │
       │                                └────────────┘
       │                                       ↑
       └───────────────────────────────────────┘
          NO reviewer → PM feedback; NO cross-sibling context
```

### 4.3 Detailed Coordination Gaps

#### Gap 1: PM → Implementer Handoff Has No Context Transfer

When a PM creates sub-issues, each sub-issue body gets `Depends on: #N` but NO excerpt from the parent issue's analysis. The implementer sees only the sub-issue title and body — a narrow slice of the PM's broader analysis.

**Impact**: Implementers make locally-optimal but globally-wrong decisions. Example: PM decomposes "Add auth system" into sub-issues for "middleware", "token validation", "session store". The implementer for "middleware" doesn't know the PM chose JWT tokens (not sessions), and may implement session-based middleware.

**Fix**: Append parent issue body excerpt to sub-issue body during `forgejo_create_issue`. Or add parent issue fetching in `buildContext`.

---

#### Gap 2: Implementer → PM Has No Feedback Channel

After creating sub-issues, the PM session ends. There is no mechanism for an implementer to ask the PM clarifying questions. The implementer must make assumptions or post a comment on the parent issue and hope someone (human) responds.

**Impact**: Ambiguous requirements are resolved by the implementer's best guess, which may not match the PM's intent.

**Fix**: Either keep PM sessions alive for follow-up, or add a `forgejo_ping_parent_issue` tool that posts a question and re-triggers the PM session.

---

#### Gap 3: Sibling Sub-Issues Have Zero Shared Context

Implementer working on sub-issue #5 has no visibility into sub-issues #3, #4, #6 from the same parent. Each session has isolated memory (`memory.jsonl`), and `buildContext` never fetches sibling issues.

**Impact**:
- **Duplication**: Two implementers independently create the same utility function.
- **Inconsistency**: One implementer uses `pkg/auth`, another uses `pkg/session/auth`.
- **Conflicting approaches**: One implementer writes tests first, another writes code first, leading to merge conflicts.

**Fix**: Shared memory file for all sub-issues of the same parent. Or `forgejo_get_sibling_issues` tool that returns sibling issue bodies and current PR status.

---

#### Gap 4: No Priority Ordering on Unblock

When multiple sub-issues are unblocked simultaneously (all dependencies satisfied), ALL get the `ready` label at once. If #3, #4, #5 all depend on #2 and #2 merges, all three become ready simultaneously.

**Impact**: Multiple implementers start conflicting work on overlapping files. The merge queue blocks PRs that touch the same files, but this causes delays rather than preventing the work from starting.

**Fix**: Priority-ordered unblocking. Add `Priority: N` syntax to issue bodies. Scheduler unblocks one at a time in priority order. Or add `in_progress` label as a claim mechanism: first implementer to comment "starting #N" claims it.

---

#### Gap 5: No Claim Protocol for Ready Issues

Two implementers can see the `ready` label on the same issue and both start working. This produces duplicate PRs and merge conflicts.

**Impact**: Wasted agent turns, merge queue blocking, manual resolution required.

**Fix**: Add `in_progress` label. When an implementer session starts, it adds `in_progress` and removes `ready`. Other implementers see `in_progress` and skip.

---

#### Gap 6: Human PRs Get No Reviewer

Only bot-created PRs trigger auto-reviewer assignment (manager.go:678). Human-created PRs sit unreviewed unless manually tagged `[review]`. This is the reverse of what's expected: human work should be reviewed at least as thoroughly as bot work.

**Impact**: Human PRs may introduce bugs, style violations, or architectural drift without review.

**Fix**: All `pull_request.opened` events should spawn reviewer sessions, regardless of author.

---

#### Gap 7: Reviewer Has No Parent Issue Context

When a reviewer inspects a PR that implements a sub-issue, `buildContext` fetches the PR's issue body + comments but NOT the parent issue. The reviewer cannot verify that the PR satisfies the parent's overall requirements.

**Impact**: Reviewer approves code that is locally correct but doesn't satisfy the parent's intent.

**Fix**: If PR body contains `Closes: #N` or `Fixes: #N`, fetch the referenced issue and include in reviewer context.

---

#### Gap 8: Merge Queue Doesn't Coordinate with Scheduler

The merge queue checks file-level conflicts between open PRs but has no awareness of the dependency graph. A PR that is a dependency of another blocked issue is not treated differently from any other PR.

**Impact**: If a dependency PR is blocked by the merge queue (due to file overlap with a non-dependency PR), the scheduler can't prioritize it.

**Fix**: Merge queue should accept priority hints from the scheduler. Dependency PRs should be given merge priority.

---

#### Gap 9: PM Sessions End Prematurely

PM creates sub-issues then stops. There is no mechanism for the PM to:
1. Track sub-issue completion
2. Revise the decomposition if implementation reveals new requirements
3. Post a coordinating summary when all sub-issues are done
4. Handle implementer questions

**Impact**: PM is a one-shot role that creates work but never manages it. The coordination loop is broken at the PM → implementer boundary.

**Fix**: Keep PM sessions alive in a "waiting" state. On sub-issue completion, re-activate PM to check remaining work and post summaries.

---

#### Gap 10: No State Recovery After Scheduler Unblock

When the scheduler unblocks an issue (removes `blocked`, adds `ready`, posts comment), if no session picks it up, the issue sits `ready` forever. There is no background process to detect `ready` issues with no active session.

**Impact**: Ready issues are ignored if the unblock comment doesn't trigger a new webhook event (e.g., because the agent's webhook filter drops label updates from non-role events).

**Fix**: Periodic reaper that scans for `ready` issues with no active session and re-creates sessions.

---

### 4.4 Coordination Failure Severity Matrix

| Gap | Likelihood | Impact | Severity |
|-----|-----------|--------|----------|
| Sibling sub-issues isolated | High | High | **Critical** |
| No parent context transfer | High | High | **Critical** |
| No claim protocol | Medium | Medium | **High** |
| Human PRs unreviewed | High | Medium | **High** |
| No priority ordering | Medium | Medium | **High** |
| PM no feedback channel | Medium | Medium | **High** |
| Reviewer no parent context | Medium | Medium | **Medium** |
| No state recovery | Low | High | **Medium** |
| Merge queue ≠ scheduler | Low | Medium | **Medium** |
| PM sessions end early | High | Low | **Medium** |

---

## 5. Lifecycle Analysis

### 5.1 State Machine Coverage

```
created ──→ working ──→ pr_created ──→ completed
                │
                ├──→ failed_max_turns
                ├──→ failed_error
                └──→ blocked
```

**Missing transitions:**
- `working → timed_out` (session timeout is handled by context cancellation, not lifecycle)
- `blocked → working` (scheduler unblock doesn't trigger lifecycle transition)
- `failed_* → working` (no auto-retry mechanism)
- `pr_created → completed` (merge event doesn't trigger lifecycle completion)

### 5.2 Stuck Session Scenarios

| Scenario | Root Cause | Frequency | Recovery |
|----------|-----------|-----------|----------|
| Stuck in `working` | LLM timeout, agent crash | Common | Manual cleanup |
| Stuck in `blocked` | Merge gate never clears | Rare | Manual label removal |
| Never leaves `created` | First LLM call fails | Uncommon | Session recovery (2hr window) |
| Stuck in `failed_error` | No auto-retry path | Common | Manual re-trigger |

### 5.3 Session Recovery Gaps

Current recovery (manager.go:197-218):
- Only runs on startup
- 2-hour window (sessions older than 2hrs ignored)
- Only implementer/tester/devops roles (PM explicitly skipped)
- No background stuck-session detector

**Recommended fixes:**
1. Extend recovery window to 24 hours
2. Add background stuck-session detector (runs every 30 min)
3. Auto-retry `failed_error` sessions up to 3 times
4. Include PM sessions in recovery

---

## 6. SQLite Concurrency Issues

### 6.1 BUSY Errors

Under concurrent load (5+ simultaneous sessions), SQLite returns `database is locked` errors. Current handling:

| Location | Behavior |
|----------|----------|
| `lifecycle.go:70-73` | Logs warning, returns error (callers may ignore) |
| `lifecycle.go:333-340` (RecordTurn) | Logs warning, returns error |
| `lifecycle.go:345-352` (RecordDelivery) | Logs warning, returns error |
| `cost/cost.go` | Similar pattern |

**Fix**: Add `PRAGMA busy_timeout = 5000` in all `initSchema()` functions. This makes SQLite wait up to 5 seconds for locks rather than failing immediately.

### 6.2 No WAL Mode

SQLite default journal mode is `DELETE`, which is slower under concurrent writes. WAL mode allows concurrent reads + writes.

**Fix**: Add `PRAGMA journal_mode = WAL` in initSchema.

---

## 7. Error Handling Patterns

### 7.1 Inconsistent Error Wrapping

| Pattern | Examples | Problem |
|---------|----------|---------|
| `fmt.Errorf("...: %w", err)` | Most packages | Correct — enables `errors.Is/As` |
| Return typed errors | `HTTPError`, `ErrAPIClient` | Good — enables type switching |
| Swallow errors | `lifecycle.go`, `scheduler.go` | Bad — callers can't detect failures |
| Silent fallback | `detectRoleFromSession` defaults to `implementer` | Bad — masks failures |

### 7.2 Recommended Standards

1. All errors should be wrapped with `%w` for chain unwrapping
2. Add sentinel errors for key failure modes:
   - `ErrMaxTurnsReached`
   - `ErrSessionTimeout`
   - `ErrSQLiteBusy`
   - `ErrMergeConflict`
   - `ErrApprovalRequired`
3. Never silently default to a role on API failure — log and return empty string

---

## 8. Security Review

### 8.1 Path Traversal in `read_file`

| Attribute | Value |
|-----------|-------|
| File | `internal/tool/local_tools.go:229-240` |
| Severity | High |
| Status | Partially mitigated |

Current sanitization:
- Strips `repo/` prefix
- Strips `repoDir` prefix
- Uses `filepath.Clean()` + `HasPrefix(repoDir)`

**Gap**: `..` traversal not explicitly tested. `filepath.Clean` + `HasPrefix` should catch it, but the logic is fragile. A path like `repo/../../../etc/passwd` after Clean becomes `../../etc/passwd` which the prefix check should reject — but only if the check is correct.

**Fix**: Add explicit test case for `..` traversal. Consider using `filepath.Rel` to verify the resolved path is within repoDir.

### 8.2 Token Handling

Forgejo token stored in `.env` file, loaded via `--env-file`. Not committed to git. Acceptable for local dev.

**Production concern**: Token is passed as environment variable to container. Ensure container runtime doesn't leak env vars in logs or inspection endpoints.

### 8.3 Webhook Authentication

HMAC verification with shared secret. The `/status` endpoint is unauthenticated — consider adding auth for production.

---

## 9. What Works Well

These subsystems are well-designed and require no immediate changes:

1. **`escapeRepoPath()`** — Correctly handles `owner/repo` path escaping per Forgejo's router
2. **Agent comment marker (`<!-- ford -->`)** — Prevents self-comment loops reliably
3. **Retry with exponential backoff** — Correctly classifies retryable vs non-retryable errors
4. **Auto-compaction** — Truncate strategy is predictable and avoids extra LLM calls
5. **Merge queue file gate** — Correctly compares branch files vs open PRs
6. **Protected branch blocking in `bash` tool** — Blocks `git push origin main`
7. **Cost tracking** — SQLite-backed, per-session/per-repo/per-month granularity
8. **FSM state machine** — Useful observability; label integration provides external visibility
9. **Scheduler `isIssueClosed` for PM issues** — Correctly treats issues without PRs as non-blocking
10. **Verification gate** — `go build` + `go test` before PR creation catches broken code early

---

## 10. Prioritized Fix Plan

### Phase 1: Critical Fixes (1-2 hours)

| # | Fix | File | Effort |
|---|-----|------|--------|
| 1 | Fix `has_conflits` typo | `client.go:81` | 1 min |
| 2 | Fix merge approval gate — move bot detection outside error block | `forgejo_tools.go:728-772` | 10 min |
| 3 | Merge queue fail-closed on API errors | `queue.go:68-71, 78-81` | 10 min |
| 4 | Remove 409 retry in merge tool | `forgejo_tools.go:803-804` | 2 min |
| 5 | Fix `--force-with-lease` refspec | `stalegate.go:52` | 5 min |
| 6 | Fix `LastActive` race conditions | `manager.go:752, 821` | 20 min |
| 7 | Add `LoadOrStore` for label bootstrap | `manager.go:259` | 5 min |

### Phase 2: High Fixes (4-8 hours)

| # | Fix | File | Effort |
|---|-----|------|--------|
| 8 | Block `git commit` on protected branches | `local_tools.go:426` | 15 min |
| 9 | Auto-assign reviewer for all PRs | `manager.go:666-681` | 2 hrs |
| 10 | Cache `hasAutomerge` in Agent struct | `agent.go:440-449` | 1 hr |
| 11 | Remove hard-coded 30min fallback | `agent.go:89-92` | 5 min |
| 12 | Add SQLite `busy_timeout` + WAL mode | All `initSchema()` | 1 hr |
| 13 | Add sentinel errors (`ErrMaxTurnsReached`, etc.) | Multiple | 2 hrs |
| 14 | Log on `eventToUserMessage` truncation | `agent.go:604-607` | 30 min |

### Phase 3: Coordination Fixes (12-20 hours)

| # | Fix | File | Effort |
|---|-----|------|--------|
| 15 | Fetch parent issue in `buildContext` | `agent.go:550-573` | 4 hrs |
| 16 | Add claim protocol (`in_progress` label) | `manager.go` | 3 hrs |
| 17 | Circular dependency detection | `scheduler.go` | 3 hrs |
| 18 | Extend session recovery to 24hrs | `manager.go:197-218` | 1 hr |
| 19 | Add stuck-session detector | New: `internal/lifecycle/reaper.go` | 4 hrs |
| 20 | Priority-ordered unblocking | `scheduler.go` | 4 hrs |
| 21 | Parent issue completion tracking | `scheduler.go` | 6 hrs |
| 22 | Push git notes to remote `fordjent` ref | `memory.go:62-69` | 2 hrs |

### Phase 4: Hardening (8-12 hours)

| # | Fix | File | Effort |
|---|-----|------|--------|
| 23 | Deduplicate `escapeRepoPath` | 3 files → `forgejo/util.go` | 20 min |
| 24 | Fix label dedup error handling | `client.go:276` | 15 min |
| 25 | Add `..` traversal test for `read_file` | `local_tools_test.go` | 30 min |
| 26 | Add concurrency integration test (10 simultaneous events) | New test | 4 hrs |
| 27 | Add `/status` endpoint authentication | `router.go` | 2 hrs |
| 28 | Role detection from issue body | `manager.go` | 1 hr |
| 29 | Scheduler retry with backoff on API errors | `scheduler.go:102-105` | 2 hrs |

---

## 11. Test Coverage Baseline

| Package | Coverage | Priority Gaps |
|---------|----------|---------------|
| `internal/agent` | 45.1% | Compaction edge cases, concurrent turn execution |
| `internal/config` | 35.5% | Validation of new fields, role provider resolution |
| `internal/cost` | 61.9% | Budget enforcement, concurrent SQLite access |
| `internal/event` | 94.7% | Good |
| `internal/forgejo` | 13.5% | **Critical gap** — API client barely tested; `has_conflits` typo not caught |
| `internal/lifecycle` | 35.5% | Recovery paths, concurrent transitions |
| `internal/memory` | 70.6% | Git notes push, cross-session queries |
| `internal/mergequeue` | 80.4% | Error path coverage (currently fail-open) |
| `internal/provider` | 65.8% | Retry classification, usage tracking |
| `internal/scheduler` | 77.7% | Circular deps, transitive deps, error paths |
| `internal/session` | 60.0% | Role coordination, cross-session context, claim protocol |
| `internal/stalegate` | 61.1% | Force-with-lease refspec, conflict resolution |
| `internal/tool` | 32.5% | **Critical gap** — protected branch bypass, path traversal |
| `internal/webhook` | 38.6% | Closed PR guard, HMAC verification |

**Priority test additions:**
1. Forgejo client: JSON deserialization with correct field names
2. Tool: protected branch commit blocking
3. Tool: path traversal in `read_file`/`write_file`
4. Merge queue: fail-closed behavior on API errors
5. Integration: 10 simultaneous webhook events on same repo

---

## Appendix A: Complete Finding Index

| # | Category | Severity | Summary | File:Line |
|---|----------|----------|---------|-----------|
| 1 | Bug | Critical | `has_conflits` typo | `client.go:81` |
| 2 | Bug | Critical | Approval gate fails open on error | `forgejo_tools.go:728` |
| 3 | Design | Critical | Merge queue fail-open | `queue.go:68-81` |
| 4 | Bug | High | Protected branch commit bypass | `local_tools.go:426` |
| 5 | Race | High | `LastActive` read without lock | `manager.go:752,821` |
| 6 | Bug | High | 409 retry is pointless | `forgejo_tools.go:803` |
| 7 | Bug | High | `--force-with-lease` without refspec | `stalegate.go:52` |
| 8 | Design | High | Human PRs get no reviewer | `manager.go:666-681` |
| 9 | Config | High | Session timeout fallback 30min vs 60min | `agent.go:89-92` |
| 10 | Perf | High | API call every turn for automerge check | `agent.go:440-449` |
| 11 | Coordination | Critical | Sibling sub-issues isolated | `agent.go:550-573` |
| 12 | Coordination | High | No parent context transfer | `agent.go:550-573` |
| 13 | Coordination | High | No claim protocol for ready issues | `manager.go` |
| 14 | Coordination | High | No priority ordering on unblock | `scheduler.go` |
| 15 | Coordination | High | PM no feedback channel | — |
| 16 | Coordination | Medium | Reviewer no parent context | `agent.go:550-573` |
| 17 | Coordination | Medium | No state recovery after unblock | — |
| 18 | Coordination | Medium | Merge queue ≠ scheduler | — |
| 19 | Coordination | Medium | PM sessions end prematurely | — |
| 20 | Lifecycle | Medium | No stuck-session detector | `lifecycle.go` |
| 21 | Lifecycle | Medium | Recovery window only 2hrs | `manager.go:197-218` |
| 22 | Scheduler | Medium | No circular dependency detection | `scheduler.go:92-114` |
| 23 | Scheduler | Medium | Assumes not-closed on API error | `scheduler.go:102-105` |
| 24 | SQLite | Medium | No `busy_timeout` or WAL mode | All `initSchema()` |
| 25 | Dedup | Medium | `escapeRepoPath` in 3 packages | 3 files |
| 26 | Error | Medium | Label dedup swallows non-422 | `client.go:276` |
| 27 | Error | Medium | Silent payload truncation | `agent.go:604-607` |
| 28 | Race | Low | Label bootstrap not atomic | `manager.go:259` |
| 29 | Config | Medium | `detectRoleFromSession` silent fallback | `manager.go:904-910` |
| 30 | Security | High | Path traversal in `read_file` incomplete | `local_tools.go:229-240` |

---

*End of analysis. Generated by 3 parallel review agents (vino, rori, mojo) with source-level verification by rhea.*
