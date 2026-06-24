## 1. Webhook subscription

- [x] 1.1 In `scripts/bootstrap-local.sh:483`, extend the `events` array with `"check_run"`, `"workflow_run"`. Do NOT add `"status"`.
- [x] 1.2 In `scripts/cloud-bootstrap.sh` (and any equivalent snippet in `deploy/`), apply the same extension.
- [x] 1.3 Add a checklist note in `scripts/RUNBOOK-local-docker.md` documenting that CI events must be subscribed for the rework loop.

## 2. Event types

- [x] 2.1 In `internal/event/event.go`, add `CheckRunCompleted`, `WorkflowRunCompleted`, and `ReviewRequested` event types.
- [x] 2.2 Add carrier fields on the `Event` struct: `CheckName`, `CheckConclusion`, `CheckURL`, `HeadSHA` (already present? if so reuse).
- [x] 2.3 Add `PullRequestReview` event type with `ReviewState` (`approved`/`changes_requested`/`commented`) — fired when a `pull_request_review` webhook arrives.

## 3. Router parsing of CI events

- [x] 3.1 Extend the `switch eventType` in `internal/webhook/router.go:1142-1154` to accept `check_run` and `workflow_run`.
- [x] 3.2 For `check_run`: fire `CheckRunCompleted` with `CheckName`, `CheckConclusion`, `HeadSHA`, `CheckURL`. If `pull_requests` is populated, set `PRNumber` directly. Otherwise resolve via Forgejo API `GET /repos/{repo}/pulls?head={owner}:{branch}` (preferred) or `GET /repos/{repo}/commits/{sha}/statuses`. If no PR resolved, drop with debug log.
- [x] 3.3 For `workflow_run`: fire `WorkflowRunCompleted` similarly. Resolve PR by head SHA via the same API call.
- [x] 3.4 For `pull_request_review`: parse `review.state` into `ReviewState` and set `evt.PRNumber` from `pull_request.number`. Skip processing if state is `dismissed`.

## 4. Routing table rules for CI failures

- [x] 4.1 In `RouteTable.Route` (router.go:86), add rules 7 and 8 per the spec: failed `check_run.completed`/`workflow_run.completed` on a PR with no spec/ralph/merging labels → `pulls/N-fix` with `IsFix=true`.
- [x] 4.2 Ensure rules 1-3 take priority: if a PR has `spec-proposed`/`spec-approved`, the failed-check event does NOT route to implementer (drop, or route to PM via existing rules, depending on event type — for `check_run` there is no spec-route equivalent, so drop).
- [x] 4.3 In `RouteTable.fetchPRLabels`, ensure the labels for the resolved PR are fetched (PR-by-SHA path may need a `forgejo.GetPR` lookup to populate labels).
- [x] 4.4 Add tests in `internal/webhook/router_test.go`: failed-check routes to `pulls/N-fix`; successful-check does not route; check on spec PR is dropped; check on PR-with-`merging` label does not route.

## 5. Failed-check prompt injection

- [x] 5.1 When routing rule 7/8 fires (or the equivalent manager.go path), inject "CI check '<name>' failed. See <URL>." into the implementer session's system prompt, modeled after the PR Review Mode injection.
- [x] 5.2 Optionally fetch a short log summary via `GetCheckRun`/`GetJobLog` if Forgejo exposes an endpoint; otherwise just the run URL.
- [x] 5.3 Add a test verifying the prompt is enriched when the trigger was a `CheckRunCompleted` event.

## 6. Stop filtering djent-qa comments

- [x] 6.1 In `internal/session/manager.go:1002` and `:1092`, change the predicate from `!strings.Contains(evt.Sender, "fordjent") && !strings.Contains(evt.Sender, "djent")` to a marker-based filter: do not set `IsPRReviewFix=true` only when the triggering comment body contains the `<!-- ford -->` marker.
- [x] 6.2 Confirm `isAgentEvent` still drops `<!-- ford -->`-tagged comments BEFORE they hit `getOrCreateSession` (it does today). If not, add a guard so cost-summary comments do not become `_fix` sessions.
- [x] 6.3 Update the test that asserted djent-* senders were excluded. Replace with test that djent-qa actionable comments are routed to `pulls/N-fix` and that `djent-pm` cost-summary comments (with marker) are dropped.

## 7. forgejo_submit_review tool

