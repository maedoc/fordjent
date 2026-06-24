## Why

The user's mental model — "file/discuss on issue → PM spec → dev implements → CI runs per commit → dev fixes CI failures → QA reviews → dev iterates on QA feedback → merge" — collides with current behavior on three concrete points. A live audit of the code confirmed each gap precisely:

1. **CI events arrive and get dropped.** The repo webhook (per `scripts/bootstrap-local.sh:483`) is subscribed to only `issues`, `issue_comment`, `pull_request`, `pull_request_review_comment`. The router's `switch eventType` (router.go:1142-1154) handles those five plus `push`; every other event type — including Forgejo's `status`, `check_run`, `check_suite`, `workflow_run` — hits the default case and is rejected as `unsupported event type`. So today, when CI fails on a dev PR, the implementer session never wakes up. The only "CI" the dev runs is its own pre-PR build/test gate inside `forgejo_create_pr` (in the fordjent container) — which is not the real pipeline.

2. **`djent-qa` feedback is silently dropped.** Rule 3 of the Event-to-Role Routing Table (router.go:117-126) already routes `pull_request_review_comment` with the `changes_requested` label to a `pulls/N-fix` implementer session — but in the live path, `Manager.getOrCreateSession` ALSO applies its own duplicate routing (manager.go:1002 and 1092) keyed off human-only senders. Any sender containing `fordjent` or `djent` is excluded from the `IsPRReviewFix = true` path, which means every review comment `djent-qa` posts (the bot whose whole job is to review) is treated as agent noise and routed nowhere — or to a reviewer session with no write tools. QA approval/disapproval cannot loop back to dev.

3. **Automerge fires pre-CI, pre-QA.** Manager.go:770 the instant the `automerge` label appears on a PR — and `forgejo_create_pr` adds that label on every PR open — the manager attempts a direct API merge. There is no CI gate, no QA gate, no rework-flattening. In yolo mode this means a dev PR merges before the pipeline runs, before QA reviews, and before any CI failure could drive another dev commit.

The unifying observation: **the `pulls/N-fix` rework loop already exists** (Bug 39/41 built it for human comments — implementer role, PR branch checkout, write tools available, feedback injected into prompt). The work needed is "extend the set of triggers that feed that loop, and gate automerge behind rework clear." Nothing here is a new subsystem; it is wiring events into an existing primitive.

## What Changes

- **Subscribe to CI events.** Register `status`, `check_run` (and optionally `workflow_run`) in the repo webhook. In `bootstrap-local.sh` and `cloud-bootstrap.sh`, extend the `events` array.
- **Parse CI events into a normalized event.** Add `internal/event` types `CheckRunCompleted` and `WorkflowRunCompleted` (carrying PR number, run name, success/failure status, run URL). Router normalizes Forgejo's `check_run` payload (which may carry a PR number via `check_run.pull_requests`) and `workflow_run` payload (resolves PR by head SHA via the Forgejo API).
- **Route a failed check run on a dev PR to the implementer.** New routing rule: on `check_run.completed` with conclusion `failure`/`cancelled`/`action_required` on a PR with no `spec-proposed`/`spec-approved`/`ralph`/`merging` labels → route to implementer at session key `pulls/N-fix`, IsFix=true. Inject the failing check name + URL + log-summary into the system prompt so the dev session knows what to fix.
- **Stop filtering djent-qa as agent noise on PR comments.** In the duplicate routing path in `manager.go` (lines 1002 and 1092), stop excluding `djent-*` senders from the reviewer-fix path. Keep the existing `<!-- ford -->` marker filter for self-comments. Acceptable refinement: still drop bare summary-only comments (those that contain only the marker and no actionable content) to avoid reviving dev sessions for cost-tracking noise.
- **Gate automerge behind "CI green + reviewer neutral/positive."** Change the automerge label watcher in `manager.go:770`: when the `automerge` label is detected, don't merge immediately. Instead, query the PR's check runs and review state via the Forgejo API. Merge only when:
  - all `check_run`s have conclusion `success`, AND
  - the most recent review from `djent-qa` (if any) is `approved` (or no review exists yet, in which case the PR waits for review — see next bullet), AND
  - the PR is `mergeable` with no conflicts.
