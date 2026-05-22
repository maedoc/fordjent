# Noise Reduction Plan: Replace Text Firehose with Forgejo Native Features

## Problem

Fordjent's agent generates a firehose of comments that humans must wade through.
In `marmaduke/test01`, 41 of 42 comments were from the agent (19K chars of text).
Humans quickly learn to ignore the issue comment stream entirely.

## Current Comment Sources (All Automated, All from Agent)

| # | Source | What It Says | Current Mechanism | Frequency |
|---|--------|-------------|-------------------|-----------|
| 1 | Session complete | "Session completed successfully (implementation). Total: 98173 tokens ($0.0280 USD)" | `lifecycle.OnSessionComplete` → PostIssueComment | Every session |
| 2 | Dependency unblock | "All dependencies are now resolved. This issue is unblocked and ready to work on! (Priority: 2)" | `scheduler.checkAndUnblock` → PostIssueComment | When deps resolve |
| 3 | Depends-on in body | "Depends on: #6" written into issue body as text | `forgejo_create_issue` tool writes it | Every PM sub-issue |
| 4 | Scaffold block | "This repository needs a scaffold first. Please wait for #2 to be resolved" | `scaffold.CheckAndBlock` → PostIssueComment | Empty repo |
| 5 | Role gate guidance | "I need a role tag in the title before I can start working on it" | `postRoleGuidance` → PostIssueComment | Untagged issues |
| 6 | Max turns failure | "This session reached the maximum turn limit and could not finish the task." | `lifecycle.OnSessionFailedMaxTurns` → PostIssueComment | Session hits limit |
| 7 | Error failure | "The agent session failed with an error: ..." | `lifecycle.OnSessionFailedError` → PostIssueComment | Session crashes |
| 8 | Merge queue block | "This issue is blocked by the merge queue. ..." | Agent tool error → PostIssueComment | File conflict |
| 9 | Plan approved | "Plan approved on #1. This issue is now ready for implementation." | `unblockSubIssues` → PostIssueComment | Human approves plan |
| 10 | Auto-retry nudge | "Auto-retry: attempting session for previously failed issue" | `runAutoRetry` → PostIssueComment | Retry timer |
| 11 | Circular dep warning | "Circular dependency detected involving issue #3" | `scheduler.checkAndUnblock` → PostIssueComment | Cycle detection |
| 12 | Parent completion | "All sub-issues are now complete! 3/3 children merged or closed." | `scheduler.closeParent` → PostIssueComment | All children closed |
| 13 | Agent LLM comments | Whatever the LLM decides to post via `forgejo_comment` tool | Agent's tool call | Variable (0-5+ per session) |

---

## Forgejo Native API Features That Replace Text

### 1. Issue Dependencies API (replaces "Depends on: #N" text)

**Endpoints (verified on our Forgejo v9.0.3):**
- `POST /repos/{owner}/{repo}/issues/{index}/dependencies` — Set dependency (body: `IssueMeta{index, owner, repo}`)
- `GET /repos/{owner}/{repo}/issues/{index}/dependencies` — List what blocks this issue
- `DELETE /repos/{owner}/{repo}/issues/{index}/dependencies` — Remove a dependency
- `POST /repos/{owner}/{repo}/issues/{index}/blocks` — Set blocking relationship (reverse direction)
- `GET /repos/{owner}/{repo}/issues/{index}/blocks` — List what this issue blocks

**UI effect:** Forgejo renders dependency relationships as a dedicated "Dependencies" section on the issue page, with explicit UI for adding/removing. Users can click through.

