# Fordjent Recovery & Enhancement Plan

**Date**: May 18, 2026
**Sources**: CODE_REVIEW_ANALYSIS.md, sandboxed.sh research, live testing session

---

## State Before This Plan

### Fixed This Session
- `ListLabels` returned `ID: 0` for all labels (root cause of most label bugs)
- `RemoveIssueLabel` sent name string instead of numeric ID
- Scheduler `addLabel`/`removeLabel` sent name strings instead of IDs
- `EnsureLabels` created duplicates on every call (Forgejo allows duplicate names)
- `forgejo_create_issue` added `blocked` as string `["blocked"]` instead of `{"labels": [id]}`
- `AddIssueLabels` `CreateLabel` fallback created duplicates
- FSM: `needs-role` → `opened` transition rejected
- FSM: `ready` → `plan-approved` transition missing
- FSM: `blocked` → `plan-approved` transition missing
- FSM: state got stuck on invalid transitions (now always updates to actual labels)
- Green-light detection (plan-approved/ready/implementing) now triggers sessions
- Green-light events skip role gate + scaffold detection
- Role gate had two redundant blocks; both now skip green-light events
- `handleRoleAssignment` returns bool so green-light check runs after
- `role:*` labels added to `EnsureLabels`
- Protected branch `git commit` blocking (confirmed FIXED in review)

### Not Yet Fixed (from CODE_REVIEW_ANALYSIS.md)
Critical bugs, data races, coordination gaps, and architectural improvements remain.

---

## Phase 0: Critical Production-Safety Bugs (1 hour)

### 0.1 `has_conflits` JSON typo — `forgejo/client.go:86`
**Status**: Still exists
**Impact**: `HasConflicts` always `false`; `forgejo_merge_pr` proceeds on conflicting PRs
**Fix**: `has_conflits` → `has_conflicts`
**Effort**: 1 min + verify with Forgejo API swagger spec

### 0.2 Merge queue fail-open on API errors — `mergequeue/queue.go:68-81`
**Status**: Still exists
**Impact**: Any API timeout/503 disables the merge gate; PRs proceed without conflict checking
**Fix**: Change `return false, "", nil` → `return true, "Merge gate unavailable: ...", nil`
**Effort**: 10 min

### 0.3 `LastActive` data race — `session/manager.go:752, 821`
**Status**: Still exists
**Impact**: Go race detector violation; stale reads under concurrent session access
**Fix**: Read `LastActive` under `sess.mu.Lock()` in both `reapIdle` and `evictOldest`
**Effort**: 20 min

---

## Phase 1: High-Priority Bug Fixes (3 hours)

### 1.1 Retry on 409 conflict in merge tool — `forgejo_tools.go:803-804`
**Status**: Still exists
**Impact**: Wastes retry budget on unrecoverable state; 3s delay
**Fix**: Return immediately on 409; only retry on 405
**Effort**: 2 min

### 1.2 `--force-with-lease` without branch refspec — `stalegate/stalegate.go:51`
**Status**: Still exists
**Impact**: Lease check may fail on some git versions
**Fix**: Use explicit refspec: `HEAD:refs/heads/<branch>`
**Effort**: 5 min

### 1.3 Session timeout fallback inconsistent — `agent.go:89-92`
**Status**: Still exists
**Impact**: Config says 60min, runtime fallback is 30min
**Fix**: Remove hard-coded fallback; rely on config validation
**Effort**: 5 min

### 1.4 Label bootstrap not atomic — `manager.go:259`
**Status**: Still exists
**Impact**: Redundant `EnsureLabels` calls under concurrent events (idempotent but wasteful)
**Fix**: Replace `Load`+`Store` with `LoadOrStore`
**Effort**: 5 min

### 1.5 SQLite `busy_timeout` + WAL mode — all `initSchema()`
**Status**: Not implemented
**Impact**: `database is locked` errors under concurrent sessions
**Fix**: Add `PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL` in all schema init
**Effort**: 30 min

