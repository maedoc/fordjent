## Purpose

Bug reports filed against work that has not yet landed on `main` are unsolvable: the agent spins for tens of turns searching git history for code that does not exist on its `main` clone. This capability gates implementer session creation on a pre-flight dependency check, so that bug-report issues referencing open, unmerged PRs are auto-blocked instead of spinning.

## ADDED Requirements

### Requirement: Implementer session pre-flight blocks on unmerged dependency
When an implementer-class `IssueOpened` event arrives for a non-PM-tagged issue (i.e. title does not contain `[pm]`, `[review]`, `[decompose]`), the harness SHALL execute a pre-flight dependency check before creating the implementer session.

The pre-flight shall:

1. Parse the issue title and body for issue/PR number references using the existing `Depends on:` syntax recognised by `scheduler.parseDependsOn`, **plus** additional `#N` / `issue #N` / `PR #N` references embedded anywhere in the title or body.
2. For each unique referenced number `N`, fetch the issue via `forgejoClient.GetIssue(repository, N)`.
3. If any referenced issue `N` satisfies ALL of:
   - `State == "open"`
   - the issue has an associated PR (its `pull_request` field is non-empty)
   - the PR is open (equivalently, the referenced issue is in `open` state and is a PR)
   then the pre-flight SHALL:
   - skip implementer session creation;
   - apply the `blocked` FSM label to the triggering issue (if not already present);
   - append (`Depends on: #N`) to the triggering issue body if not already present (idempotent);
   - post a single comment on the triggering issue explaining the block and that auto-unblock will occur when the referenced PR merges.

If the referenced `N` is an issue WITHOUT an associated PR (a PM or coordination issue), it MUST be treated as non-blocking (per the convention established in Bug Fix 27).

If the referenced `N` is a closed PR (merged or rejected), it MUST be treated as non-blocking (the dependency is satisfied).

The pre-flight MUST be gated behind the `enable_bug_report_dep_block` config flag (default `true`). When the flag is `false`, the pre-flight MUST be a no-op and the implementer session proceeds as before.

The auto-block comment MUST include the agent comment marker (`<!-- ford -->`) per the existing isAgentEvent filter so the comment does not trigger a new session.

#### Scenario: Bug report references open unmerged PR — auto-blocked
- **WHEN** issue #10 is created with body "Bug introduced in PR #8. Running `prime 2` says 'no' but should say 'yes'."
- **AND** PR #8 is open (`State == "open"`, has `pull_request` field, not merged)
- **AND** `enable_bug_report_dep_block` is `true`
- **AND** the issue title does not contain `[pm]` or `[review]` or `[decompose]`
- **THEN** the harness does NOT create an implementer session for issue #10
- **AND** the `blocked` FSM label is added to issue #10
- **AND** `Depends on: #8` is appended to issue #10's body (idempotently)
- **AND** the auto-block comment is posted on issue #10 referencing PR #8's title
- **AND** the auto-block comment body contains the `<!-- ford -->` marker

#### Scenario: Bug report references a merged PR — not blocked, session proceeds
- **WHEN** issue #15 is created with body "Bug introduced in PR #6..."
- **AND** PR #6 is closed (merged)
- **THEN** the pre-flight fetch of `GetIssue(#6)` returns `State == "closed"`
- **AND** the pre-flight treats #6 as non-blocking
- **AND** an implementer session is created for issue #15

#### Scenario: Bug report references a PM issue without a PR — not blocked
- **WHEN** issue #20 is created with body "See issue #9 for the PM decomposition"
- **AND** issue #9 is a PM issue with `State == "open"` and no `pull_request` field
- **THEN** the pre-flight treats #9 as non-blocking
- **AND** an implementer session is created for issue #20

#### Scenario: Bug report does not reference any issue/PR — no pre-flight effect
- **WHEN** issue #25 is created with body "Sort command crashes when input is empty"
- **AND** the body contains no `#N` reference
- **THEN** the pre-flight finds no dependencies
- **AND** an implementer session is created for issue #25 (no blocking)

#### Scenario: PM issue is exempt from pre-flight
- **WHEN** issue #30 is created with title `[pm] Plan the new release`
- **AND** the body references "PR #8" for context
- **THEN** the pre-flight is skipped because the title contains the `[pm]` role tag
- **AND** the PM session proceeds normally (PM sessions are not implementers)

#### Scenario: Config flag disabled — pre-flight is no-op
- **WHEN** `enable_bug_report_dep_block: false` is set in `fordjent.local.yaml`
- **AND** a bug-report issue references an open unmerged PR
- **THEN** the pre-flight is not executed
- **AND** the implementer session proceeds as before this change (no auto-block)

#### Scenario: Auto-unblock fires when referenced PR merges
- **WHEN** issue #10 was previously auto-blocked with `Depends on: #8`
- **AND** PR #8 subsequently merges
- **THEN** the existing scheduler `OnPRMerged` path removes the `blocked` label from issue #10
- **AND** the scheduler posts the existing unblock comment
- **AND** issue #10 becomes ready for a new implementer session (no extra code required — reuses existing `scheduler` infrastructure)

#### Scenario: Auto-block comment contains agent marker to prevent self-trigger
- **WHEN** the auto-block comment is posted on issue #10
- **THEN** the comment body contains the `<!-- ford -->` marker
- **AND** the `isAgentEvent` filter in `internal/webhook/router.go` detects the marker and does NOT create a new session from the comment webhook
