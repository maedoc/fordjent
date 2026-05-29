## ADDED Requirements

### Requirement: Implementer reads spec for implementation context
When processing a `[implementer]` issue created from a spec, the implementer SHALL read the relevant spec files and `tasks.md` from the merged change using the `openspec_get_tasks` and `openspec_read_spec` tools.

#### Scenario: Implementer reads tasks and spec before coding
- **WHEN** an implementer picks up issue `[implementer] Implement OAuth flow`
- **AND** the issue body references `openspec/changes/user-auth/tasks.md`
- **THEN** the implementer calls `openspec_get_tasks("user-auth")` to see all tasks and status
- **AND** the implementer calls `openspec_read_spec("auth-oauth")` for requirements
- **AND** the implementer begins coding after understanding the spec

#### Scenario: Implementer works without spec reference
- **WHEN** an implementer picks up an issue that does NOT reference a spec change
- **THEN** the implementer falls back to existing behavior (reads issue body, explores codebase)
- **AND** no spec-related tools are called

### Requirement: openspec_get_tasks returns structured task information
The `openspec_get_tasks` tool SHALL accept a change name and return the task list from `tasks.md` including descriptions, completion status, and parallel tags. The response SHALL include context about which task the current issue corresponds to, when determinable from the issue body.

#### Scenario: Get tasks with context
- **WHEN** `openspec_get_tasks("user-auth")` is called
- **THEN** the response includes:
  - Change name and description
  - Task list with `[x]`/`[ ]` status
  - Which task this session should implement (matched from issue body)
  - The spec capabilities relevant to each task

#### Scenario: tasks.md has been updated by another session
- **WHEN** task 1 is already marked `[x]` by a parallel implementer session
- **AND** `openspec_get_tasks("user-auth")` is called by the implementer for task 2
- **THEN** the response shows task 1 as complete and task 2 as the active task
- **AND** the implementer proceeds with task 2

### Requirement: openspec_read_spec returns capability requirements
The `openspec_read_spec` tool SHALL accept a capability name and return the full spec content from `openspec/specs/<capability>/spec.md` or, if the spec is still in a change, from `openspec/changes/<name>/specs/<capability>/spec.md`.

#### Scenario: Read merged spec capability
- **WHEN** `openspec_read_spec("auth-core")` is called
- **AND** `openspec/specs/auth-core/spec.md` exists
- **THEN** the response returns the full spec content including requirements, scenarios, and verification criteria

#### Scenario: Read spec from active change (not yet merged)
- **WHEN** `openspec_read_spec("auth-core")` is called
- **AND** `openspec/specs/auth-core/` does not exist
- **AND** `openspec/changes/user-auth/specs/auth-core/spec.md` exists
- **THEN** the tool finds the spec in the active change and returns its content

#### Scenario: Spec not found
- **WHEN** `openspec_read_spec("nonexistent")` is called
- **AND** no spec with that name exists anywhere
- **THEN** the tool returns an error: "Spec 'nonexistent' not found"
- **AND** the implementer asks the PM for clarification

### Requirement: openspec_mark_task updates task completion
The `openspec_mark_task` tool SHALL accept a change name and task index (1-based), and update the corresponding `- [ ]` checkbox to `- [x]` in `tasks.md`. The implementer SHALL call this after creating a PR for the task.

#### Scenario: Mark task complete after PR creation
- **WHEN** the implementer creates a PR for task 2
- **AND** calls `openspec_mark_task("user-auth", 2)`
- **THEN** task 2 in `tasks.md` changes from `- [ ]` to `- [x]`
- **AND** the file is committed and pushed to the implementer's branch
- **AND** the PR is created with the updated tasks.md

#### Scenario: Mark task complete idempotent
- **WHEN** `openspec_mark_task("user-auth", 2)` is called
- **AND** task 2 is already `- [x]`
- **THEN** the tool returns success without modifying the file

### Requirement: Implementer validates against verification contract
Before creating a PR, the implementer SHALL check the spec's `## Verification` section (if present) and confirm that all criteria are met. The implementer SHALL report verification results in the PR description.

#### Scenario: Verification passes before PR
- **WHEN** the implementer has implemented the OAuth flow
- **AND** the spec's verification section says:
  - `- [ ] go build ./... succeeds`
  - `- [ ] go test ./pkg/auth/... passes`
- **AND** the implementer runs `go build ./...` and `go test ./pkg/auth/...`
- **AND** both pass
- **THEN** the PR description includes: "## Verification: ✅ All checks pass"

#### Scenario: Verification fails before PR
- **WHEN** verification criteria include `go test ./pkg/auth/... passes`
- **AND** the tests fail
- **THEN** the implementer does NOT create the PR
- **AND** the implementer fixes the failing tests
- **AND** re-runs verification until it passes

### Requirement: Implementer reports spec deviations
If the implementation cannot satisfy a spec requirement, the implementer SHALL report the deviation via `forgejo_ping_parent` with reason `spec_conflict`, including the specific requirement and the reason it cannot be met.

#### Scenario: Spec requires impossible behavior
- **WHEN** the spec says "The system SHALL use PostgreSQL for session storage"
- **AND** the repository only has SQLite available
- **THEN** the implementer calls `forgejo_ping_parent(parent, "spec_conflict", "Spec requires PostgreSQL but repo uses SQLite. Options: (1) add PostgreSQL dependency, (2) update spec to allow SQLite.")`
- **AND** the implementer does NOT proceed with implementation until the PM responds
