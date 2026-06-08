# Code Context — 5 Gap Analysis

## Gap 1: Merge Queue Auto-Retry

### (a) Where CheckGate is called: `internal/tool/forgejo_tools.go:659`

```go
// Line 648-686
if t.mq != nil {
    var mqErr error
    for attempt := 0; attempt < 3; attempt++ {
        blocked, msg, err := t.mq.CheckGate(ctx, params.Repository, params.Head, params.Base)
        if err == nil {
            if blocked {
                slog.Warn("create_pr: merge queue blocked", "branch", params.Head, "msg", msg)
                if t.repoDir != "" {
                    cmd := exec.CommandContext(ctx, "git", "-C", t.repoDir, "push", "--delete", "origin", params.Head)
                    // ... cleanup branch
                }
                return "", fmt.Errorf("%w: %s", sentinel.ErrBlocked, msg)
            }
            break
        }
        // ... retry on transient errors
    }
}
```

The `MergeGate` interface (line 500):
```go
type MergeGate interface {
    CheckGate(ctx context.Context, repo, headBranch, baseBranch string) (blocked bool, message string, err error)
}
```

### (b) What happens after block message: `internal/session/manager.go:1114-1126`

When `forgejo_create_pr` returns `err` containing `sentinel.ErrBlocked`:

```go
if err := agt.ProcessEvent(ctx, evt); err != nil {
    if errors.Is(err, sentinel.ErrBlocked) {
        slog.Info("session blocked by merge queue", "session_key", sess.Key)
        // ... git branch name extraction
        m.lc.OnSessionBlocked(ctx, evt.Repository, evt.IssueNumber, sess.Key, branch)
    }
    // ...
}
```

`OnSessionBlocked` (in `internal/lifecycle/lifecycle.go`) labels the issue, logs the time, and ends the session. **The agent never tries again.**

### (c) How scheduler `OnPRMerged` works: `internal/scheduler/scheduler.go:74-88`

```go
func (s *Scheduler) OnPRMerged(ctx context.Context, repo string, mergedPRNumber int) ([]PMReactivateResult, error) {
    err := s.checkAndUnblock(ctx, repo, mergedPRNumber)
    // ...
}

func (s *Scheduler) checkAndUnblock(ctx context.Context, repo string, mergedPRNumber int) error {
    // 1. List all open issues
    issues, _ := s.listOpenIssues(ctx, repo)
    // 2. Detect circular deps
    // 3. For each issue with "Depends on: #N" deps, check if deps are closed
    // 4. If all deps closed → remove 'blocked', add 'ready', add reaction
}
```

### (d) Auto-retry mechanism needed

**Current problem**: The agent hits the merge queue block, `ErrBlocked` is returned, the session ends via `OnSessionBlocked`, and the branch is deleted. No mechanism exists to retry after the blocking PR merges.

**Where to add retry logic**:

1. **Option A — In `forgejo_tools.go`**: After `sentinel.ErrBlocked` is returned, instead of just returning the error, store the PR creation params (`params.Repository`, `params.Head`, `params.Base`, `params.Title`, `params.Body`) on the session and schedule a delayed retry. Check the `MergeGate` periodically until it clears.

2. **Option B — In `manager.go`**: When `OnSessionBlocked` fires, create a deferred task that watches for the blocking PRs' merge event. On merge, re-dispatch the same event to the agent session (if still alive) or spin up a new session.

3. **Option C — In `scheduler.go`**: Extend `OnPRMerged` to create synthetic events for any sessions that were blocked by the now-merged PR.

**Key data structures**:
- `mergequeue.Client.CheckGate` — already knows which PR numbers cause the block
- `sentinel.ErrBlocked` — already defined, already wrapped in the error message
- `lifecycle.Lifecycle.OnSessionBlocked` — already logs and labels; does NOT schedule retry

### Interface: `internal/mergequeue/queue.go:63`
```go
func (c *Client) CheckGate(ctx context.Context, repo, headBranch, baseBranch string) (bool, string, error)
```

---

## Gap 2: Forgejo Merge API 405

### (a) `MergePR` implementation: `internal/forgejo/client.go:144-179`

