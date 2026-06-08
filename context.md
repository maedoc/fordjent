# PR Review Feedback Flow — Scouting Findings

## 1. How PR Review Sessions Are Created and What Role They Get

### Session Creation Flow

PR review sessions are created through **two paths**:

**Path A: Automerge label detected** (`Manager.handleEvent`, line ~476)
When `pull_request.label_updated` fires and an `automerge` label appears on an open PR:
- A synthetic `IssueCommentCreated` event is created with session key `repo/pulls/N`
- The synthetic payload contains a comment body: `"[System] This PR has the 'automerge' label. Review the code and merge if it passes all checks."`
- This triggers `getOrCreate()` → `runSession()`

**Path B: PR comment/ review event** (`Agent.ProcessEvent`, line ~574)
When a `issue_comment.created` or `pull_request_review_comment.created` event arrives with `PRNumber > 0`:
- The session was already created (likely by automerge path A or a prior PR opened event)
- `ProcessEvent()` checks for PR comments and checks out the PR branch

**Role assignment** (`Manager.runSession`, line ~956):
```go
role := detectRoleFromSession(ctx, m.forgejoClient, sess)
if sess.IsPMFollowUp { role = "pm" }
if sess.IsScaffoldAnswer { role = "implementer" }

// ALL PRs get a reviewer session to inspect and merge code.
// Bot PRs retain auto-bypass for merge approval.
if sess.PRNumber > 0 && (role == "" || role == "implementer") {
    role = "reviewer"
}
```

This means **any PR session defaults to "reviewer" role** unless the title contains a role tag (e.g., `[review]`) that maps to a different role. The `detectRoleFromSession()` function checks both issue labels/title AND PR title for role tags.

**Session key formats**:
- Review session: `fjadmin/repo/pulls/N`
- Issue session: `fjadmin/repo/issues/N`

---

## 2. Tools: Reviewer vs Implementer Role

### Reviewer Role (`buildRoleRegistry`, `case "reviewer":`)
| Tool | Available | Purpose |
|------|-----------|---------|
| `forgejo_comment` | ✅ | Post review comments |
| `forgejo_list_issues` | ✅ | List issues |
| `forgejo_get_issue` | ✅ | Get issue details |
| `forgejo_search_code` | ✅ | Code search |
| `forgejo_add_reaction` | ✅ | Add emoji reactions |
| `forgejo_list_branches` | ✅ | List branches |
| `forgejo_list_prs` | ✅ | List PRs |
| `forgejo_pr_files` | ✅ | Files changed in a PR |
| `forgejo_list_files` | ✅ | List repo files |
| `forgejo_list_hooks` | ✅ | List webhooks |
| `forgejo_list_collabs` | ✅ | List collaborators |
| `forgejo_get_version` | ✅ | Forgejo version info |
| `forgejo_get_user` | ✅ | User info |
| `forgejo_get_sibling_issues` | ✅ | Related issues |
| `read_file` | ✅ | Read file content |
| `forgejo_merge_pr` | ✅ (with `bypassHumanApproval=true`) | Merge the PR |
| `open_spec_read_spec` | ✅ | Read spec requirements |
| `open_spec_get_tasks` | ✅ | Read spec tasks |
| `open_spec_read_change` | ✅ | Read spec change details |
| `bash` | ❌ **EXPLICITLY BLOCKED** | No shell access for reviewers |
| `write_file` | ❌ Not registered | Cannot write files |
| `git` | ❌ Not registered | Cannot run git |
| `forgejo_create_pr` | ❌ Not registered | Cannot create new PRs |
| `forgejo_create_issue` | ❌ Not registered | Cannot create sub-issues |
| `ping_parent` | ❌ Not registered | Cannot ping PM |
| `delete_branch` | ❌ Not registered | Cannot delete branches |

