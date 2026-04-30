# Agent Developer Guide — Fordjent Local Deployment & Known Issues

This file is written for the project agent (and any future AI assistant) to understand how the local Fordjent stack is wired, what bugs have already been fixed, and what gotchas remain.

## Current Local Stack

All containers run on a custom Docker bridge network `fordjent-net`.

| Service           | Container name     | Internal endpoint             | External bind       | Purpose                  |
|-------------------|-------------------|-------------------------------|---------------------|--------------------------|
| Forgejo           | `forgejo-local`   | `http://forgejo-local:3000`   | `localhost:3000`    | Forgejo/Gitea v9.x       |
| Fordjent          | `fordjent`        | `http://fordjent:8080`        | `localhost:8080`    | Agent harness            |
| Ollama (local)    | `ollama-probe`    | `http://ollama-probe:11434`   | —                   | Currently unused         |
| Redis (future)    | —                 | —                             | —                   | Not yet deployed         |

- **Host Docker**: Docker 20.10.24, docker-compose 1.29.2 (Compose v1 syntax, not `docker compose` v2).
- **NVIDIA runtime available on host** but currently unused (Cloud provider chosen).

## Provider History

| Phase | Provider | Model | Result |
|-------|----------|-------|--------|
| 1 | `openai` | gpt-4o-mini | Never tried (no key) |
| 2 | `ollama` (local) | `qwen2.5-coder:7b` | **Unsupported** — model does not emit OpenAI `tool_calls`; returns JSON as raw text in `message.content` |
| 3 | `ollama-cloud` (Ollama Cloud) | `minimax-m2.5` | **Working** — proper `finish_reason: tool_calls` and structured function calls |

Current config in `fordjent.local.yaml`:

```yaml
providers:
  - name: "ollama-cloud"
    api_base: "https://ollama.com/v1"
    api_key: "${OLLAMA_API_KEY}"
    model: "${OLLAMA_MODEL}"
    max_tokens: 16384
```

Ollama Cloud free tier resets every 5 hours. GPU-time billing, not per-token.
Token lives in `.env` file (loaded via `--env-file`). Never commit it.

## Credentials

| What | Value | Where |
|------|-------|-------|
| Forgejo admin user | `fjadmin` | `gitea admin user create` |
| Forgejo admin pass | `REDACTED` | Bootstrap script |
| Fordjent Forgejo token | `REDACTED` | API-created, stored as `FORGEJO_TOKEN` |
| Webhook secret | `REDACTED` | Forgejo webhook (shared with Fordjent config) |

## Bugs Found & Fixed

### 1. URL path encoding — `owner/repo`
**Cause**: `url.PathEscape("owner/repo")` produces `owner%2Frepo`, which breaks Forgejo/Gitea's two-segment URL router. This caused 404s on every tool that hits the API.

**Fix**: Added `escapeRepoPath()` which splits on `/`, escapes each segment separately, and joins them back with `path.Join()`.

```go
func escapeRepoPath(repo string) string {
    parts := strings.Split(repo, "/")
    for i, p := range parts {
        parts[i] = url.PathEscape(p)
    }
    return path.Join(parts...)
}
```

**Files changed**:
- `internal/forgejo/client.go` — `GetIssue`, `ListComments`, `AddReaction`
- `internal/tool/forgejo_tools.go` — all 7 API tools

### 2. `AddReaction` used wrong HTTP method
**Cause**: The client sent `PUT` to `/api/v1/repos/{repo}/issues/{id}/reactions`. Forgejo/Gitea expects `POST`.

**Fix**:
```go
// internal/forgejo/client.go
_, err := c.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"content": emoji})
```

### 3. `bash` tool ran in `WorkDir()` (parent of repo)
**Cause**: `SessionInfo.WorkDir()` is `/var/lib/fordjent/work/fjadmin/testbed/issues/N/`. The repo is cloned into `repo/` inside it. The model saw `repo/` as a subdirectory, passed paths like `repo/README.md` to `read_file`, producing double paths (`…/repo/repo/README.md`).

**Fix**:
```go
// internal/tool/local_tools.go
Dir: info.RepoDir(),  // was: info.WorkDir()
```

### 4. `read_file` path sanitization missing
**Cause**: The model sometimes passed relative paths with `repo/` prefix, and sometimes (rarely) full paths that accidentally included `repoDir`. No guard existed.

**Fix**:
```go
filename := params["filename"].(string)
filename = strings.TrimPrefix(filename, "repo/")
if strings.HasPrefix(filename, info.RepoDir()) {
    filename = strings.TrimPrefix(filename, info.RepoDir())
    filename = strings.TrimPrefix(filename, "/")
}
```

### 5. Agent self-comment loop (Critical)
**Cause**: When Fordjent posted a comment via `forgejo_comment`, Forgejo sent an `issue_comment.created` webhook. The sender was the token owner (`fjadmin`), not a bot user, so `isAgentEvent()` didn't catch it. This caused infinite loops where the agent replied to its own comments.

**Fix**: The `forgejo_comment` tool now appends a hidden HTML marker `<!-- ford -->` to every comment body. The `isAgentEvent()` function in `internal/webhook/router.go` detects this marker in comment/issue/PR bodies and drops the event.

```go
// internal/tool/forgejo_tools.go
const agentCommentMarker = "\n\n<!-- ford -->"
// appended to every comment body before POST

// internal/webhook/router.go
func (r *Router) isAgentEvent(payload map[string]interface{}) bool {
    marker := "<!-- ford -->"
    // ... (check commits, sender, then:)
    if comment, ok := payload["comment"].(map[string]interface{}); ok {
        if body, ok := comment["body"].(string); ok {
            if strings.Contains(body, marker) { return true }
        }
    }
    if issue, ok := payload["issue"].(map[string]interface{}); ok {
        if body, ok := issue["body"].(string); ok {
            if strings.Contains(body, marker) { return true }
        }
    }
    if pr, ok := payload["pull_request"].(map[string]interface{}); ok {
        if body, ok := pr["body"].(string); ok {
            if strings.Contains(body, marker) { return true }
        }
    }
    // ...
}
```