- **Auto-spawn a reviewer session on PR open (yolo repos only).** Today yolo repos auto-merge the instant the label is set. Replace that with: emit an internal dispatch event `ReviewRequested` for the PR. This spawns a `djent-qa` reviewer session. The reviewer reads the diff, posts an approval review (state `approved`, no label needed) OR posts a `changes_requested` review with a `changes_requested` PR label. The automerge watcher above then acts on whichever the reviewer posted.
- **`changes_requested` PR label is controlled, not server-set.** When `djent-qa` posts a review with `state=changes_requested`, the reviewer (via a new `forgejo_submit_review` tool — wrapping Forgejo's `POST /repos/{repo}/pulls/{N}/reviews`) ALSO adds the `changes_requested` label to the PR. That label is what Rule 3 keys on. When the dev pushes the next commit, the dev session can clear the label (or the reviewer clears it on next approval cycle). This keeps the routing-table-driven approach intact.
- **Rework counter (per-PR).** Add a `rework_attempts` counter per PR (`lifecycle.db` `pr_rework` table or extend existing `blocked_branches`). Cap at `max_rework_attempts` (default 3). After the cap, label the PR `fordjent/failed:rework-exhausted` and stop auto-spawning dev sessions; fall back to a human.

## Capabilities

### New Capabilities

_None_ — all changes reuse the existing `pulls/N-fix` primitive and existing role definitions.

### Modified Capabilities

- **`spec-driven-lifecycle`**: routing table gains rules for `check_run.completed` (failed check → implementer fix on dev PR); the automerge decision moves from "label-appearance-triggered" to "CI-green + review-aware gated merge"; the `djent-qa` review handoff becomes an auto-spawned reviewer session in yolo repos; the `changes_requested` label becomes the routing-table signal (instead of an after-the-fact comment filter); a bounded `rework_attempts` counter caps the dev↔QA ping-pong.

## Impact

- **`scripts/bootstrap-local.sh`, `scripts/cloud-bootstrap.sh`** — extend webhook `events` array with `check_run`, `workflow_run` (optionally `status`).
- **`internal/event/event.go`** — add `CheckRunCompleted`, `WorkflowRunCompleted`, `ReviewRequested` event types with carrier fields.
- **`internal/webhook/router.go`** — parse `check_run` and `workflow_run` payloads into the normalized events; resolve PR from head SHA when the payload doesn't carry `pull_requests`; extend `switch eventType` to handle them.
- **`internal/webhook/router.go` (`RouteTable.Route`)** — add a rule routing failed check runs on dev PRs to `pulls/N-fix` with `IsFix=true`.
- **`internal/session/manager.go`** — (a) stop excluding `djent-*` senders from the `IsPRReviewFix` duplicate-routing branch (lines 1002, 1092); (b) replace direct merge on `automerge` label with a gated merge that queries check runs + reviews; (c) auto-dispatch `ReviewRequested` in yolo repos when a new dev PR opens.
- **`internal/forgejo/client.go`** — add `ListCheckRuns(repo, ref)`, `GetCheckRun(repo, id)`, `ListPRReviews(repo, pr)`, `SubmitReview(repo, pr, state, body)` API methods.
- **`internal/tool/forgejo_tools.go`** — new `forgejo_submit_review` tool (reviewer role) that submits a Forgejo review (`approved` / `changes_requested`) and toggles the `changes_requested` label on the PR accordingly.
- **`internal/lifecycle/lifecycle.go`** — extend per-PR rework tracking (new `pr_rework` table or column on existing table); enforce `max_rework_attempts`.
- **`internal/config/config.go`** — `max_rework_attempts: 3` default; nothing removed.
- **`fordjent.local.yaml`** — document `max_rework_attempts`; nothing else changes (CI events are filtered by routing rule, not config).
- No breaking API changes; the routing table absorbs the new event types as additional matching rules. Existing tests for the routing table and manager pass after updating the two duplicate-routing predicates.