```go
func (c *Client) MergePR(ctx context.Context, repo string, number int, style string) error {
    apiPath := path.Join("/api/v1/repos", EscapeRepoPath(repo), "pulls", fmt.Sprintf("%d", number), "merge")

    resp, err := c.doRequest(ctx, http.MethodPost, apiPath, map[string]interface{}{
        "Do":                     style,
        "merge_commit_title":     "Merge PR",
        "merge_message":          "auto",
        "allow_unrelated_histories": true,
    })
    // ...
}
```

### (b) Payload sent

The payload keys `Do`, `merge_commit_title`, `merge_message`, and `allow_unrelated_histories` are sent. Per the AGENTS.md, Bug #31 fixed the 405 by adding `merge_commit_title` and `merge_message`, but **405 still occurs for some merge styles** on Forgejo v9.

### (c) Automerge flow in `manager.go:659-703`

```go
// Automerge label detection on PRs
if evt.Type == event.PullRequestLabelUpdated && evt.PRNumber > 0 {
    // ...
    if hasAutomerge {
        // Try direct API merge
        mergeErr := m.forgejoClient.MergePR(ctx, evt.Repository, evt.PRNumber, "merge")
        if mergeErr == nil {
            slog.Info("automerge: direct merge succeeded")
            return  // SUCCESS — no LLM session needed
        }
        slog.Warn("automerge: direct merge failed, falling back to reviewer")

        // Fallback: spawn reviewer session
        synthEvt := event.NewEvent(
            event.IssueCommentCreated, evt.Repository, evt.IssueNumber, evt.PRNumber,
            "automerge-trigger", "created",
        )
        synthEvt.SessionKey = fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber)
        synthEvt.Payload = map[string]interface{}{
            "comment": map[string]interface{}{"body": "[System] This PR has the 'automerge' label..."},
        }
        m.handleEvent(ctx, synthEvt)  // ← This spawns a reviewer agent session
    }
}
```

### (d) Automerge label workaround: `internal/tool/forgejo_tools.go:799-804`

```go
// Add 'automerge' label to trigger Forgejo native auto-merge.
if lerr := t.adapter.Client().AddIssueLabels(ctx, params.Repository, prResp.Number, []string{"automerge"}); lerr != nil {
    slog.Warn("create_pr: failed to add automerge label", "error", lerr)
}
```

The `automerge` label triggers Forgejo's native auto-merge (if configured). Per AGENTS.md, the 405 API merge still happens because this is a "workaround" — the label approach bypasses the API but the API merge attempt runs FIRST and fails before the label is even added.

**Fix approach**: Add the `automerge` label BEFORE calling `MergePR()` in manager.go, so Forgejo's native auto-merge handles it without needing the API call. If native auto-merge already merged it, the `Forgejo GetPR` check at line 680 (`prDetail.State == "open"`) would detect it's already merged and skip.

---

## Gap 3: PM Missing `[implementer]` Tags on Sub-Issues

### (a) PM system prompt: `internal/session/agent.go:569-597`

```go
case "pm":
    modeInstructions += `
## ROLE: Project Manager
...
**ROLE TAGS ARE MANDATORY**: Every sub-issue title MUST start with a role tag in brackets:
  - [implementer] for code implementation tasks
  - [devops] for CI/CD, Docker, infrastructure, deployment tasks
  - [tester] for testing, QA, integration test tasks
  - [reviewer] for code review, PR review tasks
  - Example: "[implementer] Implement git init command"
  - Example: "[tester] Write integration tests for auth flow"
  - Without these tags, the agent won't be assigned the correct tools and will fail.`