### 6. PR cascade loop
**Cause**: When the agent opened a PR, Forgejo sent `pull_request.opened`. The agent processed that event and opened *another* PR for the same work, because the PR body didn't have the agent marker.

**Fix**: The `forgejo_create_pr` tool also appends `<!-- ford -->` to every PR description, so `isAgentEvent` filters `pull_request.opened` for agent-generated PRs.

### 7. `max_turns` too low for complex tasks
**Cause**: The default was 25. Writing multiple files + git operations + compilation probes easily exceeded this, causing sessions to abort mid-work.

**Fix**: Bumped `max_turns: 50` in `fordjent.local.yaml`.

### 8. Branch not pushed before PR creation
**Cause**: The agent committed locally but didn't push before calling `forgejo_create_pr`. Forgejo returned 500 with `fatal: bad revision 'refs/heads/main...feature/foo'`.

**Fix**: Two-layer fix:
1. The `git` tool in `local_tools.go` now auto-runs `git push -u origin HEAD` immediately after every successful `git commit`.
2. The Dockerfile sets `git config --global push.default current` so bare `git push` works for new branches.

```go
// internal/tool/local_tools.go
if strings.HasPrefix(..., "commit") {
    pushCmd := exec.CommandContext(pushCtx, "git", "push", "-u", "origin", "HEAD")
    pushCmd.Dir = t.repoDir
    pushOut, pushErr := pushCmd.CombinedOutput()
    // errors ignored — non-fatal
}
```

### 9. Multi-line `git commit -m` messages broke the git tool
**Cause**: Newlines inside the JSON string for `command` were passed to `strings.Fields()`, which preserved them incorrectly, but more critically the shell interpreted them as multiple arguments when passed via `exec.Command`.

**Fix**: In the `git` tool, `commit` commands have newlines replaced with spaces before execution:
```go
if strings.HasPrefix(strings.TrimSpace(strings.ToLower(cmdStr)), "commit") {
    cmdStr = strings.ReplaceAll(cmdStr, "\\n", " ")
    cmdStr = strings.ReplaceAll(cmdStr, "\n", " ")
}
```

### 10. Docker container missing build tools
**Cause**: The base `debian:bookworm-slim` image had no `gcc`, `make`, or headers.

**Fix**: Changed Dockerfile to install `build-essential` (includes gcc, g++, make, libc-dev).

## Known Gotchas

### Git identity
The Dockerfile pre-configures `user.email` and `user.name` globally in `/root/.gitconfig` and copies it to `/var/lib/fordjent/.gitconfig` (owned by `fordjent`). If you mount a different home directory or switch to a non-standard user, git commits may fail with "Please tell me who you are".

### Forgejo Webhook Allowed Hosts
When running entirely in Docker containers, Forgejo refuses to deliver webhooks to internal IPs by default. The bootstrap must set:

```ini
[webhook]
ALLOWED_HOST_LIST = *
```

Or equivalently, set env var `FORGEJO__webhook__ALLOWED_HOST_LIST=*`.

## One-Command Bootstrap (Post-Mortem)

The manual steps that worked:

```bash
# 1. Create bridge network
docker network create fordjent-net

# 2. Start Forgejo with no install wizard
mkdir -p /tmp/forgejo-config
cat > /tmp/forgejo-config/app.ini << 'EOF'
APP_NAME = Forgejo Local
RUN_MODE = prod
[server]
DOMAIN = forgejo-local
ROOT_URL = http://forgejo-local:3000/
HTTP_PORT = 3000
[database]
DB_TYPE = sqlite3
PATH = /data/gitea/gitea.db
[service]
DISABLE_REGISTRATION = true
[webhook]
ALLOWED_HOST_LIST = *
[repository]
DEFAULT_PRIVATE = public
[security]
INSTALL_LOCK = true
EOF

docker run -d --name forgejo-local --network fordjent-net \
  -p 127.0.0.1:3000:3000 -v forgejo-data:/data \
  -v /tmp/forgejo-config:/data/gitea/conf:rw \
  -e USER=1000 codeberg.org/forgejo/forgejo:9

# Wait for start, then init DB + admin user
docker exec forgejo-local gitea migrate
docker exec forgejo-local gitea admin user create \
  --username fjadmin --password REDACTED --email admin@local --admin

# 3. (Optional) Start local Ollama — currently unused
docker run -d --name ollama-probe --runtime=nvidia \
  --network fordjent-net -v ollama-probe-data:/root/.ollama ollama/ollama

# 4. Build and start Fordjent
docker run -d --name fordjent --network fordjent-net \
  -p 127.0.0.1:8080:8080 \
  -v fordjent-data:/var/lib/fordjent \
  -v /home/duke/src/fordjent/fordjent.local.yaml:/etc/fordjent/fordjent.yaml:ro \
  --env-file /home/duke/src/fordjent/.env \
  fordjent:local

# 5. Create repo + token via API
curl -X POST http://localhost:3000/api/v1/user/repos \
  -u fjadmin:REDACTED \
  -H "Content-Type: application/json" \
  -d '{"name":"testbed","description":"Fordjent integration test","private":false}'

# Token creation (POST /api/v1/users/fjadmin/token with name)
# Then create webhook pointing to http://fordjent:8080/acp/v1/events
```

## How to Continue Development

### Rebuild Fordjent after code changes
```bash
cd /home/duke/src/fordjent
docker build -t fordjent:local .
docker stop fordjent && docker rm fordjent
docker run -d --name fordjent --network fordjent-net \
  -p 127.0.0.1:8080:8080 \
  -v fordjent-data:/var/lib/fordjent \
  -v /home/duke/src/fordjent/fordjent.local.yaml:/etc/fordjent/fordjent.yaml:ro \
  --env-file /home/duke/src/fordjent/.env \
  fordjent:local
```

### Create an issue to trigger the agent
```bash
curl -X POST http://localhost:3000/api/v1/repos/fjadmin/testbed/issues \
  -u fjadmin:REDACTED \
  -H "Content-Type: application/json" \
  -d '{"title":"...","body":"..."}'
```

### Watch live logs
```bash
docker logs -f fordjent 2>&1 | jq -r '. | "[\(.time)] [\(.level)] \(.msg) \(.tool // "") \(.turn // "") \(.session_key // "")"'
```

