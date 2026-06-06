## ADDED Requirements

### Requirement: PM creates spec artifacts for features
When processing a `[pm]` issue, the PM SHALL create a complete OpenSpec change directory with `proposal.md`, `design.md` (for complex changes), capability specs under `specs/`, and `tasks.md`. The PM SHALL write these files using the existing `write_file` tool, commit them to a spec branch, and push.

#### Scenario: Full spec creation for complex feature
- **WHEN** a PM processes `[pm] Build user authentication system`
- **AND** the PM determines the change is multi-file and cross-package
- **THEN** the PM creates:
  - `openspec/changes/user-auth/proposal.md` (why, what changes, capabilities, impact)
  - `openspec/changes/user-auth/design.md` (architecture decisions, data flow)
  - `openspec/changes/user-auth/specs/auth-core/spec.md` (requirements, scenarios)
  - `openspec/changes/user-auth/specs/auth-oauth/spec.md` (requirements, scenarios)
  - `openspec/changes/user-auth/tasks.md` (implementation steps with checkboxes)
- **AND** all files are committed to branch `spec/user-auth`
- **AND** the branch is pushed to origin

#### Scenario: Simple change skips design.md
- **WHEN** a PM processes `[pm] Fix typo in README`
- **AND** the PM determines the change is single-file and trivial
- **THEN** the PM creates proposal.md and tasks.md
- **AND** design.md is omitted
- **AND** the PM notes in a comment that design was skipped due to simplicity

### Requirement: PM creates spec PR for human review (non-yolo)
In non-yolo mode, the PM SHALL create a pull request from the spec branch to `main` with the `spec-proposed` label. The PR description SHALL summarize the spec and link to the parent issue.

#### Scenario: Create spec PR
- **WHEN** the PM has pushed spec files to `spec/user-auth`
- **AND** the repo does NOT have the `fordjent-yolo` topic
- **THEN** `forgejo_create_pr` is called with base `main`, head `spec/user-auth`
- **AND** the PR gets label `spec-proposed`
- **AND** the PM posts a comment on the parent issue: "Spec PR #N ready for review"
- **AND** the parent issue is not labeled `blocked` (human review gate replaces blocking)

> **Event routing note**: The `event.SpecPRMerged` type exists but is **never emitted**. The webhook router emits `event.PullRequestMerged` for all merged PRs, and `handleSpecPRMerged` acts as a sidecar — it checks `SpecPRManager.IsSpecPR()` and returns early for non-spec PRs. This sidesteps the need for a separate event dispatch path.

#### Scenario: Yolo mode skips spec PR
- **WHEN** the repo has the `fordjent-yolo` topic
- **AND** the PM has written spec files
- **THEN** the PM commits directly to `main` (or merges immediately)
- **AND** no spec PR is created
- **AND** the scheduler creates implementer issues immediately

### Requirement: PM refines spec in response to review comments (non-yolo)
When a human leaves review comments on a spec PR, the PM SHALL re-activate, read the comments, update the spec files accordingly, and push changes to the same spec branch.

#### Scenario: Address review feedback on design
- **WHEN** a human comments on spec PR #42: "Use bcrypt not scrypt"
- **AND** the webhook triggers a PM session on the PR
- **THEN** the PM reads the comment, updates `design.md` to change scrypt→bcrypt
- **AND** commits and pushes to the same `spec/user-auth` branch
- **AND** posts a comment: "Updated per feedback: bcrypt instead of scrypt"

#### Scenario: PM asks for clarification on ambiguous feedback
- **WHEN** a human leaves a vague comment: "This could be better"
- **THEN** the PM posts a comment asking: "Could you clarify which section? The design choices, a specific spec requirement, or the task breakdown?"
- **AND** the PM does not modify spec files until clarification is received

### Requirement: PM decomposes tasks with parallelism awareness
When writing `tasks.md`, the PM SHALL identify tasks that touch completely disjoint files and mark them with `[parallel]`. The PM SHALL NOT add `blocked` labels — the scheduler manages blocking via `Depends on:`.

#### Scenario: Mark independent tasks as parallel
- **WHEN** the PM creates `tasks.md` with tasks touching files `pkg/auth/handler.go` and `pkg/middleware/ratelimit.go`
- **AND** these files have no overlap
- **THEN** task entries include `[parallel]` tag:
  ```
  - [ ] Implement OAuth handler [parallel]
  - [ ] Add rate limiting middleware [parallel]
  ```

#### Scenario: Dependent tasks are serial
- **WHEN** task B depends on code created by task A
- **THEN** task B includes `Depends on: #<task-a-issue>` in the task description line
- **AND** task B does NOT have `[parallel]` tag

### Requirement: PM includes verification contracts in specs
Each capability spec SHALL include a `## Verification` section with a checkbox list of concrete, testable criteria that the reviewer and implementer can use to validate completion.

#### Scenario: Spec with verification contract
- **WHEN** the PM creates `specs/auth-core/spec.md`
- **THEN** the spec includes:
  ```markdown
  ## Verification
  - [ ] `go build ./...` succeeds
  - [ ] `go test ./pkg/auth/...` passes with >80% coverage
  - [ ] `curl -X POST localhost:8080/login -d '{"user":"test","pass":"test"}'` returns 200
  - [ ] Invalid credentials return 401
  - [ ] Expired tokens return 401 with "token expired" message
  ```

### Requirement: PM closes the spec loop on completion
When all implementation tasks for a change are complete, the PM SHALL be re-activated to archive the change, sync delta specs, and post a completion summary.

#### Scenario: Archive completed change
- **WHEN** all tasks in `tasks.md` are complete and all PRs merged
- **AND** the scheduler triggers a PM follow-up session
- **THEN** the PM calls `openspec_archive_change("user-auth")`
- **AND** the PM commits the archive move and delta spec syncs to `main`
- **AND** the PM labels the parent issue `spec-complete`
- **AND** the PM posts a completion summary with token/time metrics

#### Scenario: Archive creates a PR in non-yolo mode
- **WHEN** the repo does NOT have `fordjent-yolo` topic
- **AND** the PM archives a change
- **THEN** the archive commit is pushed to a branch and a PR is created
- **AND** the human can review and merge the archive
- **AND** the PR is labeled `spec-complete`
