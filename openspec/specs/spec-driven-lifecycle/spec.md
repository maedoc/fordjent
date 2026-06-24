# Spec-Driven Lifecycle

## Purpose

Govern *when* Fordjent agent roles (PM, implementer, reviewer/djent-qa)
activate and *what* they hand off to each other. This spec is the source of
truth for the event-to-role routing table, the lifecycle stage machine, the
automerge gate, and the dev↔QA rework loop; it delegates the *how* of each
role's craft to the role-specific specs.

## Context

Fordjent coordinates multiple agent roles (PM, implementer, reviewer/djent-qa) through Forgejo webhooks. Prior specs defined each role's craft in isolation: PM writes specs (`pm-spec-authoring`), implementers build from them (`spec-driven-implementation`), reviewers verify them (`spec-driven-review`), and Ralph extends difficult implementations (`ralph-scheduler`, `ralph-protocol`). This spec defines the explicit handoff chain and stage machine that binds those roles into a coherent lifecycle.

This spec is the source of truth for:
- Which role handles which event
- How each role's output becomes the next role's input
- When a change moves from one lifecycle stage to the next
- Where Ralph fits in the sequence (it is a harness technique, not a lifecycle stage)

It delegates to the craft specs for *how* a role performs its work; this spec only governs *when* roles activate and *what* they hand off.

## Decisions

### Decision 1: Ralph is an implementation harness extension, not a lifecycle stage

Ralph fresh-session iteration is a technique for extending a single implementer's work when the bounded session turn budget is insufficient. It does not create a new lifecycle stage; it happens entirely within the `implementing` stage. The lifecycle transitions from `implementing` to `reviewing` after Ralph signals it is done and the `ralph-completed` label is applied; the `ralph-completed` label tells the reviewer to read `.ralph/progress/` but does not create a new stage.

### Decision 2: Event routing uses PR labels, not event type alone

The same webhook event (`issue_comment.created`) can arrive for a spec PR, a normal implementation PR, or a Ralph PR. Routing is determined by the PR's labels at the time of the event, not the event type. This is expressed as a routing table (see Requirement: Event-to-Role Routing Table below).

### Decision 3: tasks.md is the system-of-record for task state; spec.md verification checkboxes are human-auditable records

Implementers and Ralph read `tasks.md` to know what work remains. Spec verification checkboxes (`## Verification` sections in spec files) are checked by djent-qa during review as a human-auditable record. Agents do not use checkbox state for coordination.

### Decision 4: PM archival is a natural consequence of the chain, not a separate trigger event

PM archival fires when all task issues for a change are closed. A task issue only closes when its PR merges. A Ralph PR only merges after djent-qa review. Therefore, if any task is still in Ralph, its issue is open, and archival is naturally blocked. No explicit "check Ralph before archive" gate is required.

---
## Requirements
### Requirement: Lifecycle Stage Machine

A change SHALL progress through the following stages. Each stage transition is unidirectional and SHALL be triggered by a specific event or action defined in the Handoff Rules.

| Stage | Meaning | Triggering Transition |
|-------|---------|----------------------|
| `spec-proposed` | PM has written spec files and opened a spec PR (non-yolo) or committed to main (yolo) | PM creates spec artifacts |
| `spec-approved` | Spec PR merged to main; spec is immutable from this point forward | `PullRequestMerged` on spec PR |
| `implementing` | Implementer issues exist; implementers or Ralph work on them | Scheduler creates tasks from tasks.md |
| `reviewing` | Implementation PR created (normal or post-Ralph); awaiting djent-qa review | Implementer creates PR or Ralph signals done |
| `merging` | djent-qa has approved PR; merge is permitted by policy | djent-qa approves / calls `forgejo_merge_pr` |
| `archived` | All task issues for change are closed; PM has archived change directory | Scheduler detects all tasks closed |

Transitions:
- `spec-proposed → spec-approved`: Spec PR merged to main. In yolo mode, this is immediate (PM commits directly).
- `spec-approved → implementing`: Scheduler reads `tasks.md` from merged spec, creates implementer issues.
- `implementing → reviewing`: Implementer creates PR and build gate passes (no Ralph). OR Ralph harness signals done (`ac_met: true`, `ralph-completed` label added).
- `reviewing → merging`: djent-qa approves and policy allows merge.
- `merging → archived`: Scheduler detects all task issues for this change are closed, dispatches `ArchiveChangeRequested`.