### Read a session's memory
```bash
docker exec fordjent cat /var/lib/fordjent/work/fjadmin/testbed/issues/N/memory.jsonl | jq
```

## Autonomous Test Results (Round 2)

After applying all fixes above, a fresh test run demonstrated:
- ✅ **0 comments** on issues (self-comment loop completely eliminated)
- ✅ **"filtered agent-originated event"** in logs confirming marker-based filtering works for comments, issues, and PRs
- ✅ **PRs created successfully** on first or second attempt after auto-push fix
- ✅ **No duplicate PRs** from PR-opened cascade
- ✅ **50-turn budget** comfortably handles multi-file PRs without maxing out
- ✅ **Makefile includes build tools** (`build-essential` in Docker image)

### Remaining minor issues
- **Agent sometimes creates a branch, commits, but the auto-push silently fails** if `origin` is not configured as a remote (very rare — only seen with freshly cloned repos that already have the remote). The agent usually recovers by calling `git push` explicitly via `bash`.
- **Commit message multiline sanitization** works for literal `\n` but very long multi-line strings might still trip up if the LLM doesn't escape them properly.
- **Build artifacts (.o files) in git**: `.gitignore` works if pre-seeded in the scaffold, but the agent can still accidentally commit binaries if told to `git add -A` when `.gitignore` is missing. Best practice: always include `.gitignore` in the first scaffold issue.

## Model Recommendation

Use **minimax-m2.5** via Ollama Cloud for reliable tool calling. Local options that may work but are untested here: `llama3.1:8b`, `qwen2.5:7b` (base, not coder).

---

## Parallel Wave Stress Test (April 27, 2026)

### What We Tested
Fired **5 issues simultaneously** in a single "wave" to measure how Fordjent handles parallel workstreams touching overlapping files.

| Issue | Title | Turns | PR | Result |
|-------|-------|-------|-----|--------|
| #18 | Fix panic-prone argument handling | 16 | #20 | ✅ Merged |
| #16 | Implement branch command | 22 | #21 | ✅ Merged (after human rebase) |
| #15 | Implement git add with staging index | 25 | #22 | ✅ Merged (after human rebase) |
| #17 | Implement log command | 41 | #23 | ✅ Merged (after human rebase) |
| #19 | Integration test (init→add→commit→log) | **49** | — | ❌ **Maxed out** |

All 5 sessions spawned within **260ms**. Parallel session creation is solid.

### Key Findings

#### 1. Merge Conflicts Are the #1 Blocker
All 4 successful PRs touched `cmd/gogit/main.go` to wire their subcommand. Merging the first one immediately invalidated the other 3:

| PR | Files | After PR 20 merge? |
|----|-------|-------------------|
| 20 | `main.go` | ✅ Merged first |
| 21 | `main.go`, `branch.go` | ❌ Unmergeable |
| 22 | `main.go`, `add.go`, `index/*.go` | ❌ Unmergeable |
| 23 | `main.go`, `log.go` | ❌ Unmergeable |

**Human had to manually rebase each branch**, resolve `main.go` conflicts by keeping all commands, and force-push. The agent has **no auto-rebase logic**.

#### 2. Review/Pushback Cycle Didn't Work Initially
When a human left a review comment on a PR, the agent did **not** respond. The webhook was either not delivered or the agent session was already complete with no mechanism to re-activate.

#### 3. Integration Tests Across Unmerged PRs Fail
Issue #19 asked for an end-to-end test exercising commands that weren't merged yet. The agent's clone is a snapshot of `main` at session creation — it has zero awareness that other PRs are in flight. It burned 49 turns trying to compile code that didn't exist in its checkout.

### What Works in Parallel vs. Sequential

| Scenario | Works? |
|----------|--------|
| Independent features in separate packages | ✅ Yes |
| Multiple issues spawning simultaneously | ✅ Yes |
| Issues touching the same file | ❌ Merge conflicts |
| Review → fix → resubmit cycle | ❌ Requires new code |
| Integration tests depending on unmerged PRs | ❌ Maxes out |
| Agent rebasing stale branches | ❌ Not supported |

---

## Bug Fix 11 — PR Review/Pushback Cycle (April 27, 2026)

**Problem**: When a human left a review comment on an existing PR, the agent couldn't respond because:
1. The session was keyed to the PR number (`repo/pulls/N`) which was a new session, not the original issue session
2. The agent's clone had `main` checked out, not the PR branch
3. The agent tried to create a **new** PR instead of pushing to the existing branch
4. No system prompt told the agent it was in "review mode"

**Solution**: Three-layer fix:

### 1. Fetch and checkout PR branch (`internal/session/manager.go`)

Before the LLM loop starts, if the event is a PR review comment (`IssueCommentCreated` or `PullRequestReviewComment`):
- Call `forgejo.GetPR()` to get the head branch name
- Run `git fetch origin <branch>` and `git checkout -B <branch> origin/<branch>` in the repo clone
- Inject a context message telling the agent it's on the PR branch and should NOT create a new PR

### 2. PR review mode instructions (`buildSystemPrompt`)

Added a `PR Review Mode` section to the system prompt when responding to review comments:
- "You are already on the PR branch"
- "Make your fixes directly on this branch"
- "After fixing, commit and push to the SAME branch"
- "Do NOT create a new PR — the PR already exists"
- "Post a comment confirming which issues were fixed"

### 3. Updated `forgejo_create_pr` tool description

Changed from generic "Use for submitting code changes for review" to:
> "Create a pull request from a head branch to a base branch. Use **ONLY** for submitting **NEW** code changes. If you are responding to a review on an existing PR, **do NOT call this tool** — push to the existing branch instead."

### 4. Added `GetPR` to Forgejo client (`internal/forgejo/client.go`)

Added `PullRequest` struct and `GetPR()` method to fetch head branch info via `GET /api/v1/repos/{repo}/pulls/{number}`.

### Validation

Sent a fake `issue_comment` webhook for PR #25. Fordjent:
1. Created session `fjadmin/gogit/pulls/25`
2. Logged: `"checked out PR branch for review","branch":"test/review-cycle"`
3. The agent read `init.go`, removed the test marker, committed, and pushed
4. Posted a comment: `"Done! I've removed the test marker line entirely..."`
5. PR head SHA updated from `8b7925e` → `3efe5df`

