## 1. A1 — Reviewer branch checkout predicate widening

- [x] 1.1 In `internal/session/agent.go`, add a helper `isReviewerClassEvent(t event.Type) bool` that returns `true` for `event.IssueCommentCreated`, `event.PullRequestReviewComment`, `event.ReviewRequested`, and `event.PullRequestOpened`.
- [x] 1.2 Replace the inline branch-checkout predicate at `internal/session/agent.go:212` (`evt.PRNumber > 0 && evt.Type == event.IssueCommentCreated || evt.Type == event.PullRequestReviewComment`) with `evt.PRNumber > 0 && isReviewerClassEvent(evt.Type)`.
- [x] 1.3 Add a context-message injection after the checkout: `"[Context] You are on branch '<head>'. read_file succeeds for files in this PR."` appended to `contextMessages` for reviewer sessions (only when checkout succeeded).
- [x] 1.4 Add an INFO log line "checked out PR branch for review" already exists — verify it fires for `ReviewRequested` and `PullRequestOpened` triggers in a unit test.
- [x] 1.5 Unit test in `internal/session/agent_test.go` (or a focused new test): the branch-checkout path is taken for all four event types.

## 2. A2 — `forgejo_submit_review` definitive success + idempotency

- [x] 2.1 In `internal/tool/forgejo_tools.go`, modify the `forgejo_submit_review` tool's returned success string to a JSON schema: `{"status":"ok","pr":<N>,"state":"<state>","action_required":false,"note":<str>}` where `note` says "Review submitted and labels updated. Do NOT call this tool again with the same state for this PR in this session." Different `note` text per state is acceptable; schema must be identical.
- [x] 2.2 Add a `submittedReviews` map (keyed by `pr_number:state`) to the `forgejoSubmitReviewTool` struct. Initialize in the constructor.
- [x] 2.3 On a second call with the same `(pr_number, state)` in the same session, return the same success JSON without calling the Forgejo API; log INFO `"duplicate": true, "pr": <N>, "state": <state>`; do not re-execute the label side-effects.
- [x] 2.4 In `internal/session/agent.go` reviewer system prompt, add a single rule: "Call `forgejo_submit_review` exactly once per PR per review state. The tool's success result is final; do not re-submit." Add it to the existing "Use `forgejo_submit_review` for decisive verdicts" paragraph.
- [x] 2.5 Unit test: calling the tool twice with the same state returns the same success schema and only one Forgejo POST is made (use a fake client / httptest).
- [x] 2.6 Unit test: calling `submit_review(approved)` then `submit_review(changes_requested)` for the same PR is NOT treated as a duplicate — both calls execute.

## 3. A3 — Bug-report dependency pre-flight block

- [x] 3.1 In `internal/config/config.go`, add `EnableBugReportDepBlock bool` field with `mapstructure:"enable_bug_report_dep_block"` tag, default `true`.
- [x] 3.2 In `fordjent.local.yaml`, add `enable_bug_report_dep_block: true` with a comment, alongside the other `enable_*` flags.
- [x] 3.3 Add a helper `extractIssueRefs(title, body string) []int` in `internal/scheduler/scheduler.go` (or a small new util in the same package) that returns all unique `#N` integers referenced anywhere in the title/body, including `Depends on: #N`, `issue #N`, `PR #N`, `#N` standalone. Uses regex; re-uses / extends the existing `parseDependsOn` regex if natural.
- [x] 3.4 In `internal/session/manager.go` `Manager.handleEvent` (the path that creates `issues/N` implementer sessions), AFTER role detection and BEFORE session spawn, call `shouldBlockOnUnmergedDep(ctx, evt)` if `cfg.Agent.EnableBugReportDepBlock != false`. Skip the gate when the issue title contains `[pm]`/`[review]`/`[decompose]`.
- [x] 3.5 `shouldBlockOnUnmergedDep`: for each ref returned by `extractIssueRefs`, call `forgejoClient.GetIssue(repo, N)`. Return the FIRST `(blockedRefNum, blockedIssue)` that satisfies: `State == "open"` AND `PullRequest.URL != "" || PullRequest.HTMLURL != ""`. Otherwise return nil.
- [x] 3.6 When a block is detected: skip session creation, add `blocked` label via `forgejoClient.AddIssueLabels(repo, issue, ["blocked"])` (existing dedup applies per Bug 21), append `\n\nDepends on: #N` to the issue body via `forgejoClient.EditIssueBody` (new small wrapper or inline `PATCH /repos/{repo}/issues/{N}` with merged body), and post an auto-block comment with `<!-- ford -->` marker.
- [x] 3.7 Unit test: bug report referencing open PR → blocked, no session, label applied, comment posted, body appended.
- [x] 3.8 Unit test: bug report referencing merged PR → no block, session proceeds.
- [x] 3.9 Unit test: bug report referencing a non-PR PM issue (open) → no block.
- [x] 3.10 Unit test: title `[pm] ...` references open PR → pre-flight skipped.
- [x] 3.11 Unit test: `enable_bug_report_dep_block: false` → pre-flight is no-op.

## 4. New Forgejo client / capability plumbing

- [x] 4.1 Verify `forgejo.GetIssue` returns the `PullRequest` field correctly (per AGENTS.md Bug 24 / Bug 27) — no change if present; add assertion in tests.
- [x] 4.2 Add `forgejoClient.EditIssueBody(repo, issue, body)` to `internal/forgejo/client.go` if not present — wraps `PATCH /repos/{repo}/issues/{N}` with `{"body": body}`. (The existing `AddIssueLabels` already handles label add; only body edit needs adding.)
- [x] 4.3 Verify the auto-block comment path uses the agent-comment marker (`<!-- ford -->`) so `isAgentEvent` filters the resulting `issue_comment.created` webhook.

## 5. Build, test, redeploy, re-test

- [x] 5.1 `go build ./...` — compiles.
- [x] 5.2 `go test ./...` — all packages pass (the only documented pre-existing failure is `TestBashToolSuccess` on Alpine without `bash`, which does not run in CI on macOS).
- [x] 5.3 `docker build -t fordjent:local .` — image builds.
- [x] 5.4 Restart the local Fordjent container with the new image (or re-run `scripts/bootstrap-local.sh`).
- [x] 5.5 File an issue + PR + a bug-report issue referencing the open (unmerged) PR on `fjadmin/gemma-stress` (or a fresh test repo).
- [x] 5.6 Verify in logs: reviewer session logs `"checked out PR branch for review", branch=<head>` for a `ReviewRequested`-spawned reviewer.
- [x] 5.7 Verify in logs: reviewer calls `forgejo_submit_review` exactly once per state per PR for the same `state` (no `duplicate: true` log unless the model actually retried — in which case the duplicate IS logged as evidence the guard fired).
- [x] 5.8 Verify: bug-report issue referencing open PR is auto-blocked — label `blocked` applied, `Depends on: #N` appended to body, auto-block comment posted with marker, no implementer session spawned.
- [x] 5.9 Merge the referenced PR; verify scheduler unblocks the bug-report issue (reuses existing scheduler path).
- [x] 5.10 Update `AGENTS.md` with the new "Bug Fix #34 / A1+A2+A3" section summarizing what changed, evidence, and the test result.
