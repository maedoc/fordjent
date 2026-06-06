## ADDED Requirements

### Requirement: Integration test exercises full spec PR lifecycle
The system SHALL include an integration test in `internal/session/` that exercises the complete spec PR lifecycle using a fake Forgejo: spec PR merge event → `handleSpecPRMerged` → implementer issues created → milestone attached → `spec-implementing` label added → `handleSpecLifecycleLabels` transitions `spec-proposed` to `spec-approved`.

#### Scenario: Full lifecycle with 3 tasks
- **WHEN** a `PullRequestMerged` event is dispatched for PR containing `openspec/changes/test-feature/proposal.md`
- **AND** the fake Forgejo returns 3 tasks from `openspec/changes/test-feature/tasks.md`
- **THEN** 3 `[implementer]` issues are created with correct titles
- **AND** a milestone is created and all 3 issues are attached
- **AND** `spec-implementing` label is added to the merged PR
- **AND** `handleSpecLifecycleLabels` removes `spec-proposed` and adds `spec-approved`
- **AND** a summary comment is posted on the PR listing all tasks

#### Scenario: Lifecycle with partially completed tasks
- **WHEN** `tasks.md` has 4 tasks but 2 are already marked `- [x]`
- **THEN** only 2 `[implementer]` issues are created (completed tasks skipped)
- **AND** the summary comment lists 2 tasks

#### Scenario: Non-spec PR is ignored
- **WHEN** a `PullRequestMerged` event is dispatched for a PR that contains only `pkg/auth/handler.go`
- **THEN** no issues are created, no labels are changed, no comment is posted

### Requirement: Integration test verifies spec tool responses
The integration test SHALL call spec tools (`openspec_get_tasks`, `openspec_read_spec`, `openspec_mark_task`) against a real `SpecManager` backed by a temp directory and verify the responses contain expected data.

#### Scenario: openspec_get_tasks returns parsed tasks
- **WHEN** `openspec_get_tasks("test-feature")` is called with a valid change directory
- **AND** the change has 3 tasks (1 complete, 2 incomplete, 1 parallel)
- **THEN** the response includes: "3 total, 1 complete, 2 remaining"
- **AND** each task is listed with its index, description, and `[parallel]` tag where applicable

#### Scenario: openspec_read_spec returns capability spec
- **WHEN** `openspec_read_spec("test-cap")` is called
- **AND** `openspec/specs/test-cap/spec.md` exists
- **THEN** the response returns the full spec content

#### Scenario: openspec_mark_task updates and commits
- **WHEN** `openspec_mark_task("test-feature", 2)` is called on a git repo
- **THEN** task 2 in `tasks.md` changes from `- [ ]` to `- [x]`
- **AND** the change is committed and pushed (if git is available)
