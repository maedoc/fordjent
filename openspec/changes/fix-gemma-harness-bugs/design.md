## Context

The Gemma4-12B stress test on 2026-06-20 (`fjadmin/gemma-stress`, see `GEMMA-FAILURE-ANALYSIS.md`) classified failures into three buckets: harness bugs (~50%), indecision loops (~35%), genuine capability gaps (~15%). This change addresses the harness-bug bucket only. All three fixes have backing evidence in session memories (`pulls/14`, `pulls/15`, `issues/10`) and were chosen because:

1. A fine-tune or a bigger-model upgrade would NOT fix them — even a perfect model cannot read a file that isn't on the working branch.
2. They are prerequisites to a meaningful model-upgrade decision — once the harness is correct, the test wave gives clean signal on whether the model needs help.

Current state:
- `internal/session/agent.go:212-230` — the branch-checkout block is gated on `evt.Type in {IssueCommentCreated, PullRequestReviewComment}`. It correctly handles human PR comments but misses the *machine-spawned* reviewer paths.
- `internal/tool/forgejo_tools.go` — `forgejo_submit_review` returns English prose about what it did. Token-capped models (Gemma4-12B) re-call the tool because the prose doesn't say "you're done".
- `internal/session/agent.go` (implementer pre-flight) — no dependency check today. The implementer just clones `main`, reads the issue, and begins work — even when the issue text references another issue/PR whose code isn't on `main` yet.

Stakeholders: any LLM provider serving Fordjent (especially smaller local models). Specifically the model-upgrade decision blocked by noise in the Gemma4-12B test.

## Goals / Non-Goals

**Goals:**
- A1: every reviewer-class session (regardless of trigger event type) starts with the PR head branch checked out in its working repo, so `read_file` succeeds for files only present in the PR.
- A2: `forgejo_submit_review` returns a result whose schema unambiguously signals "no further action required"; the per-session idempotency guard doubles as a safety net.
- A3: implementer sessions for bug reports with `#N` references to open, unmerged dependencies are auto-blocked before the LLM spins, instead of running for tens of turns.

**Non-Goals:**
- Selecting / tuning / swapping the LLM model. (The bigger-model decision is separate; see `GEMMA-FAILURE-ANALYSIS.md`.)
- Implementing cross-session state seeding (failure C2 in the analysis). Tracked separately.
- Reducing the reviewer's max turns or reformatting the reviewer system prompt beyond the one explicit anti-pattern line.
- Auto-rebase-on-stale for the reviewer path — the existing stalgate integration covers PR creation. Reviewer sessions start from a freshly fetched PR head; they don't push.
- Migrating existing in-flight sessions.
- Spec immutability enforcement for the new auto-block helper (no spec files involved in the A3 path).

## Decisions

### D1 — Branch-checkout predicate widening (A1)

**Decision**: replace the predicate in `internal/session/agent.go`:

```go
// before
if evt.PRNumber > 0 && (evt.Type == event.IssueCommentCreated || evt.Type == event.PullRequestReviewComment) { ... }

// after
if evt.PRNumber > 0 && isReviewerClassEvent(evt.Type) { ... }
```

with a new helper:
```go
func isReviewerClassEvent(t event.Type) bool {
    return t == event.IssueCommentCreated ||
        t == event.PullRequestReviewComment ||
        t == event.ReviewRequested ||
        t == event.PullRequestOpened
}
```

**Why**: a single predicate is more maintainable than widening the inline condition, and the helper's name documents the intent ("this is a path where the repo should be on the PR head"). All four event types place the session on a PR; for all four, the repo working copy must reflect the PR head.

**Alternatives considered**:
- *Add checkout logic only for `ReviewRequested`*: matches the immediate symptom but leaves `PullRequestOpened` reviewers (the yolo auto-spawned reviewer on PR creation) on `main`. Skipped — half measure.
- *Checkout lazily inside `read_file` when the path is missing*: would mask the underlying bug, slow every read_file miss, and re-introduce a checkout race if two reviewer sessions share a working dir. Skipped.
- *Clone the PR branch fresh instead of fetching into existing repo*: slower (full clone per session) and busts the role-token clone-URL caching. Skipped.

### D2 — In-split handler vs. shared helper for A1
**Decision**: reuse the existing `git fetch origin <branch>` + `git checkout -B <branch> origin/<branch>` code block verbatim, wrapped in the widened predicate. No new helper for the work-tree operation itself — the existing block works; only the gating predicate was wrong.

