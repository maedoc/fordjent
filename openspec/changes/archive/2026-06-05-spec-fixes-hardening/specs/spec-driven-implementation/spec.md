## MODIFIED Requirements

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

## ADDED Requirements

### Requirement: `openspec_mark_task` rolls back on commit failure
The `openspec_mark_task` tool SHALL roll back the local `tasks.md` mutation if `git commit` or `git push` fails. After mutating the file, if the commit fails, the tool SHALL revert the checkbox change (`- [x]` back to `- [ ]`) on the affected line before returning the error string. If the revert itself fails, the tool SHALL log a warning and return the error string.

#### Scenario: Commit fails after marking task
- **WHEN** `openspec_mark_task("user-auth", 2)` is called
- **AND** the local tasks.md is modified (task 2 becomes `- [x]`)
- **AND** `git commit` fails (e.g., pre-commit hook rejection)
- **THEN** the tool reverts task 2 back to `- [ ]` in the local file
- **AND** the tool returns a warning string indicating the failure

#### Scenario: Push fails after commit succeeds
- **WHEN** `openspec_mark_task("user-auth", 2)` is called
- **AND** the local tasks.md is modified and committed
- **AND** `git push` fails
- **THEN** the tool returns a warning string indicating push failed
- **AND** the local commit remains (can be pushed manually later)

#### Scenario: Rollback itself fails
- **WHEN** the tool attempts to revert the checkbox change after a commit failure
- **AND** the revert operation fails (e.g., file permissions)
- **THEN** the tool logs a warning about the revert failure
- **AND** returns a warning string indicating both the commit and rollback failures

### Requirement: `openspec_archive_change` scopes git add to openspec/
The `openspec_archive_change` tool SHALL use `git add openspec/` instead of `git add -A` to stage only spec-related files for the archive commit. This prevents unrelated working-tree changes from being swept into the commit.

#### Scenario: Archive with unrelated dirty working tree
- **WHEN** `openspec_archive_change("user-auth")` is called
- **AND** the working tree has modifications to `cmd/main.go` and `openspec/changes/user-auth/tasks.md`
- **THEN** only files under `openspec/` are staged for the archive commit
- **AND** the modification to `cmd/main.go` is NOT included in the commit

#### Scenario: Archive with clean working tree
- **WHEN** `openspec_archive_change("user-auth")` is called
- **AND** the working tree has no unrelated modifications
- **THEN** all openspec files (archive move, delta spec syncs) are staged and committed

### Requirement: `handleSpecPRMerged` reads tasks from merge commit SHA
When a spec PR is merged, `handleSpecPRMerged` SHALL read `tasks.md` from the PR's merge commit SHA instead of the `main` branch ref. This guarantees the exact content that was merged, closing a race window where another PR could merge to `main` between the event and the read.

#### Scenario: Read from merge SHA
- **WHEN** a spec PR for `user-auth` is merged with SHA `abc1234`
- **AND** `handleSpecPRMerged` processes the merge event
- **THEN** `GetFile` is called with ref `abc1234` (not `"main"`)
- **AND** the tasks.md content matches what was in the merged PR

#### Scenario: Merge SHA unavailable — fallback to main
- **WHEN** a spec PR is merged but the PR object has an empty `MergeCommitSHA`
- **THEN** `handleSpecPRMerged` falls back to reading from `"main"`
- **AND** a debug log is emitted noting the fallback

### Requirement: `specChangeRefRegex` rejects prose false positives
The `extractSpecChangeRef` function SHALL use a regex that requires the explicit marker `Spec:` (capital S, colon) followed by a kebab-case name, anchored to line start or after a newline. The regex SHALL NOT match lowercase `spec:`, `Change:`, `change:`, or standalone words like "climate change" or "spec file".

#### Scenario: Valid spec reference extracted
- **WHEN** an issue body contains `Spec: user-auth`
- **THEN** `extractSpecChangeRef` returns `"user-auth"`

#### Scenario: Valid spec reference with openspec path
- **WHEN** an issue body contains `Spec: openspec/changes/user-auth`
- **THEN** `extractSpecChangeRef` returns `"user-auth"`

#### Scenario: Lowercase spec ignored
- **WHEN** an issue body contains `spec: user-auth`
- **THEN** `extractSpecChangeRef` returns empty string

#### Scenario: Change marker ignored
- **WHEN** an issue body contains `Change: user-auth`
- **THEN** `extractSpecChangeRef` returns empty string

#### Scenario: Prose with "climate change" ignored
- **WHEN** an issue body contains `We need to address climate change: urgent`
- **THEN** `extractSpecChangeRef` returns empty string

### Requirement: `handleSpecPRMerged` counts actual issue-creation successes
`handleSpecPRMerged` SHALL track the number of issues that were successfully created versus the number that failed. The log message and summary comment SHALL report both counts instead of only the total.

#### Scenario: All issues created successfully
- **WHEN** `handleSpecPRMerged` processes a spec PR with 3 tasks
- **AND** all 3 `createSpecIssue` calls succeed
- **THEN** the log message reports "3/3 issues created"
- **AND** the summary comment lists all 3 tasks

#### Scenario: Some issues fail to create
- **WHEN** `handleSpecPRMerged` processes a spec PR with 3 tasks
- **AND** 2 `createSpecIssue` calls succeed and 1 fails
- **THEN** the log message reports "2/3 issues created (1 failed)"
- **AND** the summary comment lists all 3 tasks with the failed one marked
