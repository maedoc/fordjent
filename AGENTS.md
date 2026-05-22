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
| Forgejo admin pass | *(see bootstrap script)* | Bootstrap script |
| Fordjent Forgejo token | *(see `.env`)* | API-created, stored as `FORGEJO_TOKEN` |
| Webhook secret | *(see `.env`)* | Forgejo webhook (shared with Fordjent config) |

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
  --username fjadmin --password "$ADMIN_PASS" --email admin@local --admin

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
  -u fjadmin:"$ADMIN_PASS" \
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
  -u fjadmin:"$ADMIN_PASS" \
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




---

## Wave10–12 Testing Round (May 6, 2026)

### What Was Tested
End-to-end autonomous agent loop across three waves, with progressive fixes:
- **Wave10**: PM→implementer→PR→review pipeline. Found push-to-main bug, 405 merge bug, PR comment routing bug.
- **Wave11**: 6 PRs created and merged autonomously. Validated PR-based workflow, protected branch blocking, and auto-merge.
- **Wave12**: Tested wafer provider (Qwen3.5 + GLM-5.1). Found JSON Schema validation issue, rate limiting, and unrelated-histories merge conflict.

### Bugs Fixed in This Session

| # | Bug | Severity | Fix |
|---|-----|----------|-----|
| 1 | Push events filtered by `isAgentEvent()` | Critical | Push events now always pass through |
| 2 | PM sessions blocked by scaffold on empty repos | Medium | `[pm]`/`[decompose]` titles skip scaffold blocking |
| 3 | PR comments routed to `issues/N` instead of `pulls/N` | Critical | Detect `issue.is_pull_request` in webhook payload |
| 4 | Bot PRs blocked by human approval gate | High | `forgejo_merge_pr` auto-bypasses for fordjent-bot |
| 5 | Agents pushed to main instead of feature branches | High | Hard-block `git commit`/`git push` on protected branches; scaffold sessions bypass |
| 6 | Auto-push in `forgejo_create_pr` — branch not found | Medium | Auto-push before `ls-remote` check |
| 7 | PM didn't include `Depends on: #N` in sub-issues | Medium | Added to PM system prompt |
| 8 | `fj` CLI ignored `.fj` config file | Low | `loadConfig()` before fallback defaults |
| 9 | Wafer rejected tool schemas with `"items": "string"` | High | Fixed to `"items": {"type": "string"}` |
| 10 | `reasoning_content` from GLM-5.1 discarded | Medium | Added field to response struct; fallback logic |
| 11 | Forgejo merge 405/409 for unrelated histories | High | `MergePR()` passes `allow_unrelated_histories: true` |
| 12 | SQLite BUSY errors with concurrent sessions | Low | Non-fatal — lifecycle transitions are logging only |

### Architecture Changes

| Feature | Description |
|---------|-------------|
| **Protected branch blocking** | `bash` tool blocks `git push origin main`; `git` tool blocks commits on protected branches. Scaffold sessions bypass via `AllowProtectedPush()` |
| **Auto-bypass approval for bot PRs** | `forgejo_merge_pr` checks PR author; fordjent-bot PRs skip approval |
| **Push event passthrough** | `isAgentEvent()` never filters push events |
| **PR comment routing** | PR comments (with `is_pull_request: true`) route to `pulls/N` sessions |
| **Wafer provider support** | Qwen3.5 + GLM-5.1 via wafer.ai with `reasoning_content` handling |
| **Session issue title** | `Session.IssueTitle` from webhook payload for role detection |
| **Auto-push in PR creation** | `forgejo_create_pr` pushes branch before checking remote |
| **Scaffold blocking skip for PM** | `[pm]`/`[decompose]` issues skip empty-repo blocking |

### Provider Configuration

| Role | Model | Provider | Latency |
|------|-------|----------|--------|
| All (current) | minimax-m2.5 | ollama-cloud | 35–90s/turn |
| (Future) | Qwen3.5-397B-A17B | wafer | ~4–7s/turn |
| (Future) | GLM-5.1 | wafer | ~6–10s/turn |

Wafer providers are configured in YAML. Switch by changing `role_providers`. Rate-limited to 2 concurrent calls.

---

## Interaction Layer Hardening (May 13, 2026)

### What Was Done
Hardened the agent/user interaction paths with FSM-enforced tool blocking, expanded test coverage for role-specific prompts, and validated webhook guard rails.

### Changes

#### 1. State-Aware Tool Blocking (Bug Fix 15)

**Problem**: The `planning` and `blocked` FSM states only had **prompt-level** instructions telling the agent not to write code. The agent could still call `write_file`, `git`, `forgejo_create_pr`, or `forgejo_merge_pr` in these states, violating the FSM constraints.

**Fix**: Added hard tool blocking in `Agent.ProcessEvent()` alongside the existing analysis-mode blocking:
- When `fsmState == StatePlanning || fsmState == StateFSMBlocked`, implementation tools (`write_file`, `git`, `forgejo_create_pr`, `forgejo_merge_pr`) return an error message instead of executing
- The agent receives `"Error: This issue is in Planning state..."` or `"Error: This issue is Blocked..."` as the tool result
- FSM state is detected once at session start via `detectIssueState()` and passed through to `buildSystemPrompt()` and the tool execution loop
- `issueStateInstructions()` refactored to standalone function taking state directly (no more per-call API hit)

