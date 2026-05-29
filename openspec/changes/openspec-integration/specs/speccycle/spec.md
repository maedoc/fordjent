## ADDED Requirements

### Requirement: SpecManager creates OpenSpec change directories
`SpecManager` SHALL create the directory structure for a new change at `openspec/changes/<name>/` with a `.openspec.yaml` scaffold file. The manager SHALL use Go stdlib (`os`, `path/filepath`) and SHALL NOT depend on any external CLI.

#### Scenario: Create a new change directory
- **WHEN** `SpecManager.CreateChange(repoDir, "user-auth")` is called
- **THEN** `openspec/changes/user-auth/` is created with a `.openspec.yaml` file
- **AND** the `.openspec.yaml` contains schema name and change metadata
- **AND** the function returns nil error

#### Scenario: Change directory already exists
- **WHEN** `SpecManager.CreateChange(repoDir, "user-auth")` is called and `openspec/changes/user-auth/` already exists
- **THEN** the function returns an error indicating the change already exists
- **AND** no files are modified

### Requirement: SpecManager lists active changes
`SpecManager` SHALL list all non-archived changes by scanning `openspec/changes/` and excluding the `archive/` subdirectory. Each change entry SHALL include its name, schema, and last-modified timestamp.

#### Scenario: List changes with active and archived
- **WHEN** `openspec/changes/` contains `user-auth/` and `archive/2026-05-29-old-feature/`
- **AND** `SpecManager.ListChanges(repoDir)` is called
- **THEN** the result contains `user-auth` only
- **AND** `archive/` entries are excluded
- **AND** each entry includes the change name and last-modified time

#### Scenario: No active changes
- **WHEN** `openspec/changes/` contains only `archive/`
- **AND** `SpecManager.ListChanges(repoDir)` is called
- **THEN** the result is an empty list

### Requirement: SpecManager parses tasks from tasks.md
`SpecManager` SHALL parse `openspec/changes/<name>/tasks.md` and return a structured task list. Each task SHALL include its description, completion status (`- [ ]` or `- [x]`), and any `[parallel]` tag.

#### Scenario: Parse tasks with mixed completion
- **WHEN** `tasks.md` contains:
  ```
  - [x] Create auth module
  - [ ] Implement OAuth flow [parallel]
  - [ ] Write integration tests
  ```
- **AND** `SpecManager.ParseTasks(repoDir, "user-auth")` is called
- **THEN** the result contains 3 tasks
- **AND** task 1 is marked complete, tasks 2 and 3 incomplete
- **AND** task 2 has `Parallel: true`

#### Scenario: Parse empty or missing tasks.md
- **WHEN** `tasks.md` does not exist or contains no checkbox lines
- **AND** `SpecManager.ParseTasks(repoDir, "user-auth")` is called
- **THEN** the result is an empty task list
- **AND** no error is returned

#### Scenario: Parse malformed tasks lines gracefully
- **WHEN** `tasks.md` contains lines without checkboxes or with invalid formatting
- **AND** `SpecManager.ParseTasks(repoDir, "user-auth")` is called
- **THEN** malformed lines are skipped
- **AND** valid checkbox lines are parsed correctly
- **AND** a warning is logged for each skipped line

### Requirement: SpecManager marks tasks complete
`SpecManager` SHALL update a specific task's checkbox in `tasks.md` from `- [ ]` to `- [x]`. The update SHALL be idempotent — marking an already-complete task is a no-op.

#### Scenario: Mark an incomplete task complete
- **WHEN** `SpecManager.MarkTaskComplete(repoDir, "user-auth", 2)` is called
- **THEN** the 2nd `- [ ]` line in `tasks.md` becomes `- [x]`
- **AND** all other lines are unchanged

#### Scenario: Mark an already-complete task
- **WHEN** `SpecManager.MarkTaskComplete(repoDir, "user-auth", 1)` is called and task 1 is already `- [x]`
- **THEN** `tasks.md` is unchanged
- **AND** no error is returned

#### Scenario: Mark out-of-range task index
- **WHEN** `SpecManager.MarkTaskComplete(repoDir, "user-auth", 99)` is called and there are only 5 tasks
- **THEN** the function returns an error indicating index out of range

### Requirement: SpecManager archives completed changes
`SpecManager` SHALL move a completed change from `openspec/changes/<name>/` to `openspec/changes/archive/YYYY-MM-DD-<name>/`. Before archiving, it SHALL sync delta specs from `openspec/changes/<name>/specs/` to `openspec/specs/<capability>/`.

