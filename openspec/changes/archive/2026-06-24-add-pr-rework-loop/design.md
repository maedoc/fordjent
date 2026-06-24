# Design — Add PR Rework Loop

## Context

This change closes the loop on three behaviors the user's mental model assumes but the code does not yet provide:

1. **CI events drive dev rework** — when a Forgejo Actions run (or any `check_run`) fails on a dev PR, the dev should be reactivated to fix the failure.
2. **QA feedback drives dev rework** — `djent-qa` review comments requesting changes must reach the implementer, not be filtered as agent noise.
3. **Merge is gated on CI green + QA approval** — automerge must not fire on label-appearance alone; it must observe CI success and reviewer approval before merging.

The good news: the `pulls/N-fix` implementer rework loop already exists (Bug 39 / Bug 41 built it for human PR comments). It uses the implementer role, checks out the PR head branch, injects the comment body into the system prompt, and gives the agent write tools. Extending what triggers that loop is the bulk of the work.

## Decisions

### Decision 1: Subscribing to `check_run`, `workflow_run` (not `status`)

Forgejo emits three candidate events for CI status:

- **`status`** — fires for every ref update's commit status. Coarse; fires for every push to any branch, including main. Will wake the manager on every bot commit. Verbose.
- **`check_run`** — fires per GitHub-Actions-style check. Carries the PR number directly via `pull_requests` field (ideally). Forgejo/Gitea populate this when the workflow run is attributable to a PR.
- **`workflow_run`** — fires when a workflow completes. Carries the workflow's head SHA; the PR must be resolved by querying the API for the PR matching that SHA.

**Choice**: subscribe to `check_run` and `workflow_run`. Skip `status` — too coarse, overlaps with `push` for our needs, would generate event storms during PRs that push multiple commits.

Fallback chain in the parser: if `check_run.pull_requests` is populated, use it directly. If empty (Forgejo sometimes doesn't fill it), resolve the PR by head SHA via `GET /repos/{owner}/{repo}/commits/{sha}/statuses` or `GET /repos/{owner}/{repo}/pulls?head=...`. If no PR can be resolved, drop the event (the check is for a direct-to-main commit, not a PR we'd rework).

### Decision 2: Routing rule — where failed CI lands

Rule 3 of the existing routing table already routes `pull_request_review_comment` with `changes_requested` label to the implementer at `pulls/N-fix`. We add an analogous rule for failed `check_run`:

- **Pre-condition** for routing to implementer: PR labels do NOT include `spec-proposed`/`spec-approved` (those route to PM) and do NOT include `merging` (already past review).
- **Action**: route to implementer at `pulls/N-fix`, `IsFix=true`. Inject the failing check name + URL into the session context (like the human-comment injection in PR Review Mode).
- **Self-loop guard**: if the implementer pushes a fix that triggers another failing `check_run`, the routing table fires again — but the new `pulls/N-fix` session reuses the same session key, which is the *existing* implementer session. That's correct: the dev is woken up to look at the new failure.

### Decision 3: QA feedback — stop filtering djent senders on PR comments

The two duplicate-routing predicates at manager.go:1002 and :1092 currently exclude senders containing `fordjent` OR `djent`. The RouteTable (router.go:117) does NOT exclude them — but the duplicate branch in `getOrCreateSession` is what actually sets `IsPRReviewFix=true`, and it gates on human senders.

**Choice**: change the predicate from "exclude fordjent/djent senders" to "exclude events whose comment body contains the `<!-- ford -->` self-marker" (which is already how cost-summary comments are tagged). Cost-summary comments from `djent-pm`/`djent-dev` etc. carry the marker and will be dropped. Review comments from `djent-qa` whose body contains actionable prose ("this is missing X", "please address Y") will pass through to the implementer session.

This reuses the existing Agent-Self-Loop Suppression (per AGENTS.md Bug 5/6) rather than inventing a new filter.

### Decision 4: `changes_requested` label is the routing-table signal

`djent-qa`'s review-triggered dev rework can take one of two shapes:

- **A.** Bare comment on the PR (no review state).
- **B.** A formal Forgejo review with `state=changes_requested` (which also posts a comment).

Choice B is more structured and Forgejo already models it. We add a `forgejo_submit_review` tool that posts a Forgejo review, AND simultaneously applies a `changes_requested` PR label. The routing table keys off the **label**, not the review state — because Forgejo's webhook payloads for reviews vary by version, but labels are stable and spend-time-cheap to check.

When the dev pushes a next commit (or `djent-qa` approves on a later pass), the `changes_requested` label must be removed. Two acceptable owners for that removal:

- The implementer's `forgejo_create_pr` no-op path (it already queries labels) — but this is awkward since the PR already exists.
- A new tiny step in the reviewer session: on `approved`, remove `changes_requested` and merge.

**Choice**: reviewer-owns-removal. When `djent-qa` approves, the reviewer session calls `forgejo_submit_review` with `state=approved`, which removes `changes_requested` (if present), and the gated automerge then fires. The implementer never has to think about label hygiene.

### Decision 5: Gated automerge instead of label-triggered automerge

