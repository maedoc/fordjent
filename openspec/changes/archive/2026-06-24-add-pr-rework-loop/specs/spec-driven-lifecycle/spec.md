## MODIFIED Requirements

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

## ADDED Requirements

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