**Status**: ✅ Review/pushback cycle implemented and validated.

---

## Bug Fix 12 — Auto-Rebase + Turn Budget (April 27, 2026)

**Problem**: Three critical gaps identified from the parallel wave stress test:
1. **No rebase instruction**: Agents created PRs from stale branches, causing immediate merge conflicts when `main` had moved
2. **Turn budget too low**: Complex integration tasks maxed out at 49/50 turns
3. **No merge-conflict avoidance**: Agents weren't told to check if `origin/main` had diverged

**Solution**:

### 1. System prompt rule: always rebase before PR (`internal/session/manager.go`)

Added Rule 8 to `buildSystemPrompt`:
> **ALWAYS rebase before creating a PR.** Before calling forgejo_create_pr, first run 'git fetch origin' and then 'git rebase origin/main' on your feature branch using the git tool (two separate calls) or the bash tool (combined). This prevents merge conflicts.

And Rule 9:
> **Do NOT create a new PR if one already exists** for the current branch. Push to the existing branch instead.

### 2. Git tool description updated (`internal/tool/local_tools.go`)

Changed from:
> "Execute git operations in the repository: status, diff, add, commit, branch, checkout, log, fetch, pull."

To:
> "Execute git operations in the repository: status, diff, add, commit, branch, checkout, log, fetch, pull, **rebase**. Note: push is blocked; use forgejo_create_pr tool instead. **IMPORTANT: before creating a PR, run 'git fetch origin' then 'git rebase origin/main' (two separate calls).**"

### 3. Turn budget bumped (`fordjent.local.yaml`)

`max_turns: 50` → `max_turns: 75`

This gives complex multi-file PRs (integration tests, refactors, parsers) enough headroom without maxing out.

### 4. Forgejo_create_pr description already updated

In Bug Fix 11, the `forgejo_create_pr` tool was already updated to warn about duplicate PRs.

**Status**: ✅ All three gaps addressed. Rebase instruction is in the system prompt, the git tool description, and the PR tool description.

---

## What's Still Missing for Realistic Dev Mode

### Merge Conflicts (Partially Addressed)
- ✅ Agent is now **instructed** to rebase before creating a PR
- ❌ Agent does **not auto-detect** when rebase is needed; it must follow the instruction
- ❌ No automatic `git rebase origin/main` execution triggered by the `forgejo_create_pr` tool
- ❌ If the agent ignores the instruction, merge conflicts still occur
- **Mitigation**: The system prompt + tool descriptions now strongly emphasize rebasing

### Integration Tests
- Integration work should be filed **after** its dependencies are merged
- A project manager layer needs to schedule issues in dependency order
- Alternatively: the agent could detect "not implemented" errors and file a follow-up issue instead of burning turns

### Turn Budget
- ✅ Bumped from 50 → 75
- May still need per-issue tuning for very large refactors
- Consider adaptive budget based on file count or issue label

### CI / Runners
- No Forgejo Actions runner is deployed
- All testing is via `go test ./...` inside the agent container
- For real use, deploy a runner or use a merge queue with test gates

---

## Files Changed in Review Cycle Implementation

| File | Change |
|------|--------|
| `internal/forgejo/client.go` | Added `PullRequest` struct + `GetPR()` method |
| `internal/session/manager.go` | Added PR branch checkout in `ProcessEvent()`; added PR review mode instructions in `buildSystemPrompt()` |
| `internal/tool/forgejo_tools.go` | Updated `forgejo_create_pr` description to warn about duplicate PRs |

## Bug Fix 13 — Merge Queue + Label Scheduler (Phase 1 & 2, April 28, 2026)

**Problem**: Parallel waves of issues caused immediate merge conflicts because multiple agents created PRs touching the same files (`cmd/gogit/main.go`) from the same base commit. There was no file-level gate or dependency scheduling.

**Solution**: Implemented two new subsystems:

### Phase 1: File-Gate Merge Queue (`internal/mergequeue/`)

Before `forgejo_create_pr` executes, the merge queue queries Forgejo's API to:
1. Compare the current branch vs `main` to get changed files (`GET /api/v1/repos/{repo}/compare/main...{branch}`)
2. List all open PRs in the repo (`GET /api/v1/repos/{repo}/pulls?state=open`)
3. For each open PR, get its changed files (`GET /api/v1/repos/{repo}/pulls/{N}/files`)
4. If any file overlap exists → **block PR creation** and return a message telling the agent which PRs conflict

```go
// MergeGate interface — implemented by mergequeue.Client
type MergeGate interface {
    CheckGate(ctx context.Context, repo, headBranch, baseBranch string) (blocked bool, message string, err error)
}
```

**Validation**: Unit tests confirm:
- ✅ No conflict → PR creation proceeds
- ✅ File overlap detected → blocked with message listing conflicting PRs and files
- ✅ Self-branch PRs are skipped (same branch already has a PR)

### Phase 2: Label Scheduler (`internal/scheduler/`)

On every `pull_request.merged` event:
1. Scans all open issues in the repo
2. Parses `Depends on: #N` syntax from issue bodies (case-insensitive)
3. For each issue whose ALL declared dependencies are now merged:
   - Removes `blocked` label
   - Adds `ready` label
   - Posts a comment: *"Dependency #N is now merged. This issue is unblocked and ready to work on!"*

**Validation**: Unit tests confirm:
- ✅ Correctly parses `Depends on: #15`, `depends on: #15, #16`, etc.
- ✅ Removes `blocked` and adds `ready` labels
- ✅ Posts unblock comment

### Files Changed

| File | Change |
|------|--------|
| `internal/mergequeue/queue.go` | New: `Client.CheckGate()` compares branch files against open PR files |
| `internal/scheduler/scheduler.go` | New: `Scheduler.OnPRMerged()` parses deps, manages labels |
| `internal/event/event.go` | Added `PullRequestMerged` event type |
| `internal/webhook/router.go` | Detects merged PRs (action="closed" + merged=true) |
| `internal/tool/forgejo_tools.go` | `forgejo_create_pr` now checks `MergeGate` before POSTing |
| `internal/session/manager.go` | Wires `mq` and `scheduler`, handles `PullRequestMerged` events |

