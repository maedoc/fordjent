## Why

The Gemma4-12B stress test on `fjadmin/gemma-stress` (2026-06-20, 13 sessions, 489 LLM turns, 5.7M tokens) revealed that roughly half of the observed failures are **harness bugs, not model-capability limits**. The findings are documented in `GEMMA-FAILURE-ANALYSIS.md`. The three most impactful failures — reviewer sessions cannot read PR files, `forgejo_submit_review` is called repeatedly because its success message is ambiguous, and bug-report sessions spin for tens of turns on code that was never merged — are all fixable in code without changing the model. Fixing them is a prerequisite to any future fine-tuning or model-upgrade decision because a fine-tune would otherwise teach the model to work around the bugs.

## What Changes

- **A1 — Reviewer branch checkout**: extend `internal/session/agent.go` branch-checkout predicate (currently `evt.PRNumber > 0 && evt.Type in {IssueCommentCreated, PullRequestReviewComment}`) to also fire for `event.ReviewRequested` and `event.PullRequestOpened`. Without this the auto-spawned reviewer (and the yolo reviewer spawned on PR open) stays on `main` and cannot read files that only exist on the PR head branch. Backing evidence: `pulls/15` ran 17 turns in a `read_file` → error loop on `Makefile` that existed only on `feature/add-makefile`.
- **A2 — `forgejo_submit_review` definitive success**: change the tool's success result from soft past-tense prose (`Review "approved" submitted on PR #14. Label side-effects applied.`) to a structured, machine-actionable JSON result that explicitly states `action_required: false` and instructs the model **not** to call the tool again for the same state. Add an explicit anti-pattern to the reviewer system prompt: "Do not call `forgejo_submit_review` more than once for the same `state` per PR." Backing evidence: `pulls/14` submitted `approved` three times in a row.
- **A3 — Bug-report dependency pre-flight**: when an implementer session starts for an issue whose body references another issue/PR number (the bug report references "issue #N" / "PR #N" and the referenced issue is open with no merged PR), the harness auto-blocks the new issue with the `Depends on: #N` syntax and the `blocked` FSM label, and skips session creation. This unlocks when the referenced PR merges via the existing scheduler. Backing evidence: `issues/10` spent 39 turns searching git history for prime-checker code that was never on `main` because PR #8 was merge-queue-blocked.

## Capabilities

### New Capabilities
- `bug-report-dependency-block`: pre-flight check on implementer sessions that auto-blocks bug-report issues referencing unmerged dependencies, instead of letting the agent spin.

### Modified Capabilities
- `spec-driven-review`: the reviewer must have its repo checked out to the PR head branch before the LLM loop starts, regardless of which event type (`ReviewRequested`, `PullRequestOpened`, `IssueCommentCreated`, `PullRequestReviewComment`) spawned the review session. Also, `forgejo_submit_review` must return a definitive machine-actionable success result so the agent does not re-submit.

## Impact

- **Affected code**:
  - `internal/session/agent.go` — widen branch-checkout predicate; inject `"[Context] You are on branch '<head>'. read_file will work for files in this PR."` into reviewer context.
  - `internal/tool/forgejo_tools.go` — restructure `forgejo_submit_review` tool result; non-fatal Idempotency guard that returns a NULL-success on duplicate `state` re-submission within the same session.
  - `internal/session/agent.go` (implementer pre-flight) — parse issue body for `#N` references, look each up via Forgejo `GetIssue`, check the `PullRequest` reference / closed state, and trigger auto-block.
  - `internal/scheduler/scheduler.go` — dependency parser already recognises `Depends on: #N`; behaviour continues unchanged.
  - `internal/forgejo/client.go` — small helper for "is this issue/PR merged" (already partially exists: `Issues.GetIssue` returns `pull_request` field and `State`, see Bug Fix 27 in AGENTS.md).
- **Affected specs/deltas**: `specs/spec-driven-review/spec.md` (delta), new `specs/bug-report-dependency-block/spec.md`.
- **No breaking changes** — all change is additive: existing event types continue to behave as before; the new behaviour kicks in only on event types that previously dropped through to a broken code path.
- **Affected systems**: local Forgejo instance, the auto-spawned reviewer path, the implementer session start path. No database schema changes.
- **Test impact**: New unit tests for branch-checkout predicate coverage; `forgejo_submit_review` idempotency; bug-reference parsing + auto-block decision logic. E2E coverage: reviewer can read PR files end-to-end; bug report with unmerged dep is auto-blocked.