**What this replaces:**
- ❌ `Depends on: #N` text in issue bodies (source #3)
- ❌ "All dependencies are now resolved" comments (source #2)
- ❌ "Circular dependency detected" comments (source #11)

**How the UI already communicates state changes:** When a dependency is resolved (the blocking issue closes), Forgejo updates the dependency section automatically. No comment needed.

### 2. Issue Labels (already used, could replace more text)

**What they already replace:**
- `ready` → "unblocked" (source #2 partially)
- `blocked` → "blocked by..." (source #4 partially)
- `fordjent/failed:max-turns` → "hit max turns" (source #6 partially)
- `fordjent/failed:error` → "session error" (source #7 partially)
- `in_progress` → "working on it" (no comment needed)

**What they could additionally replace:**
- `plan-approved` → already a label, but we still post "Plan approved on #1" comment (#9) — redundant
- `scaffold` → already a label, still post "repository needs scaffold" comment (#4) — redundant

### 3. Reactions (replies to agent status)

**Endpoints:**
- `POST /repos/{owner}/{repo}/issues/{index}/reactions` — Add reaction to issue
- `POST /repos/{owner}/{repo}/issues/comments/{id}/reactions` — Add reaction to comment

**What this could replace:**
- Session complete → 👀 or ✅ reaction on the original issue (instead of comment #1)
- Agent already uses `addReaction` for some signals (e.g. `no_entry_sign` on merge queue block)

### 4. Commit Statuses (build/test feedback without comments)

**Endpoint:** `POST /repos/{owner}/{repo}/statuses/{sha}`

**Body:** `{state: "success"|"failure"|"pending"|"error", context: "fordjent/build", description: "go test passed", target_url: "..."}`

**What this renders as:** A green checkmark / red X / yellow dot on the commit in the PR timeline. No comment needed.

**What it could replace:**
- "Session completed successfully (implementation)" → commit status `success` on the PR's head SHA
- "Build failed" agent comments → commit status `failure` on the head SHA
- The agent wouldn't need to post "I tested the code" as a comment at all

### 5. Milestones (project grouping)

**Endpoints:**
- `POST /repos/{owner}/{repo}/milestones` — Create milestone
- `PATCH /repos/{owner}/{repo}/issues/{index}` — Set `milestone` field

**What it could replace:**
- PM currently doesn't group sub-issues at all. Adding milestone assignment (`milestone: "Sprint 1"`) makes the Forgejo UI show progress bars automatically — no comment needed.

### 6. Issue Pinning (importance signaling)

**Endpoints:**
- `POST /repos/{owner}/{repo}/issues/{index}/pin` — Pin issue

**What it could signal:**
- Scaffold issues could be pinned while in progress (instead of posting "waiting for scaffold" comment)
- Critical/blocking issues could be escalated visually instead of textually

### 7. Requested Reviewers (PR review workflow)

**Endpoint:** `POST /repos/{owner}/{repo}/pulls/{index}/requested_reviewers`

**What it replaces:**
- Instead of agent posting "Ready for review!" comment, it could add the human as a requested reviewer. Forgejo emails them automatically.

---

## Proposed Changes (Priority Order)

### P0: Use Native Dependencies API Instead of "Depends on:" Text

**Biggest bang for buck.** This is what the beta tester specifically called out.

1. **Add `AddIssueDependency` / `RemoveIssueDependency` / `ListIssueDependencies` to Forgejo client**
2. **PM agent**: When creating sub-issues, call `AddIssueDependency` (sub depends on scaffold, sub depends on other subs) instead of writing "Depends on: #N" in the body
3. **Scheduler**: Query `GET /dependencies` instead of parsing `Depends on:` regex from body text
4. **Scheduler on unblock**: Just remove the `blocked` label. The Forgejo UI already shows the dependency is resolved. No "All dependencies resolved!" comment needed.
5. **Circular dep detection**: Query the dependency graph via API instead of parsing text. No "Circular dependency detected" comment — just the `blocked` label.

**Migration**: The scheduler's `parseDependsOn()` body parser becomes a fallback for old issues. New issues use API-only.

**Estimated comment reduction:** ~15% of all agent comments (sources #2, #3, #11)

### P1: Replace "Session Completed" Comments with Commit Statuses + Reactions

1. **On session complete (implementer):** 
   - Post commit status `success` on the PR head SHA: `{context: "fordjent/agent", state: "success", description: "Implementation complete (98K tokens)"}`
   - Add ✅ reaction to the issue (no comment)
2. **On session complete (PM):** Add 👀 reaction to the parent issue (no comment)
3. **On session failed:** Post commit status `failure` on the head SHA + add ❌ reaction (no comment)

**Estimated comment reduction:** ~30% of all agent comments (source #1 — the most frequent one)

### P2: Remove Redundant Label-Already-Communicates Comments

These comments duplicate what labels and native features already say:

| Comment | What replaces it |
|---------|------------------|
| "Repository needs a scaffold first" (#4) | `blocked` label + `scaffold` issue link in dependencies |
| "Plan approved on #1" (#9) | `plan-approved` + `ready` labels already shown in UI |
| "Auto-retry: attempting session" (#10) | Just remove `fordjent/failed:max-turns` + add `ready` — label transition visible in timeline |
| "This session reached the maximum turn limit" (#6) | `fordjent/failed:max-turns` label + ❌ reaction |
| "Agent session failed with error" (#7) | `fordjent/failed:error` label + ❌ reaction |

**Estimated comment reduction:** ~25% of all agent comments (sources #4, #6, #7, #9, #10)

### P3: Request Reviewers Instead of "Ready for Review" Comments

1. Agent creates PR → adds repo owner (or configurable reviewer) as requested reviewer
2. Forgejo emails them automatically
3. No "Ready to merge" or "Please review" comment needed

**Estimated comment reduction:** ~10% (review-related comments)

### P4: Milestones for PM-Generated Sub-Issues

1. PM creates sub-issues → assigns them all to a milestone named after the parent issue
2. Forgejo shows a progress bar on the milestone page
3. No "3/3 children merged" comment on the parent (#12)

**Estimated comment reduction:** ~5%

### P5: Consolidate Agent LLM Comments

The agent's own `forgejo_comment` tool calls are the hardest to control since they're LLM-generated. Options:

1. **Add max-comments-per-session** limit in config (e.g., 3 comments per session)
2. **Mark agent comments as "hidden"** by default in UI — collapsible section
3. **Instruct agent in system prompt**: "Post at most ONE progress comment when done. Never post intermediate status updates."
4. **Replace with commit statuses**: Agent posts `pending` status when starting work, `success`/`failure` when done

**Estimated comment reduction:** ~15% (LLM-generated comments)

---

## Total Estimated Reduction

| Priority | Comments Eliminated | % of Total |
|----------|-------------------|------------|
| P0: Native dependencies | 15% | Structural |
| P1: Commit statuses + reactions | 30% | Biggest single win |
| P2: Remove label-redundant | 25% | Easy cleanup |
| P3: Request reviewers | 10% | PR-specific |
| P4: Milestones | 5% | PM-specific |
| P5: Agent comment limit | 15% | LLM behavior |
| **Total** | **~85-90%** | |

After these changes, the only remaining comments from the agent would be:
- The LLM's actual work summary (1 comment per session, capped)
- Truly exceptional conditions that need human text (e.g., "merge conflict requires manual resolution")

---

## Implementation Order

1. **P0 (Dependencies API)**: Requires `forgejo.Client` additions + scheduler refactor + PM prompt change
2. **P1 (Commit statuses)**: Requires `forgejo.Client` additions + lifecycle changes
3. **P2 (Label-redundant)**: Just delete/comment out the `PostIssueComment` calls — trivial
4. **P3 (Reviewers)**: Requires `forgejo.Client` additions + PR creation tool change
5. **P4 (Milestones)**: Requires `forgejo.Client` additions + PM prompt change
6. **P5 (Agent LLM limit)**: System prompt change + optional enforcement in `forgejo_comment` tool

P0 and P1 should be done together as they're the biggest wins and share client infrastructure.
P2 can be done immediately (just delete PostIssueComment calls).
P3-P5 are incremental.

## Dependency API Details (for implementation)

```
IssueMeta = { index: int64, owner: string, repo: string }

POST   /repos/{owner}/{repo}/issues/{index}/dependencies   → 201 Issue  (sets: this issue depends on IssueMeta)
GET    /repos/{owner}/{repo}/issues/{index}/dependencies   → 200 [Issue] (lists: what blocks this issue)
DELETE /repos/{owner}/{repo}/issues/{index}/dependencies   → 200 Issue  (removes dependency)

POST   /repos/{owner}/{repo}/issues/{index}/blocks         → 201 Issue  (sets: this issue blocks IssueMeta)
GET    /repos/{owner}/{repo}/issues/{index}/blocks          → 200 [Issue] (lists: what this issue blocks)
DELETE /repos/{owner}/{repo}/issues/{index}/blocks         → 200 Issue  (removes blocking)
```

Note: `dependencies` and `blocks` are two sides of the same relationship:
- `A depends on B` ≡ `B blocks A`
- Setting one automatically sets the other.
- `POST /issues/A/dependencies {index: B}` = `POST /issues/B/blocks {index: A}`
- The Forgejo timeline shows `add_dependency` events for these, visible in the issue timeline UI.

## Commit Status API Details

```
CreateStatusOption = {
  context:     string    // e.g. "fordjent/agent", "fordjent/test"
  description: string    // e.g. "Implementation complete (98K tokens)"
  state:       string    // "pending" | "success" | "error" | "failure"
  target_url:  string    // optional link to details
}

POST /repos/{owner}/{repo}/statuses/{sha}   → 201 CommitStatus
GET  /repos/{owner}/{repo}/commits/{ref}/status   → 200 Status (combined)
```

The status appears as a badge on every commit in the PR. Green checkmark = success. No comment needed.