- [x] 7.1 Add `SubmitReview(repo, pr, state, body)` method to `internal/forgejo/client.go` wrapping `POST /repos/{repo}/pulls/{N}/reviews` with body `{"event": state, "body": body}`.
- [x] 7.2 Add `forgejo_submit_review` tool in `internal/tool/forgejo_tools.go`: params `repository`, `pr_number`, `state`, `body`. On `state=approved`, remove `changes_requested` label if present. On `state=changes_requested`, add the label. On `state=commented`, touch no labels.
- [x] 7.3 Register `forgejo_submit_review` for the reviewer role only (not implementer, not PM).
- [x] 7.4 Update reviewer system prompt to prefer `forgejo_submit_review` for decisive reviews (approve / request changes).
- [x] 7.5 Add unit test harness: `SubmitReview` calls POST with correct body; label add/remove paths execute as expected.

## 8. Support API methods

- [x] 8.1 Add `ListCheckRuns(repo, sha)` and `GetCheckRun(repo, id)` to `internal/forgejo/client.go`. (Forgejo API: `GET /repos/{repo}/commits/{sha}/check-runs`, `GET /repos/{repo}/check-runs/{id}`.)
- [x] 8.2 Add `ListPRReviews(repo, pr)` returning reviews with `state`, `user.login`, `submitted_at` for the gated automerge to find "most recent djent-qa review".
- [x] 8.3 Add a small helper `latestQAreview(reviews)` returning the most recent review whose user is `djent-qa` (or empty).

## 9. Gated automerge

- [x] 9.1 Replace the immediate-merge logic at `internal/session/manager.go:770` with a gated function `evaluateAutomerge(repo, pr)`:
  - fetch PR labels; require `automerge`.
  - fetch head SHA; `ListCheckRuns(repo, sha)`; require all conclusions `success`.
  - `ListPRReviews`; require most recent djent-qa review is `approved` (in yolo repos). In non-yolo repos, accept any `approved` review by any reviewer.
  - require `GetPR(...).Mergeable && !HasConflicts`.
- [x] 9.2 On `check_run.completed`, `issue_comment.created` (non-marker), and `pull_request_review` events, call `evaluateAutomerge` for the PR if it has `automerge` label.
- [x] 9.3 On successful merge, remove the `automerge` label.
- [x] 9.4 Add tests: all states of the gate (green+approved → merge; pending check → wait; failing check → no merge; changes_requested label → no merge; non-yolo no review → wait).

## 10. Yolo reviewer auto-spawn

- [x] 10.1 In `internal/session/manager.go`, on `pull_request.opened` for a repo with `fordjent-yolo` topic AND PR author is `djent-dev`, emit `ReviewRequested` event in-process (similar to the existing synthetic `IssueOpened` dispatch).
- [x] 10.2 Routing: `ReviewRequested` → reviewer role at `pulls/N`.
- [x] 10.3 Confirm `reviewer` role is configured for `djent-qa` (it is, per existing role setup).
- [x] 10.4 Add test: yolo repo + djent-dev PR opened → ReviewRequested emitted. Non-yolo → not emitted.

## 11. Rework counter

- [x] 11.1 In `internal/lifecycle/lifecycle.go`, add `pr_rework` table: `repo TEXT, pr_number INTEGER, attempts INTEGER DEFAULT 0, last_attempt_at TIMESTAMP, PRIMARY KEY(repo, pr_number))`.
- [x] 11.2 Add `IncrementRework(repo, pr) -> attempts` and `GetRework(repo, pr) -> int`.
- [x] 11.3 In manager.go, before spawning `pulls/N-fix` for a PR, check existing-session map. If a `_fix` session for this PR is already active, do NOT increment; reuse by kicking the session.
- [x] 11.4 If no existing session, increment rework. If `attempts > max_rework_attempts`, do NOT spawn — instead add `fordjent/failed:rework-exhausted` + `blocked` labels and post the comment.
- [x] 11.5 Add config option `max_rework_attempts: 3` in config.go + `fordjent.local.yaml`.
- [x] 11.6 Add tests for the counter: first/third/fourth attempt behavior.

## 12. End-to-end test

- [x] 12.1 Add a test scenario in `internal/e2e/` (or a new integration test under `internal/session`) simulating the full loop: dev opens PR → PM/dispatcher emits ReviewRequested → reviewer submits `changes_requested` → implementer `_fix` session fixes → reviewer approves → CI green → gated automerge fires.
- [x] 12.2 Add a second scenario: CI fails on dev PR → implementer `_fix` session fires → pushes fix → CI green → reviewer approves → merge.

## 13. Documentation

- [x] 13.1 Update `scripts/RUNBOOK-local-docker.md` with a "CI failure → dev rework" flow diagram and the new automerge gating list.
- [x] 13.2 Update `AGENTS.md` Bug Fix section with a "Bug Fix N — PR rework loop + gated automerge" entry summarizing the changes.
- [x] 13.3 Confirm `fordjent.local.yaml` documents `max_rework_attempts` and the new behaviors in comments.