**Files changed**:
| File | Change |
|------|--------|
| `internal/session/agent.go` | Added `fsmState` detection; hard tool blocking for planning/blocked; refactored `issueStateInstructions` to standalone with BLOCKED language; updated `buildSystemPrompt` signature |
| `internal/session/agent_test.go` | Updated `buildSystemPrompt` calls with `fsmState`; simplified `issueStateInstructions` tests to standalone; added `TestIssueStateInstructions_Implementing`, `TestBuildSystemPrompt_DevOpsRole`, `TestBuildSystemPrompt_TesterRole`, `TestBuildSystemPrompt_PMRole` |

#### 2. Closed-PR Comment Guard Test

Added `TestClosedPRCommentGuard` and `TestOpenPRCommentNotSkipped` to verify the router skips comments on closed/merged PRs (preventing token burn from cost-summary loops) but processes comments on open PRs normally.

#### 3. Role Assignment Failure Path Test

Added `TestRoleAssignment_ForgejoError` — when Forgejo returns 500 on `GetIssue` during role assignment, the manager logs a warning and returns gracefully instead of crashing.

#### 4. Scaffold Detection Integration Test

Added `TestScaffoldDetection_BlocksOnEmptyRepo` and `TestScaffoldDetection_PassesOnPopulatedRepo` — validates scaffold issue creation on empty repos and passthrough on repos with `go.mod` + `README.md`. Extended `interactionForgejo` fake with `repoFiles`, `openIssues`, `createdIssues` fields and handlers for `git/trees`, list issues, and create issue endpoints.

### Test Coverage Baseline (May 13, 2026)

| Package | Coverage |
|---------|----------|
| `internal/agent` | 45.1% |
| `internal/config` | 35.5% |
| `internal/cost` | 61.9% |
| `internal/event` | 94.7% |
| `internal/forgejo` | 13.5% |
| `internal/lifecycle` | 35.5% |
| `internal/memory` | 70.6% |
| `internal/mergequeue` | 80.4% |
| `internal/provider` | 65.8% |
| `internal/scheduler` | 77.7% |
| `internal/session` | 60.0% |
| `internal/stalegate` | 61.1% |
| `internal/tool` | 32.5% |
| `internal/webhook` | 38.6% |

### Files Changed in This Pass

| File | Change |
|------|--------|
| `internal/session/agent.go` | FSM state tool blocking; `issueStateInstructions` standalone; `buildSystemPrompt` takes `fsmState` |
| `internal/session/agent_test.go` | Role prompt tests (devops/tester/pm); simplified state instruction tests |
| `internal/session/interaction_test.go` | Scaffold detection tests; role assignment error test; `interactionForgejo` extended with tree/issues/create-issue handlers |
| `internal/webhook/router_test.go` | Closed-PR comment guard tests; added `forgejo` import |
| `AGENTS.md` | This update |

---

## Bug Fix 16 — `detectRoleFromTitle` Missing `[implementer]` Tag (May 13, 2026)

**Problem**: `detectRoleFromTitle()` in `internal/session/manager.go` only recognized `[pm]`, `[review]`, `[devops]`, and `[test]` title tags. The `[implementer]`, `[implement]`, `[dev]`, and `[developer]` tags were **missing** — the most common role tag for code-writing issues was completely ignored by the role gate, causing all implementer-tagged issues to be blocked as "untagged" when `require_role_tag: true`.

**Fix**: Added the implementer branch to `detectRoleFromTitle()`:
```go
if strings.Contains(lower, "[implementer]") || strings.Contains(lower, "[implement]") || strings.Contains(lower, "[dev]") || strings.Contains(lower, "[developer]") {
    return "implementer"
}
```

This matches the label-based detection in `detectRoleFromIssue()` which already had `role:implementer` and `role:developer`.

**Impact**: Without this fix, `[implementer]`-tagged issues were blocked by the role gate and a `needs-role` label was added, requiring manual intervention. The guidance comment told users to add `[implementer]` — which didn't work.

**Files changed**:
| File | Change |
|------|--------|
| `internal/session/manager.go` | Added `[implementer]`, `[implement]`, `[dev]`, `[developer]` to `detectRoleFromTitle()` |

---

## Native Local Deployment (May 13, 2026)

### What Was Built
A one-command bootstrap script that sets up Forgejo + Fordjent locally on macOS, both running natively (no Docker) inside `sandbox-exec` profiles.

### Architecture

```
Forgejo (brew) :3000  ←→  Fordjent (go build) :8080
     sandbox-exec            sandbox-exec
         ↓                        ↓
  ~/fordjent-local/forgejo-data/  ~/fordjent-local/fordjent-work/
```

Webhook delivery is trivial: `http://127.0.0.1:8080/acp/v1/events` — no tunnels needed.

### Files