#### Scenario: Yolo mode skips spec-proposed stage
- **GIVEN** repo has `fordjent-yolo` topic
- **WHEN** PM writes spec files
- **THEN** PM commits directly to `main`
- **AND** lifecycle starts at `spec-approved`
- **AND** scheduler immediately reads `tasks.md` and creates implementer issues

#### Scenario: Normal non-yolo flow
- **GIVEN** repo does NOT have `fordjent-yolo` topic
- **WHEN** PM creates spec PR #10 with label `spec-proposed`
- **AND** human reviews and merges PR #10
- **THEN** lifecycle transitions `spec-proposed → spec-approved`
- **AND** scheduler creates implementer issues from merged `tasks.md`
- **AND** lifecycle transitions to `implementing`

#### Scenario: Ralph extends hard PR within implementing stage
- **GIVEN** lifecycle is in `implementing` stage for task issue #20
- **AND** implementer creates PR #25
- **AND** turn budget is exhausted before PR satisfies AC
- **THEN** the `ralph` label is added to PR #25
- **AND** Ralph harness spawns iterations within the `implementing` stage
- **AND** lifecycle does NOT transition until Ralph removes `ralph` and adds `ralph-completed`

#### Scenario: Ralph completes and transitions to reviewing
- **GIVEN** Ralph iteration 7 for PR #25 records `ac_met: true`
- **WHEN** the iteration completes
- **THEN** the `ralph` label is removed from PR #25
- **AND** the `ralph-completed` label is added
- **AND** lifecycle transitions from `implementing` to `reviewing`
- **AND** a reviewer session is queued for PR #25