### 1.6 Sentinel errors for key failure modes
**Status**: Not implemented
**Impact**: Callers can't detect specific failures (`ErrMaxTurnsReached`, `ErrSessionTimeout`, etc.)
**Fix**: Add `internal/sentinel/` package with typed errors; use `errors.Is()` throughout
**Effort**: 2 hrs

### 1.7 `detectAnalysisMode` per-turn API call — `agent.go:440-449`
**Status**: Still exists
**Impact**: 100-500ms latency added to every turn for automerge label check
**Fix**: Cache `hasAutomerge` in Agent struct; detect on first turn only
**Effort**: 1 hr

### 1.8 Event payload truncation logging — `agent.go:604-607`
**Status**: Still exists
**Impact**: Large PR diffs silently dropped from context with no visibility
**Fix**: Log when truncation occurs; include first N chars even when full payload dropped
**Effort**: 30 min

---

## Phase 2: Coordination Architecture (12-20 hours)

### 2.1 Sibling sub-issue context sharing
**Problem**: Implementers working on sub-issues #3-#6 from the same parent have zero shared context. Each sees only its own issue body. This causes duplication, inconsistency, and conflicting approaches.
**Fix**: 
- When `buildContext` detects `Depends on: #N` in the issue body, fetch parent issue (#N) body + comments
- Add `forgejo_get_sibling_issues` tool that returns sibling issue bodies and PR status
- Include parent body excerpt in `forgejo_create_issue` tool output
**Effort**: 4 hrs

### 2.2 Claim protocol for ready issues
**Problem**: Two implementers can both start on the same `ready` issue. No lock, no claim, duplicate PRs.
**Fix**: 
- Add `in_progress` FSM state and label
- When an implementer session starts, atomically: remove `ready`, add `in_progress`
- Other implementers see `in_progress` label and skip
- On session timeout/failure, remove `in_progress`, re-add `ready`
**Effort**: 3 hrs

### 2.3 All PRs get reviewer sessions
**Problem**: Only bot-created PRs trigger auto-reviewer. Human PRs sit unreviewed.
**Fix**: On every `pull_request.opened`, spawn reviewer session regardless of author. Keep bot-auto-bypass for merge approval.
**Effort**: 2 hrs

### 2.4 Parent context transfer from PM to implementer
**Problem**: PM creates sub-issues but the implementer sees none of the PM's analysis.
**Fix**: When `forgejo_create_issue` creates a sub-issue, append parent body excerpt to sub-issue body: `\n\n## Parent Context (from #N)\n{parent_body_excerpt}`
**Effort**: 1 hr

### 2.5 Circular dependency detection
**Problem**: Mutual `Depends on: #N` creates permanent deadlock.
**Fix**: Before evaluating dependencies in `OnPRMerged`, build adjacency graph and detect cycles. Post warning comment on affected issues if cycle found.
**Effort**: 3 hrs

### 2.6 Priority-ordered unblocking
**Problem**: When 5 sub-issues are unblocked simultaneously, all get `ready` at once, causing merge conflicts.
**Fix**: Parse `Priority: N` syntax from issue bodies. Scheduler unblocks one at a time in ascending priority. Without explicit priority, use issue number order.
**Effort**: 4 hrs

### 2.7 Parent issue completion tracking
**Problem**: PM creates sub-issues then stops. No mechanism detects when ALL children are done.
**Fix**: On every PR merge, check if the merged PR's issue has a parent. If all children of that parent are closed, post a completion comment on the parent and close it.
**Effort**: 3 hrs

### 2.8 Stuck-session detector
**Problem**: Sessions stuck in `blocked` or `working` with no progress may never be cleaned up.
**Fix**: Background goroutine (every 30 min) scans `issueStates` for sessions that: have been `in_progress` > 2hrs with no recent turn logs, or have been `blocked` > 6hrs. Posts nudge comments or transitions to `failed:timeout`.
**Effort**: 4 hrs

### 2.9 Extend session recovery window
**Problem**: Recovery only runs on startup with 2hr window. Sessions idle overnight are lost.
**Fix**: Extend window to 24hrs. Add periodic recovery runner (every 1hr while running).
**Effort**: 1 hr

---

## Phase 3: Production Hardening (8-12 hours)

### 3.1 Deduplicate `escapeRepoPath`
**Problem**: Four identical copies across `forgejo/client.go`, `forgejo_tools.go`, `mergequeue/queue.go`, `scheduler/scheduler.go`.
**Fix**: Move to `internal/forgejo/util.go`, export, remove all local copies.
**Effort**: 20 min

### 3.2 Scheduler retry with backoff on API errors
**Problem**: `isIssueClosed` assumes "not closed" on any API error, keeping issues blocked indefinitely.
**Fix**: Retry 2-3 times with exponential backoff before assuming not-closed. Schedule deferred re-check via timer if all retries fail.
**Effort**: 2 hrs

### 3.3 Path traversal test hardening — `local_tools.go`
**Problem**: `..` traversal in `read_file`/`write_file` not explicitly tested.
**Fix**: Add test cases: `repo/../../../etc/passwd`, `../../../etc/shadow`. Consider `filepath.Rel` as defense-in-depth.
**Effort**: 30 min

### 3.4 Concurrency integration test
**Problem**: No test for 10 simultaneous webhook events on the same repo.
**Fix**: New integration test that fires 10 issues simultaneously, verifies all sessions created, no duplicates, no data races.
**Effort**: 4 hrs

### 3.5 Label API error handling in `AddIssueLabels`
**Problem**: `CreateLabel` fallback silently swallows non-422 errors.
**Fix**: Return error if label creation fails with status other than 422.
**Effort**: 15 min

---

## Phase 4: sandboxed.sh-Inspired Improvements (15-25 hours)

### 4.1 Persistent sessions (from sandboxed.sh)
**Problem**: Fordjent spawns new LLM calls per turn. Long-running bash commands (`npm install`, `go test ./...`) die between turns because the process exits.
**Solution**: Keep a persistent CLI process per session with open stdin. Agent sends commands via stdin, reads output asynchronously. This survives across turns.
**Effort**: 8 hrs (major architectural change)

### 4.2 Git-backed configuration library
**Problem**: FSM prompts, role instructions, tool descriptions, and agent behaviors are hardcoded in Go. Any change requires a rebuild and redeploy.
**Solution**: Externalize agent configuration to a Forgejo repo (e.g., `fjadmin/fordjent-config`). On session start, clone/pull the config repo. Prompts, skill instructions, tool descriptions loaded from YAML/markdown files. Changes reviewed via PR. Hot-reload on push webhook.
**Effort**: 10 hrs

### 4.3 Provider fallback chains
**Problem**: When a provider times out (429, 503, context deadline exceeded), Fordjent retries the same provider. If the provider is down, the session fails.
**Solution**: In `provider/client.go`, accept a list of providers per role. On non-retryable failure after max retries, try the next provider in the chain. Config:
```yaml
role_providers:
  implementer:
    - "wafer-qwen"
    - "ollama-cloud"
  reviewer:
    - "wafer-glm"
    - "kimi-k2.6"
```
**Effort**: 4 hrs

### 4.4 Workspace templates per project
**Problem**: Fordjent uses a single Docker image. Every session on a Go project burns tokens running `go mod download`. Sessions on Node.js projects fail because `npm` isn't installed.
**Solution**: Detect project type from file presence (`go.mod` → Go template, `package.json` → Node template). Templates specify base image, pre-installed packages, and init scripts. Docker compose starts per-template containers.
**Effort**: 8 hrs (requires Dockerfile changes, new template config)

### 4.5 OS-level sandboxing inside Docker (from sandboxed.sh)
**Problem**: Fordjent's `bash` tool runs with full container access. Prompt injection could write to `~/.bashrc`, exfiltrate keys, or access other sessions.
**Solution**: Add bubblewrap inside the Docker container. Every `bash` execution runs in a new user namespace with:
- Filesystem: bind-mount only `repoDir/` as read-write, everything else read-only or inaccessible
- Network: proxy through allowlisted domains only (Forgejo API, LLM endpoints)
- No access to `/var/lib/fordjent/work/` (other sessions)
**Effort**: 6 hrs

### 4.6 SSE streaming endpoint (from sandboxed.sh)
**Problem**: Only `/status` (polling) and raw JSONL logs. No real-time visibility into agent progress.
**Solution**: Add `GET /stream?session_key=X` endpoint that pushes SSE events: `status`, `thinking`, `tool_call`, `tool_result`, `assistant_message`, `error`. Separate transcript (user-facing) from trace (tool execution) from debug (protocol noise).
**Effort**: 4 hrs

### 4.7 Encrypted secrets management
**Problem**: Forgejo tokens, API keys in plaintext `.env`. No encryption at rest.
**Solution**: AES-256-GCM encrypted secrets file. Key derived from environment variable. Decrypt at startup. Optionally: Forgejo-backed secrets via `fjadmin/fordjent-secrets` private repo.
**Effort**: 3 hrs

---

## Phase 5: Multi-Role Workflow Completion (8-15 hours)

### 5.1 PM session re-activation
**Problem**: PM creates sub-issues then exits. Cannot answer implementer questions or revise decomposition.
**Fix**: PM sessions persist in "waiting" state with 24hr idle timeout. On sub-issue completion or comment from implementer, PM session re-activates. PM tool set includes `forgejo_get_sub_issues` and `forgejo_summarize_completion`.
**Effort**: 6 hrs

### 5.2 Implementer → PM feedback channel
**Problem**: Implementer has no way to ask the PM clarifying questions.
**Fix**: `forgejo_ping_parent` tool that posts a comment on the parent issue with `@fordjent-bot` mention. This triggers PM session re-activation.
**Effort**: 2 hrs

### 5.3 Reviewer parent context
**Problem**: Reviewer sees PR code but not the parent issue's overall requirements.
**Fix**: If PR body contains `Closes: #N` or `Fixes: #N`, fetch the parent issue body + comments and include in reviewer `buildContext`.
**Effort**: 2 hrs

### 5.4 Role detection from issue body (not just labels/title)
**Problem**: Only title tags and `role:*` labels are checked. Issue body content ignored.
**Fix**: `detectRoleFromIssue` also scans issue body for role keywords. Fallback only when both title and body have no role hints.
**Effort**: 1 hr

### 5.5 Background ready-issue scanner
**Problem**: Issues with `ready` label but no active session sit forever.
**Fix**: Periodic goroutine (every 5 min) scans for open issues with `ready` label and no active session. Creates sessions for any found.
**Effort**: 2 hrs

---

## Execution Order

```
Phase 0 (today, 1 hr)
  └─ fix 3 critical safety bugs

Phase 1 (today, 3 hrs)
  └─ fix 8 high-priority bugs

Phase 2 (this week, 12-20 hrs)
  └─ coordination architecture (context sharing, claim protocol, reviewer coverage)

Phase 3 (next week, 8-12 hrs)
  └─ production hardening (dedup, concurrency tests, retry hardening)

Phase 4 (next sprint, 15-25 hrs)
  └─ sandboxed.sh-inspired features (persistent sessions, config library, sandboxing)

Phase 5 (future sprint, 8-15 hrs)
  └─ complete multi-role workflow (PM re-activation, feedback channels, background scanning)
```

---

## Total Estimates

| Phase | Items | Effort |
|-------|-------|--------|
| 0 — Critical | 3 | 1 hr |
| 1 — High | 8 | 3 hrs |
| 2 — Coordination | 9 | 12-20 hrs |
| 3 — Hardening | 5 | 8-12 hrs |
| 4 — sandboxed.sh | 7 | 15-25 hrs |
| 5 — Multi-role | 5 | 8-15 hrs |
| **Total** | **37** | **47-76 hrs** |

---

*Plan generated from: CODE_REVIEW_ANALYSIS.md (4-agent parallel review), sandboxed.sh research, and live testing session on May 13-18, 2026.*