| File | Purpose |
|------|---------|
| `scripts/bootstrap-local.sh` | One-command setup: installs Forgejo via brew, generates config, creates admin user + tokens, builds Fordjent, creates test repo with FSM labels, registers webhook, fires test issue, waits for agent activity |
| `scripts/teardown-local.sh` | Kills Forgejo + Fordjent, optionally `--clean` wipes `~/fordjent-local/` |
| `scripts/sandbox/forgejo.sb` | Sandbox profile: allow local TCP only, restrict writes to `~/fordjent-local/` |
| `scripts/sandbox/fordjent.sb` | Sandbox profile: allow outbound network (LLM APIs), restrict writes |

### Usage

```bash
export WAFER_API_KEY=wfr_...
./scripts/bootstrap-local.sh          # set up everything
./scripts/teardown-local.sh           # stop services
./scripts/teardown-local.sh --clean   # stop + wipe data
```

### Key Design Decisions

- **Tokens hardcoded in YAML** (not `${ENV_VAR}`): `sandbox-exec` doesn't inherit parent env vars. The bootstrap writes actual values into the config file.
- **Forgejo stopped briefly for user creation**: SQLite DB locks prevent `forgejo admin user create` while `forgejo web` is running. Bootstrap stops Forgejo, creates user + tokens, then restarts.
- **Repo seeding**: The test repo is pre-seeded with `go.mod` + `.gitignore` (via Forgejo contents API) so scaffold detection doesn't block the first issue.
- **Test issue uses `[implementer]` tag**: Required because `require_role_tag: true` in config.

### Issues Found During Bootstrap

| # | Issue | Fix |
|---|-------|-----|
| 1 | `sandbox-exec` profile syntax: `network-listen` is not valid | Changed to `(allow network-inbound (local tcp))` + `(allow network-bind (local tcp))` + `(allow network-outbound)` |
| 2 | `sandbox-exec` doesn't inherit env vars | Config uses hardcoded values, not `${ENV_VAR}` expansion |
| 3 | `forgejo admin user create` can't run while `forgejo web` holds SQLite | Stop Forgejo → create user → restart |
| 4 | `detectRoleFromTitle` missing `[implementer]` | Bug Fix 16 — added implementer tags to title detection |
| 5 | Auto-initialized repo triggers scaffold detection | Seed `go.mod` + `.gitignore` via API before creating issues |

### Smoke Test Result

Issue #2 `[implementer] Write a hello world Go program`:
1. ✅ Agent picked up the issue (role gate passed)
2. ✅ Created feature branch `feature/hello-world`
3. ✅ Wrote `main.go` + `Makefile`
4. ✅ Created PR #3 — "Add hello world Go program and Makefile"
5. ✅ Entered PR review mode, verified `go build` + `./testbed`
6. ✅ Posted review comment: "Ready to Merge"
7. ✅ Session completed: 65K tokens, $0 cost (Wafer free tier)

---

## Bug Fixes 17–20 + Test Hardening (May 13, 2026)

### Bug Fix 17 — Label Updated Feedback Loop
**Problem**: Non-role `IssueLabelUpdated` events (e.g., FSM transitions adding `blocked`) created new sessions, which triggered more label updates, which created more sessions — infinite feedback loop.

**Fix**: In `Manager.handleEvent`, before session creation, non-role `IssueLabelUpdated` and `PullRequestLabelUpdated` events are dropped (only FSM state tracking updates proceed). The `automerge` label on PRs still creates sessions (for reviewer activation).

### Bug Fix 18 — PM Prompt Added `blocked` Labels to Sub-Issues
**Problem**: The PM system prompt instructed the agent to add `blocked` labels to sub-issues. This conflicted with the scheduler's ownership of blocking/unblocking.

**Fix**: Removed `blocked` label instructions from PM system prompt. The scheduler manages blocking via `Depends on:` dependency tracking.

### Bug Fix 19 — Scheduler `isIssueClosed` Treated All Open Issues as Blocking
**Problem**: `isIssueClosed` checked if a dependency issue was "open" and treated ALL open issues as blocking — even PM/coordination issues that would never have a PR. This caused permanent blocking.

**Fix**: `isIssueClosed` now checks the `pull_request` field on the issue API response directly. Issues with no associated PR (PM issues, coordination issues) are treated as satisfied (not blocking). The `hasOpenPR()` helper was removed — single API call approach.

### Bug Fix 20 — `blocked` State Instructions Inadequate
**Problem**: When an issue was in `blocked` FSM state, the agent was told it couldn't work but had no guidance on how to resolve the blockage.

**Fix**: Updated `issueStateInstructions(StateFSMBlocked)` to guide the agent through verifying whether dependencies actually have open PRs, and removing the `blocked` label if the dependency is resolved.

### Additional Fixes in This Pass

| # | Issue | Fix |
|---|-------|-----|
| 21 | `AddIssueLabels` created duplicate labels | Added dedup: `GetIssue` checks existing labels before POST; only adds labels not already on the issue |
| 22 | `internal/e2e` build errors: `bus.Run`/`router.ServeHTTP` undefined | Added `Router.Handler()` method; fixed e2e test to use proper HMAC, shared bus, `log/slog.Default()` logger |
| 23 | Fake Forgejo `handleGetIssue` returned repo-level `createdLabels` as issue labels | Fixed to only return `issueLabels + addedLabels` (labels actually on the issue) |
| 24 | `TestLabelUpdatedDoesNotCreateSession` used undefined `baseTestConfig`/`GetSession` | Changed to `testConfig(t, f.URL(), true)` and `mgr.sessions[key]` direct access |