```

### (b) Where `CreateIssue` is called: `internal/tool/forgejo_tools.go:155-225`

The `forgejoCreateIssueTool.Execute()` method sends the agent-provided title/body directly to Forgejo's API. There is **no post-processing** of the title to inject role tags.

```go
func (t *forgejoCreateIssueTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    var params struct { Repository, Title, Body string }
    json.Unmarshal(args, &params)
    // ... dedup check ...
    apiPath := path.Join("/api/v1/repos", forgejo.EscapeRepoPath(params.Repository), "issues")
    payload := map[string]string{"title": params.Title, "body": body}
    result, err := t.adapter.doRequest(ctx, http.MethodPost, apiPath, payload)
    // ...
}
```

### (c) Auto-inject approach

The fix should go in `forgejoCreateIssueTool.Execute()` before the API call. Logic:
1. Parse the agent-provided title to detect the intended role from description text (e.g., "implement", "code", "test", "deploy")
2. Prepend `[role]` prefix if missing
3. The agent is told the PM creates issues — so the tool itself should sanitize titles

**Where to change**: `internal/tool/forgejo_tools.go` line ~217, right before the `doRequest` call:
```go
// Auto-inject role tag if missing
params.Title = autoInjectRoleTag(params.Title, params.Body)
```

**Alternative**: Add a tool-level validation step that returns an error to the agent if the title lacks a role tag, forcing the agent to re-submit. The `execute` pattern already returns errors that the LLM sees — this would cause 1 retry per sub-issue.

---

## Gap 4: Automerge Fallback LLM Session Wastes Turns

### (a) Fallback trigger: `internal/session/manager.go:659-703`

When `MergePR()` fails with 405:
```go
slog.Warn("automerge: direct merge failed, falling back to reviewer")
synthEvt := event.NewEvent(event.IssueCommentCreated, ...)
synthEvt.SessionKey = fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber)
synthEvt.Payload = map[string]interface{}{
    "comment": map[string]interface{}{"body": "[System] This PR has the 'automerge' label. Merge if it passes all checks."},
}
m.handleEvent(ctx, synthEvt)
```

### (b) Max turns per role: `internal/config/config.go:156-160` (defaults) + `internal/session/agent.go:539-553`

```go
// Defaults in config.go:156-160
MaxTurnsImplementer: 50,
MaxTurnsReviewer:    20,