#### Scenario: Normal PR transitions to reviewing without Ralph
- **GIVEN** implementer creates PR #30
- **AND** build gate passes
- **AND` turn budget is not exhausted
- **THEN** implementer session completes normally
- **AND** PR #30 has no `ralph` label
- **AND** lifecycle transitions from `implementing` to `reviewing`
- **AND** a reviewer session is queued for PR #30

---

### Requirement: Event-to-Role Routing Table

When a webhook event arrives, the system SHALL determine the target role by inspecting the PR labels (for PR events) or issue labels (for issue events) and applying the rules below. Each rule is evaluated in order; the first match wins.

| Priority | Event Type | Condition | Session Key | Role | Notes |
|----------|-----------|-----------|-------------|------|-------|
| 1 | `issue_comment.created` | PR labels contain `spec-proposed` or `spec-approved` | `pulls/N` | PM | Spec PR review comment |
| 2 | `pull_request_review_comment.created` | PR labels contain `spec-proposed` or `spec-approved` | `pulls/N` | PM | Spec PR review thread |
| 3 | `pull_request_review_comment.created` | PR labels contain `changes_requested` OR `djent-qa` review of state `changes_requested` | `pulls/N-fix` | Implementer | Reviewer (human OR djent-qa) requested changes; implementer fix session |
| 4 | `issue_comment.created` | Sender is human OR djent-* (non-marker body); `issue.is_pull_request` is true; PR has no spec/ralph labels | `pulls/N` | Reviewer (djent-qa) | Normal PR review comment. Cost-summary comments carrying the `<!-- ford -->` marker are dropped. |
| 5 | `pull_request_review_comment.created` | PR labels none of above | `pulls/N` | Reviewer (djent-qa) | Normal PR review thread |
| 6 | `pull_request.merged` | Any | `pulls/N` | Scheduler | Scheduler processes task completion, checks all tasks |
| 7 | `check_run.completed` | Conclusion is `failure`/`cancelled`/`action_required`; PR exists for head SHA (resolved via API when `pull_requests` field is empty); PR has no spec/ralph/merging labels | `pulls/N-fix` | Implementer | Failed CI on a dev PR — rework the fix |
| 8 | `workflow_run.completed` | Conclusion is `failure`/`cancelled`; same PR pre-conditions as rule 7 | `pulls/N-fix` | Implementer | Same path; redundant when `check_run` already fired for the same run |
| 9 | `issue.closed` | Issue is a task issue (linked to change) | `issues/N` | Scheduler | Scheduler checks if all tasks for change are done |
| 10 | (derived) | All task issues for a change are closed | `issues/<parent>` | PM | PM archival triggered by scheduler event |

**Routing table sovereignty**: No other component creates sessions independently. All Fordjent session creation SHALL flow through this routing table.

**Label priority**: If a PR has multiple labels (e.g. both `spec-proposed` and `changes_requested`), the higher-priority rule wins. This is a configuration error; the system SHALL log a warning.

**Sender-filtering note**: The duplicate routing predicate in `Manager.getOrCreateSession` SHALL NOT exclude `djent-*` senders from the `pulls/N-fix` path. Self-comments from `djent-*` senders carrying the `<!-- ford -->` marker are still filtered (by `isAgentEvent`) to suppress cost-summary noise. This updates the historically-human-only behavior introduced in Bug 39/41 to include the djent-qa reviewer as a valid feedback source.

**Failed-check injection**: When rule 7 or 8 routes to a `pulls/N-fix` session, the session's system prompt SHALL be enriched with the failing check's name, run URL, and (if retrievable) a short log summary, modeled after the PR Review Mode prompt injection for human comments.

#### Scenario: Failed check run on dev PR routes to implementer fix session
- **GIVEN** implementation PR #30 exists with no `spec-proposed`/`spec-approved`/`ralph`/`merging` labels
- **AND** a `check_run` for the `CI` workflow on PR #30's head SHA concludes with `failure`
- **WHEN** the `check_run.completed` event is processed
- **THEN** an implementer session is created with key `fjadmin/repo/pulls/30-fix`
- **AND** `IsFix` is true (write_file, git, bash tools available)
- **AND** the system prompt includes "CI check 'CI' failed. See <run URL>."
- **AND** the agent pushes fixes to the PR branch

#### Scenario: check_run with empty pull_requests field resolves via API
- **GIVEN** Forgejo emits `check_run.completed` for a failed run
- **AND** the payload's `pull_requests` field is empty
- **AND** the run's head SHA matches PR #42
- **THEN** the router resolves the PR via `GET /repos/{repo}/pulls?head={owner}:{branch}` or `GET /repos/{repo}/commits/{sha}/status`
- **AND** routing continues as Scenario "Failed check run on dev PR routes to implementer fix session"
- **AND** if no PR is resolved, the event is dropped with a debug log

#### Scenario: Successful check run does not route to implementer
- **GIVEN** a `check_run.completed` event with conclusion `success`
- **THEN** routing rule 7 does not match
- **AND** no implementer session is spawned
- **AND** (the gated automerge watcher may advance depending on review state; see Requirement: Gated Automerge)

#### Scenario: Failed check run on spec PR routes to PM, not implementer
- **GIVEN** PR #10 has label `spec-proposed`
- **AND** a CI check on its head SHA fails
- **WHEN** the `check_run.completed` event is processed
- **THEN** rule 7's precondition (no spec labels) is not satisfied
- **AND** the event is routed per rules 1-2 (PM)
- **OR** the event is dropped if no comment/review thread exists (CI failures on spec PRs are not auto-routed to implementer)

#### Scenario: djent-qa comment requesting changes routes to implementer fix session
- **GIVEN** normal implementation PR #30 has no spec/ralph labels
- **AND** `djent-qa` posts an issue comment with body "Please address the missing test for empty input."
- **AND** the body does NOT contain the `<!-- ford -->` self-marker
- **WHEN** the `issue_comment.created` event is processed
- **THEN** rule 4 routes to reviewer role at `pulls/N`
- **AND** the router detects the comment is from a `djent-*` sender with actionable body
- **AND** the manager's duplicate-routing branch sets `IsPRReviewFix=true`, elevating to implementer role
- **AND** the session pushes fixes to the PR branch

#### Scenario: Cost-summary comment from djent-pm on PR is dropped
- **GIVEN** PR #30 has a cost-summary comment posted by `djent-pm` with body "Session completed (implementation): 12K tokens, $0.00 USD\n\n<!-- ford -->"
- **WHEN** the `issue_comment.created` event is processed
- **THEN** `isAgentEvent` returns true (marker match)
- **AND** the event is dropped before routing
- **AND** no implementer session is spawned

#### Scenario: Multiple simultaneous triggers on same PR use one session
- **GIVEN** PR #30 receives, within seconds: a human review comment, a djent-qa review, and a failing `check_run`
- **WHEN** the events are processed
- **THEN** routing sends all three to session key `fjadmin/repo/pulls/30-fix`
- **AND** only one implementer session is created
- **AND** the system prompt is enriched from the most recent triggering event

### Requirement: Handoff Rules

Each role's output SHALL become the next role's input according to the following handoffs:

**PM → Scheduler (spec handoff)**
- PM creates `openspec/changes/<name>/{proposal.md,design.md,specs/,tasks.md}`
- PM commits to spec branch (non-yolo) or main (yolo)
- Non-yolo: PM creates spec PR with `spec-proposed` label
- Yolo: PM commits directly to main
- **Handoff artifact**: The `openspec/changes/<name>/` directory in git

**Scheduler → Implementer (task handoff)**
- On `spec-approved` (spec PR merged or yolo direct commit), scheduler reads `tasks.md`
- Scheduler creates one issue per unchecked task, with `[implementer]` tag
- Scheduler adds `Depends on:` labels/mentions per task dependencies
- **Handoff artifact**: Task issue bodies referencing the spec and task description

**Implementer → Build Gate / Ralph (implementation handoff)**
- Implementer works on task issue, creates PR
- On `forgejo_create_pr`, build gate runs (`go build`, `go test`, etc.)
- If PR passes build gate and session completes within turn budget → proceeds to reviewer
- If turn budget exhausted or AC unmet → Ralph harness activates (see Requirement: Ralph Place in Lifecycle)
- **Handoff artifact**: PR branch with code + `.ralph/progress/` (if Ralph)

**Ralph → djent-qa (Ralph completion handoff)**
- Ralph AC self-check records `ac_met: true`
- Ralph removes `ralph` label, adds `ralph-completed` label
- Scheduler queues reviewer session for PR
- **Handoff artifact**: PR with `ralph-completed` label + `.ralph/progress/*.md`

**djent-qa → Scheduler (review handoff)**
- djent-qa reads spec verification sections, checks implementation
- If approved and policy allows: djent-qa merges PR (or approves for human merge)
- If changes needed: djent-qa posts review comment → triggers `pulls/N-fix` implementer session
- djent-qa checks spec verification boxes on the PR branch (human-auditable record)
- **Handoff artifact**: Merged PR (triggers scheduler) or review comment (triggers fix session)

**Scheduler → PM (archival handoff)**
- On `PullRequestMerged` for a task PR, scheduler marks the task checkbox in `tasks.md`
- Scheduler checks if all task issues for this change are closed
- If all closed: scheduler dispatches `ArchiveChangeRequested` event (or creates a PM archival issue with `[pm]` tag)
- **Handoff artifact**: `ArchiveChangeRequested` event with change name

**PM → Archive (final handoff)**
- PM moves `openspec/changes/<name>/` to `openspec/changes/archive/<name>/`
- PM syncs delta specs if needed
- PM commits archive to branch (non-yolo PR) or main (yolo)
- **Handoff artifact**: Archived change directory + delta specs

#### Scenario: Full handoff chain for non-yolo change
- **GIVEN** repo without `fordjent-yolo` topic
- **WHEN** PM creates spec PR #10 for change "user-auth"
- **THEN** human merges PR #10 → lifecycle `spec-approved`
- **AND** scheduler reads `tasks.md`, creates issues #20, #21, #22
- **AND** implementers work, create PRs #25, #26, #27
- **AND** PR #25 goes to Ralph (hard), PRs #26 and #27 go straight to reviewers
- **AND** Ralph completes PR #25 → `ralph-completed` → reviewer merges
- **AND** all PRs merged → scheduler marks tasks done, detects all closed
- **AND** scheduler dispatches `ArchiveChangeRequested` for archival
- **AND** PM archives change "user-auth"

#### Scenario: Fix session handoff from review feedback
- **GIVEN** reviewer session on PR #30 finds issue: "Missing nil check"
- **WHEN** reviewer posts comment requesting changes
- **THEN** human feedback detection routes to `pulls/30-fix` implementer session
- **AND** implementer adds nil check, commits, pushes to PR branch
- **AND** implementer session completes
- **AND** PR #30 is now ready for re-review (reviewer session queued again)

---

### Requirement: PM Archival Trigger

PM archival SHALL fire only when the scheduler confirms that all task issues for a change are closed. The scheduler SHALL perform this check on two occasions:
1. When a `PullRequestMerged` event is processed for a task PR
2. When an `IssueClosed` event is processed for a task issue

Both conditions are natural consequences of the upstream chain: a task issue closes only when its PR merges; a PR merges only after djent-qa review (including Ralph PRs). Therefore, if any task is still in Ralph iteration, its PR is unmerged, its issue is open, and archival is naturally deferred without an explicit gate.

**Exception**: If a task issue is closed manually by a human without a merged PR (e.g., marked "won't do"), the scheduler SHALL still check if all task issues are closed and MAY trigger archival. The PM SHALL verify the change is actually complete before archiving.

#### Scenario: All tasks done triggers archival
- **GIVEN** change "user-auth" has task issues #20, #21, #22
- **AND** PRs for #20 and #21 have merged, their issues are closed
- **WHEN** PR for #22 merges, triggering `PullRequestMerged`
- **THEN** scheduler checks: all task issues for "user-auth" are closed
- **AND** scheduler dispatches `ArchiveChangeRequested` event for change "user-auth"
- **AND** PM archival session begins

#### Scenario: Partial completion does not trigger archival
- **GIVEN** change "user-auth" has task issues #20, #21, #22
- **AND** only PR for #20 has merged
- **WHEN** scheduler processes `PullRequestMerged` for #20
- **THEN** scheduler marks task #20 done in `tasks.md`
- **AND** scheduler detects issues #21 and #22 still open
- **AND** no PM archival is triggered

#### Scenario: Ralph PR prevents archival naturally
- **GIVEN** change "user-auth" has task issues #20, #21, #22
- **AND** PR for #20 is in Ralph iteration 3 (label `ralph` on PR)
- **AND** PRs for #21 and #22 have merged, issues closed
- **WHEN** scheduler checks after #22 merges
- **THEN** scheduler detects issue #20 still open
- **AND** no PM archival is triggered (Ralph PR unmerged)

---

### Requirement: tasks.md and spec.md Ownership

Artifact ownership and access rights SHALL be governed by the table below. Agents SHALL coordinate remaining-work state via `tasks.md` and open/closed issue state, and SHALL NOT use spec verification checkbox state as a coordination signal.

| Artifact | Owned By | Written By | Read By | Updated By |
|----------|----------|-----------|---------|-----------|
| `tasks.md` | PM | PM during spec creation | Implementer, Ralph, Scheduler | Scheduler marks checkboxes on task PR merge |
| `spec.md` (capability specs) | PM | PM during spec creation | Implementer, Ralph, djent-qa | Nobody after spec PR merges (immutable) |
| `spec.md` verification checkboxes | djent-qa | PM creates unchecked boxes | djent-qa reads during review | djent-qa checks boxes during review (on PR branch) |
| `.ralph/progress/*.md` | Ralph | Ralph during append stage | djent-qa reads during review | Ralph (each iteration overwrites) |

**Agent coordination rule**: Implementers and Ralph SHALL determine remaining work by reading `tasks.md` and checking open/closed issue state. They SHALL NOT use spec verification checkbox state for coordination. djent-qa SHALL check spec verification boxes as a human-auditable record of what was verified; these checkboxes are informational only.

**Spec immutability**: After the spec PR merges (stage `spec-approved`), `openspec/changes/<name>/specs/**/*.md` are immutable. Implementers, Ralph, and djent-qa may read them but SHALL NOT modify them. The only exception is djent-qa checking verification boxes on the implementation PR branch (which merges to main along with the implementation).

#### Scenario: Implementer reads tasks.md to know scope
- **GIVEN** scheduler created task issue #20 from `tasks.md` line 3
- **WHEN** implementer session begins
- **THEN** the implementer reads `openspec/changes/user-auth/tasks.md`
- **AND** the implementer reads the linked capability spec for requirements
- **AND** the implementer DOES NOT check any boxes in `tasks.md` or spec.md

#### Scenario: Ralph reads spec verification section for AC
- **GIVEN** Ralph iteration 2 for PR #25
- **WHEN** Ralph enters awareness phase
- **THEN** Ralph reads `openspec/changes/user-auth/specs/auth-core/spec.md`
- **AND** Ralph reads the `## Verification` section for acceptance criteria
- **AND** Ralph uses the criteria to guide implementation
- **AND** Ralph does NOT read checkbox state as "remaining work" signal