### D3 — `forgejo_submit_review` definitive result (A2)

**Decision**: change the success return to a small, machine-actionable JSON string:
```json
{
  "status": "ok",
  "pr": 14,
  "state": "approved",
  "action_required": false,
  "note": "Review submitted and labels updated. Do NOT call this tool again with the same state for this PR in this session."
}
```
- Different `note` strings for different states, but the schema is identical.
- Keep the existing label add/remove side-effects unchanged.
- The reviewer system prompt gains one explicit rule: *"Call `forgejo_submit_review` exactly once per PR per review state. The tool's success result is final; do not re-submit."* — single line in the existing "Use `forgejo_submit_review` for decisive verdicts" paragraph.

**Why structured JSON**: smaller models parse explicit `"action_required": false` better than prose. This matches the AGENTS.md model-recommendation language ("use minimax-m2.5 via Ollama Cloud for reliable tool calling" / "12B models selectively drop instructions").

### D4 — Per-session idempotency guard for `forgejo_submit_review` (A2 safety net)

**Decision**: track `(pr_number, state)` tuples already submitted in the *current* reviewer session. If the same `(pr, state)` is requested a second time, return the same success result without calling the Forgejo API again — effectively a NULL-success. Logged at INFO with `"duplicate": true`.

**Why**: the model instruction (D3) is the primary fix, but a backend guard ensures token waste stays bounded even if a future model re-introduces the duplicate-call pattern.

**Alternative considered**: hard-error on duplicate. Skipped — returning an `error: do not call again` could send the model into a different loop. The NULL-success mirrors "your request was already completed".

### D5 — Bug-report dependency pre-flight (A3)

**Decision**: in `internal/session/manager.go` `Manager.handleEvent` (the path that creates `issues/N` implementer sessions), after role detection but before session start, run a `shouldBlockOnUnmergedDependency(ctx, evt)` check:

1. Parse the issue title + body for `#N` references using an extended `parseBugReportRefs` regex (handles "issue #N", "PR #N", "#N", "depends on #N", "blocks #N").
2. For each unique referenced number `N`, call `forgejoClient.GetIssue(repo, N)` (already exists; see Bug Fix 27).
3. If any referenced issue:
   - has `State == "open"` AND
   - has `PullRequest.URL != ""` (i.e. it is a PR) AND
   - has not been merged (i.e. the PR is still open — Forgejo's `Issue.State == "closed"` indicates merged-or-rejected; checking the PR's `merged` field requires the more expensive `GET /pulls/{N}` and is left out as an optimization; an open PR by definition has not merged)
   then: skip session creation, add `blocked` FSM label, append `Depends on: #N\n` to the issue body (or rewrite if already present), post a one-line comment on the original issue: *"Blocked by open PR #N. Will automatically unblock when that PR merges."*