// effectiveMaxTurns in agent.go:539-553
func (a *Agent) effectiveMaxTurns() int {
    switch a.role {
    case "pm":
        if a.cfg.Agent.MaxTurnsPM > 0 { return a.cfg.Agent.MaxTurnsPM }
    case "implementer":
        if a.cfg.Agent.MaxTurnsImplementer > 0 { return a.cfg.Agent.MaxTurnsImplementer }
    case "reviewer":
        if a.cfg.Agent.MaxTurnsReviewer > 0 { return a.cfg.Agent.MaxTurnsReviewer }
    }
    return a.cfg.Agent.MaxTurns
}
```

### (c) How the fallback session gets its role

In `runSession` (manager.go:992):
```go
role := detectRoleFromSession(ctx, m.forgejoClient, sess)
// ...
} else if sess.PRNumber > 0 && (role == "" || role == "implementer") {
    role = "reviewer"
}
```

The automerge-triggered synthetic event creates a session with key `pulls/N`. `detectRoleFromSession` checks the PR title and falls back to empty string, then the code above sets `role = "reviewer"`. So `MaxTurnsReviewer: 20` applies.

**Problem**: The reviewer gets the message "This PR has the 'automerge' label. Merge if it passes all checks." and then tries to call `forgejo_merge_pr` — which is the SAME API that just failed with 405. The agent burns ~10 turns exploring the code, checking tests, and trying to merge before maxing out.

### (d) Fix approaches

1. **Cap reviewer sessions spawned from automerge**: In `runSession`, detect if the session was auto-triggered (check sender or session key pattern) and override max_turns to something small (e.g., 2 turns — enough to read the PR state and give up).

2. **Detect 405 and skip fallback**: In manager.go line 685, if `MergePR` fails, check the error status code. If 405, add a comment saying "Automerge API not working — please merge manually" and return early without spawning the reviewer session.

3. **Use the `automerge` label BEFORE API merge**: The `forgejo_create_pr` tool already adds the `automerge` label (line 801). If we change manager.go to check if the PR already has the label and just wait (or re-attempt the merge later), we avoid spawning a session entirely.

---

## Gap 5: Agent Doesn't Reproduce Bugs Before Fixing

### (b) Implementer prompt: `internal/session/agent.go:861`

```go
- For BUG REPORTS: reproduce the bug FIRST. If the issue says 'crashes when X', run the code with X to confirm the crash BEFORE writing any fix.
- For write_file: supply ONLY the new file content. Do NOT copy the line numbers shown by read_file.
```

Also in `buildSystemPrompt`, the implementer-specific section includes:
```go
"DO NOT read the same file more than twice"
```

### (c) Steering messages: `internal/agent/turn.go:182-232`

The `ApplySteering` function injects contextual nudges:

1. **Per-tool repeat nudge** (line 136): Fires when a tool is called too many times — "You've called bash X times. Stop exploring."
2. **Duplicate output nudge** (line 174): Fires when bash/git returns the same output — "Same output as previous call."
3. **Hard gate write enforce** (line 187): At turn 15, removes exploration tools if no `write_file` has been called.
4. **Turn budget thresholds** (line 225): 40%/60%/80%/90% budget warnings.

**None of these steering messages address "reproduce before fix" specifically.** The steering system is turn-proportional and tool-call-proportional, not behavior-pattern-aware.

### (d) Forced "reproduce first" step approaches

1. **Prompt-level**: Strengthen the bug report instructions with a mandatory first-step. Instead of a suggestion, make it an explicit instruction:
   ```
   STEP 1 IS MANDATORY: Build the project and run it with the bug scenario BEFORE writing any code.
   If the bug cannot be reproduced, post a comment explaining why and STOP.
   ```

2. **Tool-level guard**: Add a `bug_reproduced` flag to the agent. On the first few turns of a bug report session, reject `write_file` and `git commit` calls with a message: "You must reproduce the bug first. Run the code with the trigger scenario and confirm the crash/bug."

3. **Steering-level**: Add a new steering message in `ApplySteering` that fires on turn 2-3 of bug report sessions if no successful run/compile has been observed yet (check for "build failed" or "no write_file" patterns). Message: "This is a BUG report, not a feature request. Reproduce the bug by running the code with the described trigger scenario BEFORE writing fixes."

4. **Pre-computation step**: In `runSession`, before the agent loop, auto-run `go build` (or a build detection for the project type). If build fails, inject a steering message. If the issue is a bug report and the build succeeds, inject a message confirming "The code compiles. Now reproduce the specific bug scenario."

### Data sources for fix:
- `internal/session/agent.go:861` — current bug report instruction (weak: suggestive, not mandatory)
- `internal/agent/turn.go:182-232` — steering system (can add new steering type, but would need a way to detect "bug report session")
- `internal/session/manager.go:1313` — `detectRoleFromTitle` already recognizes `[tester]` role (bug reports could use `[tester]` or `[bug]` tags)
- `internal/tool/local_tools.go` — the `build` and `test` commands in `forgejo_create_pr` could be extended to auto-run before a bug-fix session starts

---

## Summary of Files to Change

| Gap | Primary Files | Lines |
|-----|--------------|-------|
| 1 - Merge queue retry | `internal/tool/forgejo_tools.go` | 648-686 (CheckGate call) |
|  | `internal/session/manager.go` | 1114-1126 (ErrBlocked handling) |
|  | `internal/mergequeue/queue.go` | 63-118 (CheckGate logic) |
|  | `internal/scheduler/scheduler.go` | 74-88, 155-210 (OnPRMerged/unblock) |
| 2 - 405 merge | `internal/forgejo/client.go` | 144-179 (MergePR) |
|  | `internal/session/manager.go` | 659-703 (automerge detection + fallback) |
|  | `internal/tool/forgejo_tools.go` | 799-804 (automerge label addition) |
| 3 - PM role tags | `internal/tool/forgejo_tools.go` | 155-225 (CreateIssue tool execute) |
|  | `internal/session/agent.go` | 569-597 (PM prompt) |
| 4 - Automerge reviewer cap | `internal/config/config.go` | 156-160 (defaults) |
|  | `internal/session/manager.go` | 659-703, 992-1025 (runSession + role detection) |
|  | `internal/session/agent.go` | 539-553 (effectiveMaxTurns) |
| 5 - Bug reproduction | `internal/session/agent.go` | 861 (bug prompt) |
|  | `internal/agent/turn.go` | 182-232 (steering) |
|  | `internal/tool/local_tools.go` | build/test commands |