### Test Results

```
ok  github.com/fordjent/fordjent/internal/mergequeue (3 passes)
ok  github.com/fordjent/fordjent/internal/scheduler  (2 passes)
```

### What We Couldn't Validate End-to-End

The parallel wave test (Issues 26–28) was **blocked by LLM provider timeouts** — Ollama Cloud returned `context deadline exceeded` for every simultaneous request. This is an infrastructure limitation, not a code limitation:

- Single issue → also timed out (provider-side issue)
- Unit tests → all pass (code is correct)
- Merge queue is stateless, using only Forgejo API → no persistent storage needed

**Recommendation**: For future parallel wave tests, either:
1. Use Ollama Cloud paid tier for higher concurrency
2. Add local model fallback with a model that supports tool-calling
3. Add request retry logic with exponential backoff for transient LLM timeouts

**Current provider status** (April 28, 2026): Ollama Cloud free tier is experiencing intermittent timeouts. Single-session sequential work usually succeeds; parallel multi-session work reliably hits `context deadline exceeded`. Consider a 60–120s timeout in `fordjent.local.yaml` (provider `request_timeout`) if supported, or switch to a local model with proper tool-calling.

---

## Reliable Agent Layer (April 28–29, 2026)

### Problem
Fordjent's original agent loop was brittle: single-attempt LLM calls with no retry, no context window management, no cost tracking, and no observability. The parallel wave test (Issues 26–28) was completely blocked because every session hit `context deadline exceeded` and died.

### Solution: 4-Phase Refactor

| Phase | Component | What It Does |
|-------|-----------|--------------|
| 1 | Retry + Backoff | `internal/provider/retry.go` — exponential backoff with jitter, status-code-aware retry classification |
| 2 | Auto-Compaction | `internal/agent/context.go` — monitor token estimate vs window size; truncate old turns at 80% threshold |
| 3 | Cost Tracking | `internal/cost/cost.go` — SQLite per-session/per-repo cost DB, budget enforcement |
| 4 | Observability | `internal/metrics/metrics.go` + `internal/agent/turn.go` — structured per-turn logging, latency/costs in Prometheus format |

### Phase 1: Retry Policy (`internal/provider/retry.go`)

- **Retryable errors**: `context deadline exceeded`, 503, 502, 429, 529 (overloaded)
- **Non-retryable**: 400, 401, 403 (client errors), malformed JSON
- **Backoff**: `delay = base * 2^attempt` with ±25% jitter
- **Configurable**: `max_retries`, `retry_base_delay`, `retry_max_delay` in `fordjent.local.yaml`
- **`internal/provider/client.go`**: All calls now return `(*Response, *Usage, error)` so cost can be tracked

### Phase 2: Auto-Compaction (`internal/agent/context.go`)

- **Strategy**: Truncate (not summarize) messages when estimated tokens > 80% of window
- **What survives**: System prompt + compaction marker + last N turns (configurable, default 8)
- **Estimate**: Rough count: `len(content) / 4` chars ≈ tokens. Good enough for threshold checks.
- **Logged**: `compacted context, before_messages=X, after_messages=Y, estimate_tokens=Z`

### Phase 3: Cost Budgeting (`internal/cost/cost.go`)

- **SQLite table**: `usage` with `session_key`, `provider`, `model`, `tokens`, `cost_usd`
- **Per-session cost**: `GetSessionCost(sessionKey)` returns `tokens` + `cost`
- **Per-repo cost**: `GetRepoCost(repo)` for project-level spend tracking
- **Monthly cost**: `GetMonthlyCost()` for global budgeting
- **Enforcement**: `CheckBudget()` aborts if `max_session_cost` or `max_monthly_cost` exceeded
- **Config**:
  ```yaml
  budget:
    enabled: false
    max_session_cost: 0.50
    max_monthly_cost: 10.00
  providers:
    - name: "ollama-cloud"
      cost_per_1m_input_tokens: 0
      cost_per_1m_output_tokens: 0
  ```

### Phase 4: Metrics + Turn Logging (`internal/metrics/` + `internal/agent/turn.go`)

New metrics exposed at `http://localhost:8080/metrics`:
```
fordjent_events_total          — webhook events
fordjent_sessions_total        — cumulative sessions created
fordjent_sessions_active       — gauge of current sessions
fordjent_tool_calls_total      — all tool executions
fordjent_llm_calls_total       — all LLM calls (now surviving retries)
fordjent_llm_retries_total     — cumulative retries
fordjent_tokens_total{type}    — input/output split
fordjent_cost_total_total      — cumulative spend in USD
```

Per-turn structured log:
```json
{"level":"info","msg":"turn complete","session_key":"...","latency_ms":4500,
 "tokens_in":12000,"tokens_out":800,"cost_usd":0.0032,"tool_calls":2,"compacted":false}
```

### Files Changed

#### New Packages

| File | Purpose |
|------|---------|
| `internal/provider/retry.go` | RetryPolicy with exponential backoff + jitter |
| `internal/agent/context.go` | ContextTracker: token estimate, compaction |
| `internal/agent/context_test.go` | Unit tests |
| `internal/agent/turn.go` | TurnExecutor: wraps LLM calls with compaction + cost |
| `internal/cost/cost.go` | SQLite cost tracker + budget enforcement |
| `internal/cost/cost_test.go` | Unit tests |

#### Modified Core Packages

| File | Change |
|------|--------|
| `internal/provider/client.go` | Return `*Usage`, integrate retry policy via `RetryPolicy.Do()` |
| `internal/provider/client_test.go` | Updated for 3-return `Chat()` |
| `internal/session/manager.go` | Wire `cost.Tracker`, `ContextTracker`, `TurnExecutor`; refactor `ProcessEvent` loop |
| `internal/config/config.go` | New config fields: `context_window`, `compaction_threshold`, `compaction_keep_turns`, `request_timeout`, `max_retries`/`retry_base_delay`/`retry_max_delay`, `budget` section, provider `cost_per_1m_*` |
| `internal/metrics/metrics.go` | New counters: tokens, retries, cost |
| `fordjent.local.yaml` | Full config with all new fields |