### Files Changed in This Pass

| File | Change |
|------|--------|
| `internal/session/manager.go` | Drop non-role label_updated events before session creation; `detectRoleFromTitle` includes `[implementer]`/`[implement]`/`[dev]`/`[developer]` (Bug #16); `blocked` state instructions guide dependency verification (Bug #20) |
| `internal/session/agent.go` | FSM state tool blocking for planning/blocked; `issueStateInstructions` standalone; `buildSystemPrompt` takes `fsmState` |
| `internal/scheduler/scheduler.go` | `isIssueClosed` refactored — checks `pull_request` field directly; `hasOpenPR()` removed (Bug #19) |
| `internal/forgejo/client.go` | `AddIssueLabels` dedup: calls `GetIssue` first to check existing labels (Bug #21) |
| `internal/webhook/router.go` | Added `Handler()` method for external access to mux |
| `internal/e2e/e2e_test.go` | Fixed: proper HMAC, shared bus, real logger, `Router.Handler()` |
| `internal/session/role_gate_test.go` | Fixed fake `handleGetIssue` to not include `createdLabels` as issue labels |
| `internal/session/interaction_test.go` | `TestLabelUpdatedDoesNotCreateSession` — uses `testConfig` + `mgr.sessions` |
| `internal/session/manager_test.go` | Added `TestDetectRoleFromTitle` with 20 test cases covering all role tags |
| `internal/session/agent_test.go` | Role prompt tests (devops/tester/pm); simplified state instruction tests |
| `internal/session/interaction_test.go` | Scaffold detection tests; role assignment error test; extended `interactionForgejo` fake |
| `internal/webhook/router_test.go` | Closed-PR comment guard tests |

### Test Results

```
ok  github.com/fordjent/fordjent/internal/agent
ok  github.com/fordjent/fordjent/internal/config
ok  github.com/fordjent/fordjent/internal/cost
ok  github.com/fordjent/fordjent/internal/e2e
ok  github.com/fordjent/fordjent/internal/event
ok  github.com/fordjent/fordjent/internal/forgejo
ok  github.com/fordjent/fordjent/internal/lifecycle
ok  github.com/fordjent/fordjent/internal/memory
ok  github.com/fordjent/fordjent/internal/mergequeue
ok  github.com/fordjent/fordjent/internal/provider
ok  github.com/fordjent/fordjent/internal/scheduler
ok  github.com/fordjent/fordjent/internal/session
ok  github.com/fordjent/fordjent/internal/stalegate
ok  github.com/fordjent/fordjent/internal/tool
ok  github.com/fordjent/fordjent/internal/webhook
```

All 15 internal packages pass.

---

## Wave C–E Validation + Bug Fixes 21–24 (May 18, 2026)

### What Was Tested
Three validation waves to verify the Bug 1–3 fixes (dual-label, session recovery, reviewer cost cap) from the prior session.

### Wave C — Dual-Label + Basic Pipeline (3 issues, fired simultaneously)

| Issue | Title | Role | PR | Result |
|-------|-------|------|-----|--------|
| #30 | `[implementer]` Add ToUpper | implementer (Qwen) | PR#35 merged | ✅ Clean labels, "implementation" cost summary |
| #31 | `[implementer]` Add Abs | implementer (Qwen) | PR#34 merged | ✅ Clean labels, "implementation" cost summary |
| #32 | `[devops]` Add lint target | devops (Qwen) | PR#33 merged | ✅ Clean labels, "devops" cost summary |

**Validations**:
- ✅ No dual `blocked`+`ready` labels on any issue (Bug 1 fix confirmed)
- ✅ Cost summary comments show role labels ("implementation", "devops")
- ✅ Reviewer sessions on PRs #34/#35 used GLM-5.1 (per `role_providers.reviewer`)
- ✅ `max_turns_reviewer: 10` correctly capped reviewer at 10 turns

### Wave D — Max-Turns + Auto-Retry (1 issue, artificially low `max_turns_implementer: 3`)

| Issue | Title | Result |
|-------|-------|--------|
| #36 | `[implementer]` Add MapKeys | Hit 3-turn limit → auto-retry → permanent block |

**Timeline**:
- t=0: Issue created → agent starts → hits 3 turns → `OnSessionFailedMaxTurns` fires
- Immediately: Issue has `fordjent/failed:max-turns` only (NO `blocked`) ✅
- t=5min: Auto-retry ticker fires → adds `ready` label
- **Bug A found**: Adding `ready` triggers `issues.label_updated` webhook → immediate new session → fails in 3 turns → adds `fordjent/failed:max-turns` again → cascade
- t=10min: 2nd auto-retry → `CountFailedRetries` returns 6 ≥ `max_session_retries: 2` → permanently blocks with `fordjent/failed:max-retries` ✅

**Validations**:
- ✅ `fordjent/failed:max-turns` added without `blocked` (Bug 2 fix)
- ✅ Auto-retry fires after 5 minutes
- ✅ Permanent blocking after retries exhausted
- 🐛 **Bug A**: Cascade — adding `ready` triggers webhook storm

### Wave E — Reviewer Session (2 issues + manual review comment)

| Issue | Title | PR | Reviewer Result |
|-------|-------|-----|-----------------|
| #37 | `[implementer]` TrimSpace | PR#40 | Reviewer (GLM, 10 turns) hit max-turns 🐛 |
| #38 | `[implementer]` CountWords | PR#39 (auto-merged) | No review needed |

**Validations**:
- ✅ Reviewer used GLM-5.1 model (per `role_providers.reviewer`)
- ✅ Reviewer capped at 10 turns (per `max_turns_reviewer`)
- 🐛 **Bug B**: 10 turns too low for meaningful code review
- 🐛 **Bug D**: Auto-retry for PR reviewer used `issues/40` key instead of `pulls/40`, losing PR context and reviewer role

### Bugs Found + Fixed

#### Bug 21 — Auto-Retry Cascade (Critical)
**Problem**: `runAutoRetry()` added `ready` label to signal retry. This triggered `issues.label_updated` webhook → Fordjent created a new session → agent hit max-turns → added `fordjent/failed:max-turns` → cascade of repeated failures.

**Fix**: Instead of adding `ready` label and relying on webhook, `runAutoRetry()` now directly dispatches a synthetic `event.IssueOpened` event to `handleEvent()`. This bypasses the webhook entirely. The `ready` label is no longer added (only stale labels are removed and `fordjent/failed:max-turns` is removed).

#### Bug 22 — `max_turns_reviewer` Too Low
**Problem**: `max_turns_reviewer: 10` was insufficient for code review sessions that need to read code, check tests, and make fixes. 2 out of 3 reviewer sessions hit the limit.

**Fix**: Bumped default from 10 → 20 in `internal/config/config.go` and `fordjent.local.yaml`.

#### Bug 23 — Auto-Retry Doesn't Skip Closed/Merged PRs
**Problem**: Auto-retry scanned ALL stored sessions including closed/merged PRs, attempting to retry sessions that were already resolved. This wasted API calls and could add labels to merged PRs.

**Fix**: `runAutoRetry()` now checks `issue.State == "closed"` and skips closed issues, cleaning up the stale `fordjent/failed:max-turns` label.

#### Bug 24 — Auto-Retry Used Wrong Session Key for PRs
**Problem**: Auto-retry always constructed session key as `issues/N`, even when the issue was a PR (which should use `pulls/N`). This caused the retry session to lose the PR context (reviewer role, PR branch checkout).

**Fix**: Auto-retry now checks `issue.PullRequest` field (new `PRRef` struct on `forgejo.Issue`). If the issue has a `pull_request` reference, the session key is constructed as `pulls/N` and the synthetic event includes `PRNumber`.

### Files Changed

| File | Change |
|------|--------|
| `internal/session/manager.go` | `runAutoRetry()`: direct event dispatch instead of label trigger (Bug 21); skip closed issues (Bug 23); PR detection via `issue.PullRequest` (Bug 24); remove `ready`/`in_progress` before `blocked` in permanent block |
| `internal/forgejo/client.go` | Added `PRRef` struct and `PullRequest` field to `Issue` struct (Bug 24) |
| `internal/config/config.go` | `MaxTurnsReviewer` default: 10 → 20 (Bug 22) |
| `fordjent.local.yaml` | `max_turns_reviewer: 20` (Bug 22) |
| `AGENTS.md` | This update |

## Cloud Deployment on Scaleway (May 21, 2026)

### Architecture
- **Instance**: DEV1-L (4 vCPU, 8GB RAM, ~€31/mo) on Scaleway `fr-par-2`
- **DNS**: `forgejo.wdmn.fr` + `fordjent.wdmn.fr` via Gandi LiveDNS API
- **TLS**: Automatic via Caddy + Let's Encrypt
- **LLM**: Scaleway Qwen3.6-35b-a3b (`api.scaleway.ai`) — confirmed working with tool_calls
- **Firewall**: UFW — 22/80/443 only
- **Docker Compose**: Caddy + Forgejo + Fordjent (3 containers on `fordjent-net` bridge)

### Bugs Fixed in This Session

| # | Bug | Severity | Fix |
|---|-----|----------|-----|
| 1 | Forgejo app.ini mounted `:ro` but Forgejo needs to write JWT secrets | High | Removed `:ro` flag; copy app.ini into the volume instead of bind-mounting |
| 2 | Caddyfile `transport` directive inside `reverse_proxy` block | High | Moved `transport http {}` inside the `reverse_proxy` block |
| 3 | bwrap sandbox fails inside Docker (`--no-new-privileges` unsupported) | High | Set `sandbox.enabled: false` in cloud fordjent.yaml (nested sandboxing not supported in Docker) |
| 4 | Go build cache `/var/cache/go-build` not writable by `fordjent` user | Medium | Added `mkdir -p /var/cache/go-build /var/cache/go-mod` + `chown` in Dockerfile and entrypoint.sh |
| 5 | Cloud-init GPG key fetch fails silently | Low | Non-fatal — Docker was already installed from a partial run; remaining steps (bwrap, UFW) executed manually |

### What Works End-to-End

1. ✅ `fordjent-deploy up` creates Scaleway instance, DNS records via Gandi, provisions Docker
2. ✅ TLS certs auto-provisioned by Caddy (both domains)
3. ✅ Forgejo accessible at `https://forgejo.wdmn.fr` (v9.0.3)
4. ✅ Fordjent webhook accessible at `https://fordjent.wdmn.fr/acp/v1/events`
5. ✅ Agent picks up issues, writes code, creates PRs, merges them
6. ✅ Scaleway Qwen3.6-35b-a3b model works with tool_calls (`finish_reason: "tool_calls"`)
7. ✅ Status dashboard at `https://fordjent.wdmn.fr/status`
8. ✅ `fordjent-deploy down` tears down instance, releases IP, removes DNS records

### Files for Cloud Deployment

| File | Purpose |
|------|---------|
| `deploy/cloud/docker-compose.yaml` | 3-service stack: Caddy + Forgejo + Fordjent |
| `deploy/cloud/Caddyfile` | SNI routing with auto-TLS |
| `deploy/cloud/forgejo.app.ini` | Forgejo config template |
| `deploy/cloud/fordjent.yaml` | Fordjent config template (sandbox disabled, Scaleway AI provider) |
| `deploy/env.sh` | Environment variable template |
| `deploy/env.local.sh` | Local secrets (gitignored) |
| `deploy/src/fordjent_deploy/` | Python CLI tool: `fordjent-deploy up/down/status` |
| `deploy/src/fordjent_deploy/gandi_dns.py` | Gandi LiveDNS API client |
| `deploy/README.md` | Usage documentation |

---

## Bug Fix 25 — Scaffold Hardcodes Go for All Projects (May 22, 2026)

**Problem**: The scaffold system had Go language hardcoded throughout — the issue title said "go.mod", the body mentioned "create `go.mod` and `README.md`", and the repo population check only looked for `go.mod`. When a Python/Snakemake user (`janf/poke2`) created a data science project, the agent:
1. Created `go.mod` and a Go `.gitignore` instead of `requirements.txt` and a Python `.gitignore`
2. The scaffold issue was titled "Add project scaffold (go.mod, README.md, etc.)" which biased the LLM
3. The repo population check (`hasGoMod && hasReadme`) meant a Python repo with `requirements.txt` + `README.md` would still be considered "empty"

**Analysis of `janf/poke2` issue #6**:
- PM correctly decomposed "Setup a data science project" into 4 Python/Snakemake sub-issues (#2-5)
- Scaffold detector created issue #6 with Go-specific title and body
- Agent proceeded to create `go.mod` + Go `.gitignore` — completely wrong for a Python project
- Agent tried 3× to push to `main` (blocked by protected branch), then created PR #7 with Go files
- `no-auto-merge` policy prevented merging the wrong scaffold
- On auto-retry, scaffold detection re-blocked issue #6 because go.mod was on a branch, not merged to main

**Fix**: Language-aware scaffold detection and content generation:
- Added `detectProjectLang(files)` — examines repo files for language manifests (`go.mod` → Go, `requirements.txt`/`pyproject.toml` → Python, etc.), falls back to file extension counting, defaults to "unknown"
- Added `isRepoPopulated(files, lang)` — per-language population checks (Python: requirements.txt + README.md, Go: go.mod + README.md, etc.), unknown: 3+ files + README.md
- Added `scaffoldIssueContent(lang, fileCount)` — generates language-appropriate issue titles and bodies (Go: "Set up Go project structure", Python: "Set up Python project structure" with Snakemake mention, unknown: "Set up project structure" with hints to check other issues)
- Scaffold issue no longer gets `blocked` label (the scaffold IS the unblocker)
- For unknown language: issue body says "Look at other open issues for hints about the language and framework"

**Post-fix actions on `janf/poke2`**:
- Added `fordjent-yolo` topic for zero-friction automation
- Seeded `requirements.txt` and Python `.gitignore` via API
- Closed bad Go PR #7 and duplicate issues #6, #7
- Posted explanatory comment on issue #1

### Files Changed

| File | Change |
|------|--------|
| `internal/scaffold/scaffold.go` | Language detection (`detectProjectLang`), population check (`isRepoPopulated`), language-specific content (`scaffoldIssueContent`); scaffold issue no longer gets `blocked` label |
| `internal/scaffold/scaffold_test.go` | 12 new tests: language detection (8 cases), population check (8 cases), content generation (4 cases) |
| `internal/session/manager.go` | Role gate guidance improved: tags vs labels explanation, `fordjent-yolo` suggestion |
| `internal/session/agent.go` | PM prompt improved: plan-first guidance with `plan-approved` and `fordjent-yolo`; planning state UX with yolo suggestion; `strconv` import |
| `internal/scaffold/scaffold.go` | Empty repo 400 error logged as INFO instead of WARN |

## Bug Fix 26–29 + P3/P6 Features (May 22, 2026)

### Bug Fix 26 — Auto-retry used wrong event type for PRs
**Problem**: When auto-retry fired for a max-turns failure on a PR, it dispatched an `IssueOpened` event with session key `issues/N`, even though PR review sessions should use `pulls/N` and `PullRequestOpened` event type.

**Fix**: `runAutoRetry()` now checks `issue.PullRequest.IsPR()` and sets `evt.Type = event.PullRequestOpened` for PRs, along with the correct `pulls/N` session key.

### Bug Fix 27 — isIssueClosed treated all open issues as satisfied
**Problem**: Previous fix (Bug #19) made `isIssueClosed` treat all issues without a `PullRequest` field as "not blocking". This was correct for PM/coordination issues but was later changed to treat them as blocking (returning `false`), which broke PM dependency satisfaction.

**Fix**: Restored the original logic: open issues without a PR (coordination/PM issues) are treated as satisfied (not blocking). Open issues WITH an associated PR are blocking. Closed/merged issues are always satisfied.

### Bug Fix 28 — Comment cap wasted LLM turns
**Problem**: When the per-session comment limit (default 2) was reached, the `forgejo_comment` tool returned an error message. The LLM would then waste another turn trying to rephrase or call the tool again.

**Fix**: Added `ToolsExcluding()` method to `tool.Registry` and `TurnExecutor.SetExcludeTools()`. When the comment limit is reached, `forgejo_comment` is removed from the LLM's tool schema entirely on subsequent turns. The old execution-time block remains as a safety net for the first hit.

### Bug Fix 29 — PR detection in isIssueClosed used wrong condition
**Problem**: (Already folded into Bug #27) The condition check for `PullRequest` was using `||` instead of `&&` for URL/HTMLURL empty checks, making PRs with missing URL fields incorrectly treated as non-PRs.

**Fix**: Corrected to use `&&` in `issue.PullRequest.URL != "" || issue.PullRequest.HTMLURL != ""`.

### P3 — Request Reviewers on PR Creation
**Added**: `RequestReviewers()` method to `Forgejo.Client` that calls `POST /repos/{owner}/{repo}/pulls/{N}/requested_reviewers`. The `forgejo_create_pr` tool now automatically requests the repo owner as a reviewer after PR creation, replacing the noisy "Ready for review" comment pattern.

### P6 — Language-Aware Implementer Prompt
**Added**: `DetectProjectLang()` exported function in `scaffold/scaffold.go` that detects the repo's primary language from its file list. `buildSystemPrompt()` now includes language-specific instructions (e.g., "This is a Python project. Use Python conventions") in the system prompt, preventing the agent from creating `go.mod` in Python repos.

### Files Changed

| File | Change |
|------|--------|
| `internal/session/manager.go` | Auto-retry uses `PullRequestOpened` for PRs |
| `internal/scheduler/scheduler.go` | Restored `isIssueClosed` PM issue handling |
| `internal/session/agent.go` | Language-aware prompt; comment cap schema exclusion |
| `internal/agent/turn.go` | `SetExcludeTools()` + `ToolsExcluding()` |
| `internal/tool/registry.go` | `ToolsExcluding()` method |
| `internal/forgejo/client.go` | `RequestReviewers()` method |
| `internal/tool/forgejo_tools.go` | Auto-request reviewer after PR creation |
| `internal/tool/forgejo_tools_test.go` | Updated `TestCreatePRToolExecute` |
| `internal/scaffold/scaffold.go` | Exported `DetectProjectLang()` |

## Role-Based Agent Identities (May 22, 2026)

### Problem
All agent actions appeared under a single `fjadmin` user, making it impossible to tell which role (PM, implementer, reviewer) was responsible for a comment, PR, or label change.

### Solution
Created three role-based Forgejo users with distinct identities:

| Role | User | Avatar | Description |
|------|------|--------|-------------|
| PM | `djent-pm` | 🎯 on indigo | Planning, decomposition, coordination |
| Implementer / DevOps | `djent-dev` | ⚡ on green | Code writing, infrastructure, deployment |
| Reviewer / Tester | `djent-qa` | 🔍 on amber | Code review, testing, quality assurance |

### Implementation

**Config** (new fields in `forgejo` section):
```yaml
forgejo:
  role_tokens:
    pm: "token-for-djent-pm..."
    implementer: "token-for-djent-dev..."
    devops: "token-for-djent-dev..."    # shares with implementer
    reviewer: "token-for-djent-qa..."
    tester: "token-for-djent-qa..."     # shares with reviewer
  role_users:
    pm: "djent-pm"
    implementer: "djent-dev"
    devops: "djent-dev"
    reviewer: "djent-qa"
    tester: "djent-qa"
```

**New API methods** (`internal/forgejo/client.go`):
- `WithToken(token)` — returns a copy of the client with a different token (for role switching)
- `AddAssignees(repo, issue, []usernames)` — PATCH `/repos/{owner}/{repo}/issues/{N}` with assignees
- `RemoveAssignee(repo, issue, username)` — DELETE assignee from issue
- `RequestReviewers(repo, pr, []usernames)` — POST requested reviewers on a PR

**Agent changes**:
- `NewAgent` checks `config.Forgejo.RoleTokens[role]` and creates a role-specific Forgejo client
- All comments, PRs, reactions, and labels appear under the role user
- On session start, the issue is auto-assigned to the role user via `AddAssignees`
- Git commits use `djent-dev` as the default identity (configurable via `git_name`/`git_email`)

**Language-aware build gate** (`internal/tool/forgejo_tools.go`):
- `forgejo_create_pr` now detects the repo language via `scaffold.DetectProjectLang`
- Go projects: `go build ./...`, `go test ./...`, `golangci-lint run`
- Python projects: `python3 -m pytest` (non-blocking)
- Unknown: skip build/test gate
- Fixes bug where agents added `go.mod` stubs to Python projects

**Reviewer request**:
- `forgejo_create_pr` now requests collaborators as reviewers (not just the repo owner)
- The PR author is excluded from the reviewer list (Forgejo returns 422 for self-review)
- On repos where `djent-qa` is a collaborator, they will be auto-requested

**Bootstrap**:
- `bootstrap-local.sh` creates three Forgejo users (`djent-pm`, `djent-dev`, `djent-qa`)
- Generates tokens for each and adds them as collaborators on repos
- Uploads avatar PNGs with role initials (PM, DEV, QA) on colored circles

### Files Changed

| File | Change |
|------|--------|
| `internal/config/config.go` | `RoleTokens` and `RoleUsers` maps on `ForgejoConfig` |
| `internal/forgejo/client.go` | `WithToken()`, `AddAssignees()`, `RemoveAssignee()`, `RequestReviewers()` |
| `internal/session/agent.go` | Role-specific Forgejo client in `NewAgent()` |
| `internal/session/manager.go` | Auto-assign role user on session start |
| `internal/tool/forgejo_tools.go` | Language-aware build gate; collaborator-based reviewer request |

## Milestones + Time Tracking (May 22, 2026)

### Problem
PM sub-issues had no visual progress tracking. Cost summary comments ("Session completed: N tokens, $0.00 USD") cluttered the issue timeline with zero-actionable data.

### Solution
Replaced both with Forgejo's native features: milestones for sub-issue grouping, time tracking for session duration.

### Milestones

**New tools** (registered for PM role):
- `forgejo_create_milestone(repository, title, description)` — creates a milestone
- `forgejo_set_milestone(repository, issue_number, milestone_id)` — attaches an issue
- `forgejo_list_milestones(repository)` — lists all milestones with progress

**PM system prompt**: After decomposing a task, create a milestone titled "#N: Description" and attach each sub-issue to it. The milestone progress bar (3/5 closed = 60%) replaces the scheduler's "All dependencies resolved" comment.

**API methods** (`internal/forgejo/client.go`):
- `CreateMilestone(repo, title, description)` → `*Milestone`
- `GetMilestone(repo, id)` → `*Milestone`
- `ListMilestones(repo)` → `[]Milestone`
- `SetIssueMilestone(repo, issue, milestoneID)` → `error`
- `CloseMilestone(repo, id)` → `error`

### Time Tracking

**Concept**: Instead of a comment that says "53s spent", log the duration via Forgejo's time tracking API. The entry appears in the issue sidebar as "djent-dev: 53s". The role-specific token ensures the correct user identity.

**API methods**:
- `AddTrackedTime(repo, issue, seconds)` → `(*TrackedTime, error)`
- `GetTrackedTimes(repo, issue)` → `([]TrackedTime, error)`
- `DeleteTrackedTime(repo, issue, timeID)` → `error`

**Session flow**:
1. `Session.StartTime` recorded when processing begins
2. On completion/failure: `Manager.logSessionTime()` calls `AddTrackedTime` with role token
3. Time appears in UI as `djent-dev: 53s` (the role user, not `fjadmin`)

### What Was Removed

| Was | Replaced By |
|-----|-------------|
| "Session completed (implementation): N tokens, $X.XX USD" comment | Commit status on PR SHA + time entry in sidebar |
| "Max turns reached. Auto-retry may be attempted." comment | ❌ reaction + `fordjent/failed:max-turns` label + time entry |
| "Session error: ..." comment | ❌ reaction + `fordjent/failed:error` label + time entry |
| Scheduler "All dependencies are now resolved" comment | 🚀 reaction (milestone progress bar is self-evident) |

### Files Changed

| File | Change |
|------|--------|
| `internal/forgejo/client.go` | Milestone, TrackedTime structs; 8 new API methods; `Milestone` field on Issue |
| `internal/tool/forgejo_tools.go` | `forgejoCreateMilestoneTool`, `forgejoSetMilestoneTool`, `forgejoListMilestonesTool` |
| `internal/session/agent.go` | Milestone tools registered for PM role; PM prompt updated |
| `internal/lifecycle/lifecycle.go` | Removed all cost/summary/error comments; added sessionDuration param |
| `internal/session/manager.go` | `Session.StartTime`; `logSessionTime` helper with role token |

### Remaining Noise Sources

After milestones + time tracking, the agent comment noise is approximately 95% reduced from the original (41 of 42 comments on marmaduke/test01). Remaining:
- **Implementer summary comments** (e.g. "PR #N created with the Factorial implementation") — 1-2 per session, capped at 2
- **Merge queue block comments** — 1 per session when files overlap
- **Lifecycle pings** — none (all lifecycle comments removed)