#### Scenario: djent-qa checks verification boxes during review
- **GIVEN** reviewer session on PR #25 after Ralph completion
- **WHEN** djent-qa verifies "`go test ./...` passes" is satisfied
- **THEN** djent-qa checks the corresponding box in `spec.md` on the PR branch
- **AND** the checked box is committed as part of the review (or left on branch to merge)
- **AND** this is a human-auditable record, not an agent coordination signal

#### Scenario: Scheduler updates tasks.md on merge
- **GIVEN** PR #25 (task #20) merges
- **WHEN** scheduler processes `PullRequestMerged`
- **THEN** scheduler finds the corresponding task line in `tasks.md`
- **AND** scheduler changes `- [ ]` to `- [x]` for that task
- **AND** scheduler commits the update to main

---

### Requirement: Ralph Place in Lifecycle

Ralph SHALL be treated as an **implementation harness extension**, not a lifecycle stage. Its sole purpose is to extend a single implementer's work on a hard PR when the bounded session turn budget is insufficient. It SHALL operate entirely within the `implementing` stage.

**Ralph activation**: Ralph is activated when an implementer session exhausts its turn budget or fails the build gate/AC check on PR creation. The harness adds the `ralph` label to the PR. The Ralph scheduler then spawns iterations via its ticker.