Today (manager.go:770): `automerge` label appears on the PR → manager immediately calls `MergePR` with cycled styles.

**Choice**: when the `automerge` label is detected, do NOT merge. Instead:

- Mark "automerge requested" in the PR state (in memory — no DB needed, only one watcher).
- Poll (single attempt on label-detect, then on subsequent qualifying events) the PR's check runs and reviews:
  - all check runs have conclusion `success` **AND** the most recent djent-qa review is `approved` (if a review exists) **AND** PR is `mergeable` → attempt merge, clear the automerge label, log.
  - if any check run is still `pending` or `in_progress` → do nothing yet (next `check_run.completed` will re-evaluate; no busy-polling).
  - if any check run is `failure` → do nothing (the failed-check rule has already routed the dev to a `_fix` session; merge stays blocked).
  - if the most recent djent-qa review is `changes_requested` → do nothing (implementer fix session is already in flight via label routing).
  - if no review exists yet → wait. In yolo repos this is part of Decision 6; in non-yolo, the human is the reviewer.

This makes automerge *eventually consistent* with CI and QA — driven by incoming events, not by a timer.

### Decision 6: Yolo repos auto-spawn a reviewer session on PR open

In yolo repos, the dev merges immediately (current behavior). With the Decision 5 gate, we still need a qa reviewer to approve — otherwise the PR waits forever.

**Choice**: emit an internal `ReviewRequested` event when a new dev PR opens in a yolo repo. This spawns a `djent-qa` session keyed `pulls/<N>` (reviewer role). The reviewer reads the diff, runs build/test in a read-only way, then either:

- **Approves** (`forgejo_submit_review(state=approved)`) → clears `changes_requested` if present → Decision 5's gate will now fire on the next qualifying event.
- **Requests changes** (`forgejo_submit_review(state=changes_requested)`) + adds `changes_requested` label → Rule 3 routes to `pulls/N-fix` implementer session → dev fixes → pushes → re-runs CI → CI green → reviewer re-spawned or qa manual approve.

Ping-pong is bounded by Decision 7.

### Decision 7: rework_attempts counter, hard cap

Without a cap, a dev could keep making commits that fail CI, the reviewer keeps requesting changes, indefinitely. Add a per-PR `rework_attempts` counter (incremented on each `pulls/N-fix` session start).

- Config: `max_rework_attempts: 3` (default).
- When `rework_attempts >= max_rework_attempts`, stop auto-spawning `_fix` sessions for that PR.
- Outcome: add `fordjent/failed:rework-exhausted` label + `blocked` label, post a comment ("Max rework attempts reached; please review manually."), and stop the loop.

Counter is persisted in `lifecycle.db` — a new `pr_rework` table keyed by `repo|pr_number`, columns `attempts INTEGER`, `last_attempt_at TIMESTAMP`. Easy to inspect, easy to reset manually (delete the row).

## Alternatives Considered

### Alternative A: Subscribe to `push` for CI instead of `check_run`

Push-then-fetch-checks. Polling on every push. Generates many more events; doesn't fire on push-then-check-runs-later. **Rejected** — too coarse, coarse on failure attribution.

### Alternative B: Polling-based automerge

Timer fires every N seconds, queries every open PR with `automerge` label for CI/review state. Simpler logic, but it's busy waiting — generates API load against Forgejo, scales poorly with PR count. **Rejected** in favor of event-driven merge.

### Alternative C: Don't introduce a formal review concept; route djent-qa comments by content

Parse review comments for keywords like "fix", "rename", "rename", "address". This is `isActionableReview`'s existing approach and is fragile (reviewer might write a polite prose comment with no keyword). **Rejected** in favor of structured `forgejo_submit_review(state=...)` + `changes_requested` label — declarative and unambiguous.

### Alternative D: Unify the two routing entrances (RouteTable + manager duplicate)

Today both `RouteTable.Route` and the inline predicates in `getOrCreateSession` set routing decisions. The kludgy duplication is the actual root cause of Bug 2 (RouteTable has no djent-filter; manager does). **Considered but deferred**: full unification is a larger refactor that interferes with several existing tests. The targeted fix (stop filtering djent senders in the duplicate branch) addresses the bug without the larger churn. A unification change can follow later.

## Risks

- **Forgejo version skew**: `check_run.pull_requests` is not reliably populated across Forgejo/Gitea versions. Mitigation: head-SHA PR resolution fallback; if both fail, drop the event (with a debug log) — gracefully degrades to current behavior.
- **Event storm**: a dev PR with a slow CI that re-runs N times could fire many `check_run` events. Mitigation: the routing table reuses the existing `pulls/N-fix` session key, so only one implementer session exists per PR; new events just kick an inactive session.
- **Two implementer sessions per PR**: a human could comment, a reviewer (`djent-qa`) could submit-review, AND a check could fail, all near-simultaneously. The session key is the same (`pulls/N-fix`), so only one session is created — the prompt is enriched from the most recent triggering event.
- **Rework cap masking testable bugs**: a PR with persistent failures will be labeled `rework-exhausted` before a developer can debug it. Mitigation: this is operator-visible (label + comment + dashboard), and the cap is configurable higher if teams want more attempts.