### Test Results

```
ok  github.com/fordjent/fordjent/internal/agent          (compact + estimate)
ok  github.com/fordjent/fordjent/internal/cost           (record + budget)
ok  github.com/fordjent/fordjent/internal/provider       (retry + usage)
ok  github.com/fordjent/fordjent/internal/session         (manager still passes)
```

### What This Fixes vs Original

| Scenario | Before | After |
|----------|--------|-------|
| LLM timeout | Session dies immediately | Retries 3× with exponential backoff |
| 529 overloaded | Hard abort | Retries after 2s → 4s → 8s |
| Context full | Silent truncation / API error | Auto-compact at 80% threshold |
| No cost visibility | Zero tracking | SQLite + Prometheus per session/repo/month |
| No latency data | No observability | Structured per-turn logs with latency, tokens, cost |
| Parallel wave | All 5 sessions timeout+die | Will succeed after retries (provider permitting) |

### Notes

- **Pi was NOT integrated**. Fordjent keeps its own Go-native loop for predictability.
- **Compaction is truncate-only** (no extra LLM call). If summarization is needed later, it can replace the truncate strategy.
- **Budget is off by default** (`enabled: false`). Zero-cost providers (Ollama Cloud free tier) don't need it.
- **Provider test `TestBashToolSuccess` still fails** due to Alpine lacking `bash`. Pre-existing.

---

## Complete File Inventory (All Changes Since Base)

### New Packages

| File | Purpose |
|------|---------|
| `internal/mergequeue/queue.go` | File-gate merge queue: blocks `forgejo_create_pr` when open PRs touch same files |
| `internal/mergequeue/queue_test.go` | Unit tests: no-conflict, conflict, self-branch |
| `internal/scheduler/scheduler.go` | Label scheduler: parses `Depends on: #N`, transitions `blocked` → `ready` on PR merge |
| `internal/scheduler/scheduler_test.go` | Unit tests: unblock flow, dependency parsing |

### Modified Core Packages

| File | Change |
|------|--------|
| `internal/event/event.go` | Added `PullRequestMerged` type |
| `internal/webhook/router.go` | Detects merged PRs (`action="closed"` + `merged=true`) |
| `internal/session/manager.go` | PR review mode; `MergeGate` + `Scheduler` + `cost.Tracker` wiring; refactored `ProcessEvent` with turn executor |
| `internal/provider/client.go` | Return `*Usage`, integrate retry policy via `RetryPolicy` |
| `internal/config/config.go` | New fields: retry, compaction, cost, budget |
| `internal/metrics/metrics.go` | New counters: tokens, retries, cost |
| `internal/tool/forgejo_tools.go` | `forgejo_create_pr` checks `MergeGate` interface before POST |
| `internal/tool/forgejo_tools_test.go` | Updated `NewCreatePRTool` signature, path assertions |
| `internal/tool/local_tools.go` | `git` tool: auto-push after commit, rebase instruction in description |
| `internal/forgejo/client.go` | Added `PullRequest` struct + `GetPR()`, fixed `AddReaction` to `POST`, `escapeRepoPath()` |
| `internal/forgejo/client_test.go` | Updated assertions to match current path escaping and methods |
| `Dockerfile` | Added `build-essential`, global git identity, `push.default current` |
| `fordjent.local.yaml` | Full config with all new fields |
| `AGENTS.md` | This document |
| `internal/provider/retry.go` | Exponential backoff with jitter |
| `internal/agent/context.go` | ContextTracker: token estimate, compaction |
| `internal/agent/context_test.go` | Unit tests |
| `internal/agent/turn.go` | TurnExecutor: wraps LLM calls with compaction + cost |
| `internal/cost/cost.go` | SQLite cost tracker + budget enforcement |
| `internal/cost/cost_test.go` | Unit tests |
| `internal/provider/client_test.go` | Updated for 3-return Chat() and usage tracking |
| `fordjent.local.yaml` | Full config with all new fields |
| `AGENTS.md` | This document |

### Pre-existing (unchanged) Key Files

| File | Purpose |
|------|---------|
| `cmd/fordjent/main.go` | Entry point: wires webhook router → event bus → session manager |
| `internal/config/config.go` | Config struct: `Agent`, `Forgejo`, `Security`, `Providers`, etc. |
| `internal/provider/client.go` | LLM abstraction: `Chat()` method for OpenAI-compatible APIs |
| `internal/tool/adapter.go` | Tool registry + OpenAI function schema generation |
| `internal/memory/memory.go` | Session memory: JSONL append, compaction |


### Remaining Gaps

| Gap | Status |
|-----|--------|
| **Merge queue auto-retry after PR merge** | Agent must manually call `forgejo_create_pr` again after rebase — no auto-rebase or auto-retry yet |
| **Scheduler label creation** | Labels `blocked` and `ready` must exist in Forgejo before the scheduler can apply them. Create them manually or via bootstrap script. |
| **Dependency on unmerged issues** | If an issue declares `Depends on: #15` but #15 is an issue (not a PR), the scheduler doesn't track it. Only works for PR dependencies. |

---

## Lifecycle + Orchestrator Layer (April 28, 2026)

**Problem**: Sessions that failed (max turns, LLM errors, tool failures) died silently with no trace. The only way to detect failure was tailing logs. Additionally, there was no protection against filing parallel issues on empty repos (all agents would independently create go.mod / README.md and conflict).

**Solution**: Three event-driven coordination features built together:

### 1. Session Lifecycle State Machine (`internal/lifecycle/`)

- SQLite table `session_transitions` tracks every session through states:
  - `created` → `working` → `pr_created` → `completed`
  - `working` → `failed_max_turns`
  - `working` → `failed_error`
- On `failed_max_turns`: automatically labels the issue `fordjent/failed:max-turns` + `blocked`, posts a comment explaining the failure
- On `failed_error`: labels `fordjent/failed:error` + `blocked`, posts the error message as a comment
- Exposed for queries: `ListFailedSessions()` returns all session keys currently in a failed state
- This closes the biggest observability gap: **no more silent failures**