**Ralph session properties**:
- Session key: `repo/pulls/N-ralph-iM`
- Fresh context window each iteration
- Reads spec verification sections during awareness (NOT checkbox state)
- Cannot modify spec files (`openspec/` is immutable to Ralph)
- Progress recorded in `.ralph/progress/*.md`

**Ralph completion**: When Ralph's AC self-check passes (`ac_met: true`):
1. Ralph iteration status updated to `completed`
2. `ralph` label removed from PR
3. `ralph-completed` label added
4. Lifecycle transitions from `implementing` to `reviewing`
5. Scheduler queues a reviewer session

**Ralph does NOT**:
- Trigger PM archival
- Modify `tasks.md`
- Check spec verification boxes
- Create new PRs (it works on the existing PR branch)
- Transition lifecycle stages directly (it signals completion; the scheduler transitions)

**Ralph hard caps**: If `max_iterations_per_pr` or `max_cost_per_pr_usd` is exceeded, Ralph stops spawning iterations and records a failure reason. The lifecycle remains in `implementing`. The scheduler MAY re-queue a normal implementer issue for the remaining work, or a human MAY intervene.

#### Scenario: Ralph as harness within implementing stage
- **GIVEN** task issue #20 is in `implementing` stage
- **AND** implementer creates PR #25 but exhausts turn budget
- **WHEN** Ralph harness activates with `ralph` label
- **THEN** Ralph spawns iterations: `fjadmin/testbed/pulls/25-ralph-i1`, `i2`, etc.
- **AND** all iterations share the `implementing` stage
- **AND** the lifecycle does not move forward until Ralph completes

