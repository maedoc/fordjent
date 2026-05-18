# Fordjent — Next Session Plan

**Date**: May 18, 2026
**Prerequisite**: Phases 0-2 complete (commit `8498972`)

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

## Phase 4.5: OS-Level Sandboxing (6 hours)

### 4.5 OS-level sandboxing inside Docker (from sandboxed.sh)
**Problem**: Fordjent's `bash` tool runs with full container access. Prompt injection could write to `~/.bashrc`, exfiltrate keys, or access other sessions.

**Solution**: Add bubblewrap inside the Docker container. Every `bash` / tool execution runs in a new user namespace.

**Sandbox profile for each execution**:

#### Filesystem
- `repoDir/` — bind-mount read-write
- `/tmp/` — private tmpfs (per-execution, discarded after)
- Everything else — read-only or inaccessible
- No access to `/var/lib/fordjent/work/` (other sessions' clones)
- No access to `/etc/fordjent/` (config with tokens)

#### Network
- Proxy through allowlisted domains only:
  - Forgejo API (container-local: `http://forgejo-local:3000`)
  - LLM provider endpoints (wafer.ai, ollama.com)
  - Block all other outbound
- Block inbound connections

#### Process
- New PID namespace (can't see other session processes)
- No new privileges (`--no-new-privileges`)
- Drop all capabilities

#### Implementation
- `internal/sandbox/` package
- `sandbox.Run(repoDir string, cmd string, allowedHosts []string)` — wraps `bwrap`
- Integrate into `bash` tool, `git` tool, `read_file`, `write_file`
- `Dockerfile` installs `bubblewrap`
- Fallback: if bubblewrap not available, log warning and run with restricted permissions only

**Effort**: 6 hrs

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
Phase 3 (first, 8-12 hrs)
  └─ dedup, retry hardening, path traversal tests, concurrency test, label errors

Phase 4.5 (second, 6 hrs)
  └─ bubblewrap sandboxing for bash/tool execution

Phase 5 (third, 8-15 hrs)
  └─ PM re-activation, feedback channels, reviewer context, body-based role detection,
     background scanner
```

---

## Total Estimates

| Phase | Items | Effort |
|-------|-------|--------|
| 3 — Hardening | 5 | 8-12 hrs |
| 4.5 — Sandboxing | 1 | 6 hrs |
| 5 — Multi-role | 5 | 8-15 hrs |
| **Total** | **11** | **22-33 hrs** |

---

*Phases 0-2 completed May 18, 2026. Phases 4.1-4.4 and 4.6-4.7 deferred to a later sprint.*