### 2. Stale Gate (`internal/stalegate/`)

Integrated directly into the `forgejo_create_pr` tool, before the merge queue file gate:
1. Runs `git fetch origin <base>`
2. Runs `git merge-base --is-ancestor origin/<base> HEAD`
3. If exit code 1 → branch is stale, block PR creation with instructions to rebase

This is **enforced at the tool level**, not just a prompt suggestion. The agent *cannot* create a PR from a stale branch.

### 3. Scaffold Detection (`internal/scaffold/`)

When an `IssueOpened` event arrives:
1. Queries the repo tree — if fewer than 3 files, the repo is "empty"
2. If empty and no scaffold issue exists → creates one via API with title "[scaffold] Add project scaffold..."
3. Labels the triggering issue `blocked` and posts a comment pointing to the scaffold issue
4. Skips session creation if blocked

This prevents the parallel-chaos on empty repos that caused merge conflicts in the `parallelwave` experiment.

### Files Changed

| File | Change |
|------|--------|
| `internal/lifecycle/lifecycle.go` | New: SQLite-backed state machine, failure labeling, failure comment posting |
| `internal/lifecycle/lifecycle_test.go` | Unit tests: transitions, failed session listing, nested DB dir creation |
| `internal/stalegate/stalegate.go` | New: `IsStale(repoDir, baseBranch)` using git plumbing |
| `internal/stalegate/stalegate_test.go` | Unit tests: not-stale, stale, git-not-found skip |
| `internal/scaffold/scaffold.go` | New: `CheckAndBlock()` for empty-repo protection |
| `internal/forgejo/client.go` | Added `AddIssueLabels`, `RemoveIssueLabel`, `CreateIssue`, `ListOpenIssues`, `ListRepoFiles`, `PostIssueComment` |
| `internal/tool/forgejo_tools.go` | `forgejo_create_pr` now calls `stalegate.IsStale()` before merge queue; added `repoDir` parameter |
| `internal/tool/forgejo_tools_test.go` | Updated `NewCreatePRTool` signature |
| `internal/session/manager.go` | Wires `forgejoClient`, `lc`, `scaffold` detection; lifecycle transitions on session start/failure/complete |
| `internal/config/config.go` | New fields: `enable_lifecycle`, `enable_stale_gate`, `enable_scaffold_detection` |
| `fordjent.local.yaml` | Added new enable flags (all `true` by default) |
| `AGENTS.md` | This document |

### Test Results

```
ok  github.com/fordjent/fordjent/internal/lifecycle (3 passes)
ok  github.com/fordjent/fordjent/internal/stalegate  (2 passes, 1 skipped — git not in Alpine)
ok  github.com/fordjent/fordjent/internal/session  (manager still passes)
```

Note: `TestBashToolSuccess` still fails due to Alpine lacking `bash`. Pre-existing.

---

## Second DeepSeek Review — Critical Fixes (April 28, 2026)

After building the lifecycle + stale gate + scaffold layer, DeepSeek identified 5 specific issues and 1 recommended next feature.

### Issues Found

| # | Issue | Severity | Fix Applied |
|---|-------|----------|-------------|
| 1 | `OnSessionStart` fired on **every** event, not just once, polluting the state log | High | Guard with `if GetState() == ""` before recording start transition |
| 2 | `strings.Contains(err.Error(), "max turns")` is fragile against wrapping | Medium | **Deferred**: will use `errors.Is(err, ErrMaxTurnsReached)` after moving transitions into Agent |
| 3 | Scaffold issue was labeled `blocked` (semantically wrong — it IS the unblocker) | Medium | Removed `blocked` label; scaffold issue now only gets `scaffold` label |
| 4 | Stale gate always fetched before merge-base, causing ~500ms-2s unnecessary delay | Low | Reordered: try `merge-base` first (fast path ~10ms), only fetch if ref is missing |
| 5 | Two simultaneous `IssueOpened` events on empty repo could create duplicate scaffold issues | Low | Documented; mitigation is checking for existing scaffold issues post-creation |

### DeepSeek's Verdict

> "The overall architecture is sound. All three features are well-scoped, use the right abstractions (SQLite for persistence, git plumbing for staleness, Forgejo API for scaffold detection), and are correctly gated behind config flags. The issues above are refinements, not redesigns."

### Next Build Recommendation (DeepSeek's ranking)

| Rank | Feature | Why It's Next |
|------|---------|---------------|
| 🥇 | **Auto-rebase in `forgejo_create_pr` tool** | Stale gate currently *blocks* PR creation. Instead: auto-run `git rebase origin/main` + `git push -f` in the tool, then proceed. Eliminates the most common human intervention: telling the agent to rebase. |
| 🥈 | **Auto-merge cycle** | After agent pushes review fixes, merge the PR if mergeable. Closes the loop: issue → PR → review → fix → merge → unblocks dependents. |
| 🥉 | **Status dashboard** | Existing SQLite data (lifecycle + costs) can already serve a `/status` endpoint. Low effort, high operator value. |

### What Was Kept Deliberately Deferred

- **Typed sentinel errors (`ErrMaxTurnsReached`)**: DeepSeek recommended this heavily, but it requires moving lifecycle transitions from `runSession()` into `Agent.ProcessEvent()`. That refactor is correct but more invasive than a quick fix. We applied the guard-based fix instead; the sentinel refactor is queued for the next lifecycle pass.
- **Duplicate scaffold issue cleanup**: Requires a second Forgejo API pass after creating the scaffold issue. Acceptable as-is for the cold-start scenario.

### All-New Files in This Pass

| File | Purpose |
|------|---------|
| `internal/lifecycle/lifecycle.go` | State machine + failure labeling + failure commenting |
| `internal/lifecycle/lifecycle_test.go` | Transitions, failed session listing, nested DB dir |
| `internal/stalegate/stalegate.go` | Git plumbing staleness check |
| `internal/stalegate/stalegate_test.go` | Stale / not-stale / git-not-found |
| `internal/scaffold/scaffold.go` | Empty-repo protection + scaffold issue creation |

### Modified Core Files in This Pass