#### Scenario: Ralph completion signals stage transition
- **GIVEN** Ralph iteration 5 records `ac_met: true`
- **WHEN** the iteration's append stage commits and pushes
- **THEN** `ralph` label is removed, `ralph-completed` is added
- **AND** the scheduler transitions the PR from `implementing` to `reviewing`
- **AND** a reviewer session is queued with key `pulls/25`

#### Scenario: Normal implementer does not use Ralph
- **GIVEN** task issue #21 is in `implementing` stage
- **AND** implementer creates PR #26 on first try
- **AND** build gate passes, session completes within budget
- **THEN** PR #26 has no `ralph` label
- **AND** lifecycle transitions directly from `implementing` to `reviewing`
- **AND** Ralph was never involved

#### Scenario: Ralph cap exceeded prevents completion
- **GIVEN** `max_iterations_per_pr` is 5
- **AND** Ralph iteration 5 completes with `ac_met: false`
- **WHEN** scheduler checks before spawning iteration 6
- **THEN** iteration 6 is blocked with reason `"ralph-iteration-limit-exceeded"`
- **AND** `ralph` label remains on PR #25
- **AND** lifecycle remains in `implementing`
- **AND** scheduler MAY create a new implementer issue for the remaining work

### Requirement: Gated Automerge

When the `automerge` label is present on a PR, the manager SHALL NOT immediately merge. The manager SHALL attempt to merge only when ALL of the following hold at evaluation time:

1. The most recent review from `djent-qa` (if any) has state `approved`, OR no `djent-qa` review exists yet AND no `changes_requested` label is present, AND
2. Every `check_run` on the PR's head SHA has conclusion `success`, AND
3. The PR is `mergeable` with no conflicts.

Evaluation is event-driven: the manager re-evaluates on each `check_run.completed` event, each `issue_comment.created` event, and each `pull_request_review` event for the PR. There SHALL be no timer-based busy-polling.

When conditions are met, the manager attempts to merge using the existing multi-style merge loop (manager.go merge-styles). On success, the `automerge` label is removed. On any unmet precondition, the manager SHALL NOT merge and SHALL log at debug level.

If merged, the existing scheduler + lifecycle post-merge hooks fire (scheduler reads tasks.md, lifecycle transitions).

