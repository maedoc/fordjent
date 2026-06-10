# Spec-Driven Lifecycle

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

## ADDED Requirements

### Requirement: Lifecycle Stage Machine

A change progresses through the following stages. Each stage transition is unidirectional and is triggered by a specific event or action defined in the Handoff Rules.

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
| 3 | `issue_comment.created` | PR labels contain `ralph` | `pulls/N-ralph-iM` | Implementer (Ralph) | Next Ralph iteration |
| 4 | `issue_comment.created` | PR labels contain `ralph-completed` | `pulls/N` | Reviewer (djent-qa) | Post-Ralph review |
| 5 | `pull_request_review_comment.created` | PR labels contain `changes_requested` or body contains actionable directive ("fix", "rename", "add") | `pulls/N-fix` | Implementer | Reviewer requested changes; implementer fix session |
| 6 | `issue_comment.created` | Sender is human, `issue.is_pull_request` is true, PR has no spec/ralph labels | `pulls/N` | Reviewer (djent-qa) | Normal PR review comment |
| 7 | `pull_request_review_comment.created` | PR labels none of above | `pulls/N` | Reviewer (djent-qa) | Normal PR review thread |
| 8 | `pull_request.merged` | Any | `pulls/N` | Scheduler | Scheduler processes task completion, checks all tasks |
| 9 | `issue.closed` | Issue is a task issue (linked to change) | `issues/N` | Scheduler | Scheduler checks if all tasks for change are done |
| 10 | (derived) | All task issues for a change are closed | `issues/<parent>` | PM | PM archival triggered by scheduler event |

**Routing table sovereignty**: No other component creates sessions independently. All Fordjent session creation SHALL flow through this routing table. |

**Label priority**: If a PR has multiple labels (e.g. both `spec-proposed` and `ralph`), the higher-priority rule wins. This is a configuration error; the system SHALL log a warning.

#### Scenario: Human comment on spec PR routes to PM
- **GIVEN** spec PR #10 has label `spec-proposed`
- **WHEN** a human posts comment "Use bcrypt not scrypt"
- **THEN** a PM session is created with key `fjadmin/testbed/pulls/10`
- **AND** the PM updates spec files and pushes

#### Scenario: Human comment on Ralph PR routes to next Ralph iteration
- **GIVEN** implementation PR #25 has label `ralph`
- **WHEN** a human posts comment "Still missing edge case test"
- **THEN** the next Ralph iteration is spawned: session key `fjadmin/testbed/pulls/25-ralph-iM`
- **AND** the Ralph agent reads the comment during awareness phase

#### Scenario: Ralph-completed PR routes to reviewer
- **GIVEN** PR #25 has label `ralph-completed` (and no `ralph` label)
- **WHEN** the scheduler queues review (or a human comments)
- **THEN** a reviewer session is created with key `fjadmin/testbed/pulls/25`
- **AND** the reviewer reads `.ralph/progress/*.md` and the spec

#### Scenario: Reviewer requests changes on normal PR routes to implementer fix session
- **GIVEN** normal implementation PR #30 has no spec/ralph labels
- **AND** reviewer posted a review comment with state `changes_requested`
- **WHEN** the webhook event is processed
- **THEN** an implementer session is created with key `fjadmin/testbed/pulls/30-fix`
- **AND** the agent has write_file, git, and bash tools available
- **AND** the agent pushes changes to the PR branch

---

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

Ralph is an **implementation harness extension**, not a lifecycle stage. Its sole purpose is to extend a single implementer's work on a hard PR when the bounded session turn budget is insufficient. It operates entirely within the `implementing` stage.

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