### Implementer Role (`buildRoleRegistry`, `case "implementer" fallthrough`)
| Tool | Available |
|------|-----------|
| Everything a reviewer has (common tools) | ✅ |
| `bash` | ✅ |
| `write_file` | ✅ |
| `git` | ✅ |
| `forgejo_create_pr` | ✅ |
| `forgejo_merge_pr` | ✅ (with `bypassHumanApproval=false` — requires human approval for non-bot PRs) |
| `forgejo_create_issue` | ✅ |
| `open_spec_mark_task` | ✅ |
| `ping_parent` | ✅ |
| `delete_branch` | ✅ |
| `create_hook` / `delete_hook` | ✅ |

### Key Difference
The reviewer is **explicitly given no implementation tools** — `write_file`, `git`, `bash`, `forgejo_create_pr` are simply not registered in the registry. This is the primary defense. There are no "soft" restrictions or prompt-level only blocks for the reviewer — the tools simply do not exist in their tool catalog.

---

## 3. System Prompt Differences: Reviewer vs Implementer

### Reviewer System Prompt (agent.go, ~line 707)
```
## ROLE: Code Reviewer
You are in Code Review mode. You do NOT write code or push commits. Your job is:
- Examine the PR using read_file and forgejo_list_prs (view files, diff).
- Check for correctness, style, test coverage, and edge cases.
- If issues found, post a comment describing what needs to change.
- DO NOT leave PRs open indefinitely — either merge or request changes.
```
Plus policy-aware merge instructions (no-auto-merge, require-review, automerge label checks).

**Plus spec-driven review instructions** (openspec reading, verification criteria).

### PM System Prompt
Extensive — includes PM role, milestone tools, sub-issue creation, OpenSpec spec creation, plan-first policy, PM follow-up mode, scaffold answer mode.

### Implementer System Prompt
Extensive — includes implementer role, scope restriction, action-first workflow, anti-patterns (don't read same file >2x, don't explore more than needed), bug report reproduction guidance, write_file line number stripping.

### PR Review Mode (shared across roles when PRComment event arrives)
```
## PR Review Mode (IMPORTANT)
You are responding to a review comment on an existing pull request.
- You are already on the PR branch (check git status if unsure).
- Make your fixes directly on this branch.
- After fixing, commit and push to the SAME branch.
- Do NOT create a new PR — the PR already exists.
- Post a comment confirming which issues were fixed.
- If the PR is mergeable with no conflicts, you may call forgejo_merge_pr to merge it automatically.
```

**Note**: The "PR Review Mode" instructions are for **implementers responding to review feedback**, NOT for the reviewer role. The reviewer already has read-only tools. There's no mechanism to detect whether a human wrote "request changes" vs "approved" — the review feedback is just a comment body passed as context.

---

## 4. PR Comment Event Routing (Webhook → Event → Session)

### Step 1: Webhook Receives `issue_comment.created`
`Router.handleWebhook()` at `POST /acp/v1/events`:
- Validates HMAC signature
- Reads `X-Forgejo-Event` header → `issue_comment`
- Extracts action → `created`
- Normalizes to event type `issue_comment.created`
- Extracts issue/PR number, repo, sender, session key

### Step 2: Session Key Correction for PR Comments
`normalizeEvent()` calls `extractPRNum()`:
```go
if issue, ok := payload["issue"].(map[string]interface{}); ok {
    if isPR, ok := issue["is_pull_request"].(bool); ok && isPR {
        return extractIssueNum()  // → the PR number
    }
}
```
However, **Forgejo v9 does NOT include `is_pull_request` in webhook payloads** (agent.go line ~549). So `extractPRNum()` returns 0, and the session key is wrongly set to `repo/issues/N`.

**Workaround in `handleWebhook`** (line ~552):
```go
if evt.PRNumber == 0 && ... r.forgejo != nil {
    issue, err := r.forgejo.GetIssue(req.Context(), evt.Repository, evt.IssueNumber)
    if err == nil && issue.PullRequest.IsPR() {
        evt.PRNumber = evt.IssueNumber
        evt.SessionKey = fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.IssueNumber)
    }
}
```
This adds an **API roundtrip** to correct the session key for PR comments.