#### Scenario: Automerge fires after CI green and QA approval
- **GIVEN** PR #30 has the `automerge` label
- **AND** all `check_run`s have conclusion `success`
- **AND** the most recent `djent-qa` review has state `approved`
- **AND** the PR is `mergeable`
- **WHEN** the `pull_request_review` (state=approved) event arrives
- **THEN** the manager attempts to merge `pulls/N` (multi-style fallback: merge, squash, rebase-merge)
- **AND** on success, removes the `automerge` label

#### Scenario: Automerge waits for pending check run
- **GIVEN** PR #30 has `automerge` label
- **AND** the most recent `djent-qa` review is `approved`
- **AND** one `check_run` has conclusion `pending` (in_progress)
- **WHEN** a `check_run.completed` event arrives
- **THEN** the manager inspects all check runs on the head SHA
- **AND** does NOT merge
- **AND** logs at debug level
- **AND** re-evaluates on the next qualifying event

#### Scenario: Automerge blocked by failing check_run
- **GIVEN** PR #30 has `automerge` label
- **AND** a check run on the head SHA has conclusion `failure`
- **THEN** the manager does NOT merge
- **AND** the routing table (rule 7) has routed the implementer to `pulls/N-fix` to address the failure
- **AND** after the implementer pushes a fix, the next CI run re-evaluates the gate

#### Scenario: Automerge blocked by changes_requested label
- **GIVEN** PR #30 has `automerge` label
- **AND** the PR has the `changes_requested` label (djent-qa posted a `changes_requested` review)
- **THEN** the manager does NOT merge
- **AND** the routing table (rule 3) has already routed the implementer to `pulls/N-fix`
- **AND** the label is cleared by the reviewer on the next approval cycle (see Requirement: Reviewer Submits Formal Review)

#### Scenario: Non-yolo repo — no djent-qa review exists yet
- **GIVEN** PR #30 is on a repo without `fordjent-yolo` topic
- **AND** PR has `automerge` label
- **AND** all check runs are `success`
- **AND** no `djent-qa` review exists
- **THEN** the manager does NOT merge (waits for the human review or an explicit `approved` review)
- **AND** a human reviewer can post a Forgejo review (state=approved) to release the gate

### Requirement: Reviewer Submits Formal Review

A new `forgejo_submit_review` tool SHALL be available to the reviewer (`djent-qa`) role. The tool accepts `repository`, `pr_number`, `state` (one of `approved`, `changes_requested`, `commented`), and `body`. The tool:

- Calls `POST /repos/{repo}/pulls/{N}/reviews` with `{ "event": state, "body": body }`.
- If `state=approved`, the tool SHALL remove the `changes_requested` label from the PR if it is present (the PR is now approved; downstream gated automerge will fire on the next qualifying event).
- If `state=changes_requested`, the tool SHALL add the `changes_requested` label to the PR (so routing rule 3 routes the implementer to `pulls/N-fix`).
- If `state=commented`, the tool SHALL NOT touch labels (comments do not change routing state).
- The tool SHALL NOT call `forgejo_merge_pr` — merging is the gated automerge watcher's responsibility, not the reviewer's.

The reviewer's system prompt SHALL prefer `forgejo_submit_review` over `forgejo_comment` when the review verdict is decisive (approve / request changes). Bare comments are still permitted for non-decisive notes.

#### Scenario: Reviewer approves a passing PR
- **GIVEN** `djent-qa` reviews PR #30 and finds it correct
- **WHEN** the reviewer calls `forgejo_submit_review("repo", 30, "approved", "LGTM — tests cover edge cases.")`
- **THEN** a Forgejo review with event `approved` is posted on PR #30
- **AND** if `changes_requested` label was present, it is removed
- **AND** the gated automerge watcher conditions are evaluated on the resulting `pull_request_review` event

#### Scenario: Reviewer requests changes
- **GIVEN** `djent-qa` reviews PR #30 and finds issues
- **WHEN** the reviewer calls `forgejo_submit_review("repo", 30, "changes_requested", "Add a test for empty input.")`
- **THEN** a Forgejo review with event `changes_requested` is posted on PR #30
- **AND** the `changes_requested` label is added to PR #30
- **AND** the `issue_comment.created` webhook for the review's comment body is processed by routing rule 3
- **AND** an implementer session is spawned at `pulls/N-fix`

#### Scenario: Reviewer leaves a comment without verdict
- **GIVEN** `djent-qa` wants to leave a clarifying question
- **WHEN** the reviewer calls `forgejo_submit_review("repo", 30, "commented", "Is this method thread-safe?")`
- **THEN** a Forgejo review with event `commented` is posted
- **AND** no labels are touched
- **AND** no implementer session is spawned (commented state is not actionable for routing)

