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

#### Scenario: Yolo mode skips spec PR
- **WHEN** the repo has the `fordjent-yolo` topic
- **AND** the PM has written spec files
- **THEN** the PM commits directly to `main` (or merges immediately)
- **AND** no spec PR is created

### Requirement: PM refines spec in response to review comments (non-yolo)
When a human leaves review comments on a spec PR, the PM SHALL respond by reading the comments, updating the spec files accordingly, and pushing changes to the same spec branch.

#### Scenario: Address review feedback on design
- **WHEN** a human comments on spec PR #42: "Use bcrypt not scrypt"
- **AND** the PM session processes the comment
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

### Requirement: PM archives completed changes
When directed to archive a change, the PM SHALL move the change directory to `openspec/changes/archive/`, sync any delta specs, and commit the result.

#### Scenario: Archive completed change
- **WHEN** the PM is directed to archive change "user-auth"
- **THEN** the PM calls `openspec_archive_change("user-auth")`
- **AND** the PM commits the archive move and delta spec syncs to a branch
- **AND** the PM pushes the branch so the archive is available for merge

#### Scenario: Archive creates a PR in non-yolo mode
- **WHEN** the repo does NOT have `fordjent-yolo` topic
- **AND** the PM archives a change
- **THEN** the archive commit is pushed to a branch and a PR is created
- **AND** the PR is labeled `spec-complete`