### Step 3: Agent Event Filter
`isAgentEvent()` filters out events from the agent itself:
- Checks `<!-- ford -->` marker in comment body
- Filters comments from `fordjent-bot` user
- Push events and merged PR events are NEVER filtered

### Step 4: Closed PR Guard
Comments on **closed/merged PRs** are skipped to prevent token burn:
```go
if evt.Type == "issue_comment.created" && evt.PRNumber > 0 && r.forgejo != nil {
    pr, _ := r.forgejo.GetPR(req.Context(), evt.Repository, evt.PRNumber)
    if pr.State == "closed" { return "skipped_closed_pr" }
}
```

### Step 5: Event Published to Bus
`bus.Publish()` fans out the event to all subscribers (session manager's event queue).

### Step 6: Session Manager Routes to Session
`Manager.handleEvent()`:
- For `issue_comment.created` on PRs, the event falls through (no special gating)
- If session exists, event is queued to `sess.events` channel
- `runSession()` picks up the event → `Agent.ProcessEvent()`

### Step 7: PR Branch Checkout (agent.go ~line 574)
```go
if evt.PRNumber > 0 && (evt.Type == event.IssueCommentCreated || evt.Type == event.PullRequestReviewComment) {
    pr, _ := a.forgejo.GetPR(...)
    if pr.Head.Ref != "" {
        exec.Command(... "fetch origin " + pr.Head.Ref)
        exec.Command(... "checkout -B " + pr.Head.Ref + " origin/" + pr.Head.Ref)
        // Inject context message about PR branch
    }
}
```
This checks out the PR branch before the LLM loop runs — critical for implementer sessions responding to review feedback.

---

## 5. "Human Requested Changes" Detection — **NOT IMPLEMENTED**

After thorough searching, there is **no existing logic** that auto-detects whether a human reviewer wrote "request changes" vs "approve". The review comment body is passed as plain text context to the LLM, and:

1. **Reviewer role** has `forgejo_merge_pr` tool. The **system prompt** instructs reviewers to "either merge or request changes" but the model decides based on reading the comment body text.

2. **Implementer role in PR Review Mode** is told to "Make fixes directly on this branch" but has no signal about whether changes are actually required vs optional improvements.

3. **Agent event filter** (`isAgentEvent`) only filters by `<!-- ford -->` marker and sender identity — not by comment content semantics.

4. **No human review state machine**: There's no concept of tracking "human requested changes" state. The only states are the FSM labels (`planning`, `implementing`, `ready`, `blocked`, `done`, `plan_approved`).

### Implication
If you want the system to distinguish between:
- "Human wrote: 'Please fix X'" → should auto-reassign to implementer → implementer fixes → pushes to PR branch
- "Human wrote: 'LGTM'" → should auto-merge (if automerge enabled)

...this **does not exist**. You would need to implement it. Options:
1. LLM-based classification of comment content
2. Manual label addition by human (e.g., `needs-fix`)
3. Forgejo review API integration (checking `pr_reviews` for `REQUEST_CHANGES` vs `APPROVED` state)

The merge queue does check `request_reviewers`, but that's for POST-PR-creation reviewer assignment, not for interpreting review feedback.

---

## Start Here

1. **`internal/session/agent.go`** (lines 574-600): PR branch checkout logic — the code that switches the working tree to the PR branch before processing a review comment
2. **`internal/session/agent.go`** (lines 707-730): Reviewer system prompt — the role-specific instructions
3. **`internal/session/agent.go`** (lines 1311-1400): `buildRoleRegistry()` — the complete tool assignment per role (the authoritative source for what tools each role has)
4. **`internal/session/manager.go`** (lines 956-970): `runSession()` — the role assignment logic, including the `reviewer` override for PRs
5. **`internal/webhook/router.go`** (lines 490-560): `normalizeEvent()` + `handleWebhook` — the webhook → event → session key mapping, including the PR comment correction hack