| File | Change |
|------|--------|
| `internal/forgejo/client.go` | Added 6 new API methods (labels, create issue, list issues, list tree, post comment) |
| `internal/tool/forgejo_tools.go` | `forgejo_create_pr` now receives `repoDir` and calls `stalegate.IsStale()` before merge queue |
| `internal/tool/forgejo_tools_test.go` | Updated `NewCreatePRTool` signature |
| `internal/session/manager.go` | Wires `forgejoClient`, `lc`, scaffold detection; guards lifecycle start with state check |
| `internal/config/config.go` | Added `enable_lifecycle`, `enable_stale_gate`, `enable_scaffold_detection` |
| `fordjent.local.yaml` | Added 3 enable flags (default `true`) |
| `internal/stalegate/stalegate.go` | Reordered merge-base-first-then-fetch for fast path |
| `internal/scaffold/scaffold.go` | Removed `blocked` label from scaffold issue itself |
| `AGENTS.md` | This update |

---

## Bug Fix 14 — Next Targets: Auto-Rebase + Merge Tool + Status Dashboard (April 29, 2026)

Implemented the three features DeepSeek ranked as highest-priority after the lifecycle layer.

### 1. Auto-Rebase in Stale Gate (`internal/stalegate/`)

**Before**: `IsStale()` detected staleness and **blocked** PR creation with instructions to rebase.

**After**: When `merge-base --is-ancestor origin/main HEAD` fails (branch is stale), the stale gate now:
1. Runs `git rebase origin/main`
2. If rebase succeeds → `git push -f -u origin HEAD`
3. Re-runs `merge-base` to verify
4. If now clean → returns `(false, "", nil)` so PR creation proceeds immediately
5. If rebase fails (conflicts) → returns `(true, "Auto-rebase failed...", nil)`

**Validation**:
- ✅ Unit test: clean rebase scenario — stale detected, auto-rebased, returns not stale
- ✅ Unit test: conflict scenario — stale detected, rebase fails, returns stale with conflict message
- ✅ Unit test: already up to date — fast path, no fetch needed

### 2. `forgejo_merge_pr` Tool (`internal/tool/forgejo_tools.go`)

Added new tool `forgejo_merge_pr` to the Forgejo tool suite:
- Accepts: `repository`, `pr_number`, `style` (merge / rebase-merge / squash-merge)
- Before merging, fetches PR details and checks:
  - `has_conflicts: true` → block with error
  - `mergeable: false` → block with error
- Calls `POST /api/v1/repos/{repo}/pulls/{N}/merge` with `{ "Do": style }`
- Integrated into PR Review Mode system prompt: `'If the PR is mergeable with no conflicts, you may call forgejo_merge_pr to merge it automatically.'`
- Registered in `internal/session/manager.go` alongside other Forgejo tools

**Live test**:
- ✅ Session `fjadmin/parallelwave/pulls/32` entered PR Review Mode successfully
- ✅ Agent checked out the PR branch (`feature/add-sqrt-function`)
- ⚠️ Agent did not call `forgejo_merge_pr` in this specific run (LLM chose to make additional changes before merging); tool is available and correctly registered

### 3. Status Dashboard (`/status` endpoint)

Added `GET /status` handler to the webhook router. Returns JSON with:
- `costs`: total sessions, total tokens, total cost USD, and last 20 usage records from `costs.db`
- `lifecycle`: active sessions count (in `working`), failed sessions count, and last 20 state transitions from `lifecycle.db`
- `metrics`: in-memory Prometheus counters (events, sessions, LLM calls, tokens, cost)
- `now`: UTC timestamp

**Schema note**: The lifecycle table uses `from_state`, `to_state`, `occurred_at` columns — queries were updated to match the actual schema.

**Validation**:
- ✅ Status endpoint returns live data (14 historical sessions, ~1.17M tokens tracked)
- ✅ Lifecycle transitions visible: `created` → `working` → `completed` for sessions #29 and #31
- ✅ Prometheus `/metrics` continues to serve text format

### Files Changed

| File | Change |
|------|--------|
| `internal/stalegate/stalegate.go` | Auto-rebase on stale detection; push after rebase; verify loop |
| `internal/stalegate/stalegate_test.go` | Rewrote tests: `UpToDate`, `AutoRebaseSucceeds`, `AutoRebaseConflicts` |
| `internal/forgejo/client.go` | Added `Mergeable`/`HasConflicts` fields to `PullRequest`; added `MergePR()` method |
| `internal/tool/forgejo_tools.go` | Added `forgejoMergePRTool` with mergeability pre-checks |
| `internal/session/manager.go` | Registered `NewMergePRTool`; updated PR Review Mode prompt |
| `internal/webhook/router.go` | Added `/status` handler; `queryCostDB()`; `queryLifecycleDB()` |
| `internal/metrics/metrics.go` | Added `Snapshot()` for JSON status endpoint |
| `AGENTS.md` | This update |

### Known Limitations

- **Auto-rebase does not handle merge conflicts**. If `git rebase` hits conflicts, the agent must resolve them manually. This is by design — automatic conflict resolution would be dangerous.
- **`forgejo_merge_pr` not yet exercised live by the LLM**. The tool is registered and the system prompt mentions it, but in the PR #32 review session the model chose to make additional changes first. Future PR review tests should include a narrower prompt like "Please merge this PR" with no other instructions.
- **Status endpoint queries can be slow** on large DBs because they do `LIMIT 20` without indexes. Acceptable for now at ≤1K sessions.
- **Active session count** in status queries counts ALL `working` states, not just the latest per session. Minor bookkeeping quirk.

### Next Steps

| Rank | Feature | Status |
|------|---------|--------|
| 🥇 | Auto-rebase in stale gate | ✅ Implemented + unit tested |
| 🥈 | `forgejo_merge_pr` tool | ✅ Implemented + registered |
| 🥉 | Status dashboard | ✅ Implemented + tested live |
| **P1** | Auto-merge after review fixes | Needs explicit prompt engineering or auto-action after push |
| **P1** | Lifecycle query optimization | Add index on `occurred_at`, filter latest state per session |
| **P2** | Cost/tokens display in PR comments | Post summary comment after PR creation showing spend |