4. If the referenced `N` is an issue WITHOUT a PR — treated as not-blocking (PM/coordination issue, per Bug Fix 27's existing behaviour).
5. Skip pre-flight for issues whose title contains `[pm]` / `[review]` tags (PM issues don't implement, so dependency-block would be wrong).

**Why in manager.go instead of agent.go**: the A3 fix must happen *before* LLM session start (we don't want to pay even one LLM turn for an unsolvable bug report). `Manager.handleEvent` is where the create-session decision happens. `agent.go` only runs once a session exists.

**Alternatives considered**:
- *Have the implementer detect this itself via system-prompt guidance*: the model already struggles to follow this kind of meta-instruction per AGENTS.md findings and the bug-report bucket of GEMMA-FAILURE-ANALYSIS.md. Hard-block is more reliable.
- *Cache the dependency closure across sessions to compute transitive deps*: out of scope for this change. Direct dep check only. The scheduler's `ReconcileBlocked` two-hour safety net (existing) covers any missed webhook cases.
- *Use `GetPR` instead of `GetIssue` for the merged-state check*: Forgejo already collapses "merged PR" into `Issue.State == closed`. We only need to know "is the referenced PR merged" — open/closed state suffices.

### D6 — Auto-block comment format & labels (A3)

**Decision**:
- Label: add the existing `blocked` FSM label only (no `needs-dep` or other new label — re-use existing infra).
- Body mutation: append `\n\nDepends on: #N` if not already present (idempotent).
- Comment body: `Automatically blocked by Fordjent: this issue references open PR #N ("${pr_title}"), which has not yet been merged. The issue will be unblocked automatically when PR #N merges.\n\n<!-- ford -->` (with the agent marker so this comment doesn't re-trigger sessions per Bug Fix 5's marker mechanism).

**Why the marker**: the auto-block comment goes via `djent-pm` style account; without the marker it could itself trigger an `issue_comment.created` session. The existing `isAgentEvent` filter in `internal/webhook/router.go` already handles this pattern.

### D7 — Where the pre-flight check runs (A3)

**Decision**: gated behind a new config flag `enable_bug_report_dep_block` (default `true`), parallel to the pattern of `enable_lifecycle` / `enable_stale_gate` / `enable_scaffold_detection`. If a deployment hits a false positive (e.g. they always file bug reports weeks after the PR merges but keep the issue open), they can disable.

## Risks / Trade-offs

- [Risk] The widened A1 predicate causes a checkout race in concurrent reviewer sessions on the *same* PR if they share a working directory. → **Mitigation**: each reviewer session gets its own `sessions_<key>` working directory; the existing `WorkDir()` already keys by session key (see `internal/session/manager.go` `getOrCreateSession`). Verify with a unit test that two concurrent reviewer sessions on the same PR get separate work-dirs.
- [Risk] A1's `git fetch origin <branch>` fails on a stale PR whose branch was force-pushed and deleted. → **Mitigation**: the existing code already `Warn`s and continues; agent falls through to `main` (same as today, no regression). Add INFO log line when fetch fails so operators can spot it.
- [Risk] A2's idempotency guard hides real re-submission need (e.g. a human asks "actually request changes now instead of approve"). → **Mitigation**: the guard keys on `(pr, state)` — a different state (`changes_requested`) is not a duplicate and proceeds normally.
- [Risk] A3 false-positive: a bug report references "PR #6 introduced this" but PR #6 already merged (so `Issue.State == closed`). The Check #3 in D5 correctly skips blocking on closed-state PRs. → **Mitigation**: closed = not blocking (correct), open = blocking. Edge case: an open PR whose work has been partly reverted — out of scope; manual label removal is the operator escape hatch.
- [Risk] A3 over-blocks legitimate follow-up work: implementer supposed to fix a bug they introduced in PR #6 which IS merged but the original "bug introduced in PR #6" issue is what got re-bug-reported. → **Mitigation**: the referenced `#N` would be the implementer's PR (closed, merged) — not blocked. The check triggers only on *open* referenced issues/PRs.
- [Risk] A3 doesn't fire if the bug report is filed with no PR reference (just describes the symptom). → Out of scope (non-trivial NLP). Geometric mean — false-negative is the *acceptable* outcome (the implementer runs as today).
- [Risk] Network calls in A3 pre-flight: a bug report referencing 5 issues → 5 GetIssue calls → ~250ms latency on session start. → **Acceptable** (one-time cost; far cheaper than 39 LLM turns).
- [Trade-off] The `forgejo_submit_review` result schema change is a contract change for any caller parsing the result — but today no other code parses it, and tests assert on the expected substring, so updating tests is mechanical.

## Migration Plan

1. Apply changes to `internal/session/agent.go`, `internal/tool/forgejo_tools.go`, `internal/session/manager.go`, `internal/forgejo/client.go` (small helper if needed), and `internal/config/config.go` (new flag).
2. Add unit tests in affected `*_test.go` files.
3. Run `go test ./...` — all packages must pass.
4. Build and redeploy the local Fordjent container (`docker build -t fordjent:local .` then re-run, or via the bootstrap-local script).
5. Re-run the gemma test wave: file an issue, file a PR, file a bug report referencing an open PR, observe reviewer behaviour, observe auto-block.
6. No DB migration required.
7. **Rollback**: revert the 4-5 code commits + the new config flag. The new reviewer behaviour degrades back to the existing "check out branch on IssueCommentCreated only"; the new `forgejo_submit_review` result schema is read by no other callers; the auto-block can be disabled via `enable_bug_report_dep_block: false` without code revert.

## Open Questions

- Should the auto-block dependency parser also recognise "depends on the work in #5" (full-sentence references)? Out of scope for this change — the simple `#N` regex catches the common cases (`issue #N`, `PR #N`, `see #N`). The existing `scheduler.parseDependsOn` is the source of truth for recognized syntax; A3's parser will mirror it. If extended reference parsing is needed later, extend `parseDependsOn` and A3 inherits it.
- Should the reviewer be told *which* branch it's now on? Yes — the existing `checked out PR branch for review` INFO log plus a small context message injection (`"[Context] You are on branch '<head>'. read_file succeeds for files in this PR."`) covers this.