#### Scenario: Archive a completed change with delta specs
- **WHEN** `SpecManager.ArchiveChange(repoDir, "user-auth")` is called
- **AND** `openspec/changes/user-auth/specs/auth-core/spec.md` exists
- **THEN** `openspec/changes/user-auth/` is moved to `openspec/changes/archive/2026-06-03-user-auth/`
- **AND** `openspec/specs/auth-core/spec.md` exists with the delta content
- **AND** the original change directory no longer exists under `changes/`

#### Scenario: Archive change with no delta specs
- **WHEN** `SpecManager.ArchiveChange(repoDir, "bugfix-typo")` is called
- **AND** `openspec/changes/bugfix-typo/specs/` is empty or does not exist
- **THEN** the change is moved to archive without syncing specs
- **AND** no spec directories are created under `openspec/specs/`

#### Scenario: Archive target already exists
- **WHEN** `openspec/changes/archive/2026-06-03-user-auth/` already exists
- **AND** `SpecManager.ArchiveChange(repoDir, "user-auth")` is called
- **THEN** the function returns an error
- **AND** the existing change is not moved

### Requirement: SpecPRManager detects spec PRs
`SpecPRManager` SHALL detect whether a pull request contains spec files by checking if any changed file path matches `openspec/changes/`. Detection SHALL use the Forgejo API `GET /repos/{repo}/pulls/{N}/files` response.

#### Scenario: PR contains spec files
- **WHEN** a PR's changed files include `openspec/changes/user-auth/proposal.md`
- **AND** `SpecPRManager.IsSpecPR(repo, prNumber)` is called
- **THEN** the function returns `true` and the change name `user-auth`

#### Scenario: PR contains only code files
- **WHEN** a PR's changed files are only `pkg/auth/handler.go` and `pkg/auth/handler_test.go`
- **AND** `SpecPRManager.IsSpecPR(repo, prNumber)` is called
- **THEN** the function returns `false` with an empty change name

### Requirement: SpecPRManager generates implementer issues on spec merge
When a spec PR is merged, `SpecPRManager` SHALL parse `tasks.md` from the merged change and create one Forgejo issue per task. For `[parallel]` tasks, the scheduler SHALL validate file disjointness via the merge queue before creating parallel issues without inter-task dependencies.

#### Scenario: Spec PR merged, tasks generated
- **WHEN** a spec PR for `user-auth` is merged
- **AND** `tasks.md` contains 3 tasks: "Create auth module", "Implement OAuth flow [parallel]", "Write integration tests [parallel]"
- **AND** merge queue confirms "Implement OAuth flow" and "Write integration tests" touch disjoint files
- **THEN** 3 Forgejo issues are created with `[implementer]` tags
- **AND** tasks 2 and 3 have no `Depends on:` between them (parallel)
- **AND** task 1 has `Depends on: #<parent>` referencing the PM issue
- **AND** all issues are attached to the correct milestone

#### Scenario: Parallel tasks conflict at file level
- **WHEN** two `[parallel]` tasks would touch the same file
- **AND** the merge queue reports file overlap
- **THEN** the scheduler falls back to serial ordering
- **AND** the second task gets `Depends on: #<first-task-issue>`

#### Scenario: Spec PR merged with no tasks.md
- **WHEN** a spec PR is merged but `tasks.md` is empty or missing
- **THEN** the scheduler logs a warning
- **AND** no implementer issues are created
- **AND** the parent issue is not labeled `spec-implementing`

### Requirement: Spec lifecycle labels track change progress
The system SHALL use Forgejo labels to track change lifecycle: `spec-proposed` (spec PR open), `spec-approved` (spec merged to main), `spec-implementing` (implementation in progress), `spec-complete` (all tasks done and archived). Labels SHALL be applied automatically by the scheduler and PM agent.

#### Scenario: Full lifecycle label transitions
- **WHEN** a spec PR is opened → `spec-proposed` label added
- **AND** the spec PR is merged → `spec-proposed` removed, `spec-approved` added
- **AND** implementer issues are created → `spec-implementing` added
- **AND** all tasks complete and change archived → `spec-implementing` removed, `spec-complete` added

#### Scenario: Spec PR closed without merge
- **WHEN** a spec PR is closed without merging
- **THEN** `spec-proposed` label is removed
- **AND** the parent issue returns to its prior state
- **AND** no implementer issues are created
