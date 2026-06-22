## MODIFIED Requirements

### Requirement: Reviewer checks implementation against spec
When reviewing a PR created from a spec-driven issue, the reviewer SHALL read the relevant capability specs and verify that the implementation satisfies all requirements. The reviewer SHALL flag any requirement that is not met. Before attempting to merge a spec-driven PR, the reviewer SHALL check the repo's merge policy (`NoAutoMerge`, `RequireReview`) and respect it — the reviewer MUST NOT call `forgejo_merge_pr` if the policy prohibits it.

#### Scenario: Review against spec finds no issues — merge allowed
- **WHEN** a reviewer processes PR #45 implementing the OAuth flow
- **AND** the reviewer reads `openspec_read_spec("auth-oauth")`
- **AND** the implementation satisfies all SHALL/MUST requirements
- **AND** the repo allows agent merging (no no-auto-merge policy, no required-review gate)
- **THEN** the reviewer approves the PR (or merges if policy allows)

#### Scenario: Review against spec — merge blocked by policy
- **WHEN** the implementation satisfies all spec requirements
- **AND** the repo has a `no-auto-merge` policy
- **THEN** the reviewer posts a review comment instead of merging
- **AND** does NOT call `forgejo_merge_pr`

#### Scenario: Review finds missing requirement
- **WHEN** the spec says "The system SHALL return 401 for expired tokens"
- **AND** the implementation returns 500 for expired tokens
- **THEN** the reviewer posts a comment: "Spec requirement not met: expired tokens must return 401, not 500."
- **AND** the reviewer requests changes

#### Scenario: Review of non-spec-driven PR
- **WHEN** the PR was NOT created from a spec-driven issue
- **THEN** the reviewer falls back to existing behavior (check correctness, style, tests)
- **AND** no spec-related checks are performed

### Requirement: Reviewer session repo checked out on PR head branch
Before the LLM loop starts in any reviewer-class session (a session spawned on a PR by any of `IssueCommentCreated`, `PullRequestReviewComment`, `ReviewRequested`, or `PullRequestOpened`), the harness SHALL ensure the session's working repository is checked out to the PR head branch.

The branch checkout SHALL be performed by fetching the PR head ref and running `git checkout -B <head-ref> origin/<head-ref>` against the session's working repository.

If the fetch or checkout fails, the harness SHALL log at WARN level with the session key and the failed branch name, and SHALL proceed with the LLM loop. (This is the existing fallback behavior; this requirement only adds the trigger condition, not the error handling.)

The harness SHALL inject a context message into the reviewer's first turn stating the active branch and confirming that `read_file` succeeds for files in the PR.

#### Scenario: Auto-spawned yolo reviewer reads PR-only file
- **WHEN** a `PullRequestOpened` event arrives for a yolo-policy repo authored by `djent-dev`
- **AND** the manager dispatches a synthetic `ReviewRequested` event spawning a reviewer session at `repo/pulls/N`
- **THEN** the harness fetches the PR head ref and checks out the head branch in the reviewer's working repository
- **AND** the harness logs `checked out PR branch for review` with `branch=<head>` and the session key
- **AND** the `read_file` tool, when called on a file present only on the PR head branch, returns the file content (no "no such file or directory" error)

#### Scenario: Reviewer session spawned by ReviewRequested reads PR-only file
- **WHEN** a `ReviewRequested` event spawns a reviewer session (synthetic, from the yolo auto-spawn path or any future dispatcher)
- **THEN** the harness checks out the PR head branch before the LLM loop starts
- **AND** the reviewer can read files added by the PR via `read_file`

#### Scenario: Reviewer session spawned by human PR comment continues to read PR-only file (no regression)
- **WHEN** an `IssueCommentCreated` event on an open PR triggers a `pulls/N-fix` implementer session
- **THEN** the harness continues to fetch and check out the PR head branch (unchanged from pre-change behavior)
- **AND** the implementer can `read_file` PR-head-only files

#### Scenario: Fetch failure logged but does not abort session
- **WHEN** the PR head ref fetch fails (e.g. branch was force-pushed and ref no longer exists)
- **THEN** the harness logs a WARN line with the session key and the failed branch name
- **AND** the LLM loop proceeds (the session is not aborted)

### Requirement: `forgejo_submit_review` returns machine-actionable success result
The `forgejo_submit_review` tool SHALL return a structured JSON result on success. The result SHALL include a `status` field equal to `"ok"`, the reviewed PR number, the submitted review `state`, an `action_required` boolean equal to `false`, and a `note` field instructing the model not to call the tool again for the same state in the current session.

The tool SHALL enforce per-session idempotency: if called twice in the same session with the same `(pr_number, state)` pair, the second call SHALL return the same success result without contacting Forgejo. The duplicate call SHALL be logged at INFO level with `"duplicate": true`.

The reviewer's system prompt SHALL state that `forgejo_submit_review` MUST be called exactly once per PR per review state, and that the success result is final.

#### Scenario: Reviewer submits approved review, tool returns structured success
- **WHEN** the reviewer calls `forgejo_submit_review(repository, pr_number=14, state="approved", body="...")`
- **AND** the Forgejo API accepts the review
- **THEN** the tool returns a JSON string containing `"status": "ok"`, `"pr": 14`, `"state": "approved"`, `"action_required": false`
- **AND** the JSON `note` field instructs "Do NOT call this tool again with the same state"
- **AND** the side-effects (adding/removing `changes_requested` label) execute exactly as before

#### Scenario: Reviewer re-submits same state — idempotent duplicate NULL-success returned
- **WHEN** the reviewer calls `forgejo_submit_review(repository, pr_number=14, state="approved", body="...")` a second time in the same session
- **THEN** the tool does NOT make a second HTTP call to Forgejo
- **AND** the tool returns the same success result schema
- **AND** the tool logs an INFO line containing `"duplicate": true`
- **AND** no additional side-effects execute (no redundant label changes)

#### Scenario: Reviewer submits different state — not a duplicate
- **WHEN** the reviewer previously called `forgejo_submit_review(state="approved")`
- **AND** now calls `forgejo_submit_review(state="changes_requested")` for the same PR
- **THEN** the call is NOT treated as a duplicate
- **AND** the Forgejo API is contacted and side-effects execute normally

#### Scenario: Reviewer system prompt contains single-call rule
- **WHEN** a reviewer session is created and the system prompt is built
- **THEN** the prompt contains an instruction stating `forgejo_submit_review` MUST be called at most once per PR per review state