### Requirement: Yolo Repos Auto-Spawn Reviewer on PR Open

When a `pull_request.opened` event arrives for a repo with the `fordjent-yolo` topic AND the PR author is `djent-dev` (implementer, including the case when the PR was created via `forgejo_create_pr`), the manager SHALL emit an internal `ReviewRequested` event that spawns a `djent-qa` reviewer session keyed `pulls/N`. The reviewer then either approves (gated automerge fires) or requests changes (implementer fix session fires).

This replaces the previous behavior where `fordjent-yolo` repos auto-merged the instant the `automerge` label was applied.

#### Scenario: New dev PR in yolo repo spawns reviewer
- **GIVEN** repo `fjadmin/testbed` has the `fordjent-yolo` topic
- **WHEN** `djent-dev` opens PR #30
- **THEN** the manager emits `ReviewRequested(repo, pr=30)`
- **AND** a `djent-qa` reviewer session is created with key `fjadmin/testbed/pulls/30`
- **AND** the reviewer inspects the diff, runs read-only checks, and either approves or requests changes

#### Scenario: Non-yolo repo — no auto-spawn
- **GIVEN** repo `fjadmin/serious` does NOT have the `fordjent-yolo` topic
- **WHEN** `djent-dev` opens PR #30
- **THEN** no `ReviewRequested` event is emitted
- **AND** the human is responsible for review (the gated automerge waits for a human `approved` review)

### Requirement: Rework Attempts Counter

The system SHALL maintain a per-PR rework counter in `lifecycle.db` (`pr_rework` table keyed by `repo|pr_number`, columns `attempts INTEGER`, `last_attempt_at TIMESTAMP`). The counter is incremented each time an implementer session is spawned at `pulls/N-fix` for the PR (whether from a failed CI check, a `changes_requested` review, or a human comment).

The system SHALL refuse to spawn further `pulls/N-fix` sessions for a PR once `attempts` reaches `max_rework_attempts` (default 3, configurable). When the cap is reached, the manager SHALL:

- Add the `fordjent/failed:rework-exhausted` label and the `blocked` label to the PR.
- Post a comment: "Max rework attempts reached. Please review this PR manually."
- NOT spawn the implementer session for this event (the event is dropped after labeling).

Resetting the counter is a manual operation: delete the row from `pr_rework` or call (proposed) `lifecycle.ResetRework(repo, pr)`.

#### Scenario: First two rework attempts spawn fix sessions
- **GIVEN** PR #30's `pr_rework.attempts` is 1
- **WHEN** a second `check_run.failed` event arrives
- **THEN** the counter increments to 2
- **AND** an implementer session is spawned at `pulls/N-fix`

#### Scenario: Third attempt hits the cap
- **GIVEN** PR #30's `pr_rework.attempts` is `max_rework_attempts` (3)
- **WHEN** a fourth `check_run.failed` event arrives
- **THEN** the manager does NOT spawn an implementer session
- **AND** the manager adds `fordjent/failed:rework-exhausted` and `blocked` labels to PR #30
- **AND** the manager posts a "Max rework attempts reached" comment
- **AND** the manager increments the counter (for observability) to 4

#### Scenario: Counter not incremented for reusing existing session
- **GIVEN** a `pulls/N-fix` implementer session is already active for PR #30
- **WHEN** another `check_run.failed` event arrives for the same head SHA
- **THEN** the existing session is reused (kicked), not a new session spawned
- **AND** the counter is NOT incremented
- **AND** the existing session sees the new failure context

### Requirement: Webhook Subscription Includes CI Events

The repo-webhook registration in `scripts/bootstrap-local.sh` and `scripts/cloud-bootstrap.sh` SHALL subscribe to `check_run` and `workflow_run` events in addition to the existing `issues`, `issue_comment`, `pull_request`, `pull_request_review_comment`. The `status` event SHALL NOT be subscribed (see Design — Decision 1).

System-level hooks (already configured on cloud Forgejo) SHALL inherit the same subscription list.

#### Scenario: bootstrap-local.sh registers CI events on repo webhook
- **WHEN** `bootstrap-local.sh` creates the repo webhook for `fjadmin/testbed`
- **THEN** the webhook payload's `events` field contains `issues`, `issue_comment`, `pull_request`, `pull_request_review_comment`, `check_run`, `workflow_run`
- **AND** does NOT contain `status`

