## MODIFIED Requirements

### Requirement: SpecManager marks tasks complete
`SpecManager` SHALL update a specific task's checkbox in `tasks.md` from `- [ ]` to `- [x]` using `strings.Replace` with a count of 1 on the identified line. The update SHALL be idempotent — marking an already-complete task is a no-op. The implementation SHALL NOT use regex capture-group replacement (`ReplaceAllString` with `$1`/`$2` expansion), as this is fragile when task descriptions contain special characters.

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

#### Scenario: Task description contains dollar signs
- **WHEN** a task line is `- [ ] Fix $2 pricing bug`
- **AND** `SpecManager.MarkTaskComplete(repoDir, "pricing", 1)` is called
- **THEN** the line becomes `- [x] Fix $2 pricing bug`
- **AND** the dollar sign is preserved (not interpreted as regex capture group)

### Requirement: SpecPRManager detects spec PRs
`SpecPRManager` SHALL detect whether a pull request contains spec files by checking if any changed file path matches `openspec/changes/`. Detection SHALL use the Forgejo API `GET /repos/{repo}/pulls/{N}/files` response. When a PR contains files from multiple changes, `IsSpecPR` SHALL return only a single change name. Multi-change PRs are not supported — the PM convention is one change per PR.

#### Scenario: PR contains spec files
- **WHEN** a PR's changed files include `openspec/changes/user-auth/proposal.md`
- **AND** `SpecPRManager.IsSpecPR(repo, prNumber)` is called
- **THEN** the function returns `true` and the change name `user-auth`

#### Scenario: PR contains only code files
- **WHEN** a PR's changed files are only `pkg/auth/handler.go` and `pkg/auth/handler_test.go`
- **AND** `SpecPRManager.IsSpecPR(repo, prNumber)` is called
- **THEN** the function returns `false` with an empty change name

#### Scenario: PR contains files from multiple changes
- **WHEN** a PR's changed files include `openspec/changes/user-auth/proposal.md` and `openspec/changes/api-v2/spec.md`
- **AND** `SpecPRManager.IsSpecPR(repo, prNumber)` is called
- **THEN** the function returns `true` with one of the change names
- **AND** the other change is silently ignored (multi-change PRs are not supported)

### Requirement: SpecManager archives completed changes
`SpecManager` SHALL move a completed change from `openspec/changes/<name>/` to `openspec/changes/archive/YYYY-MM-DD-<name>/`. Before archiving, it SHALL check that the source change exists and sync delta specs from `openspec/changes/<name>/specs/` to `openspec/specs/<capability>/`. If the source change does not exist, the function SHALL return a clear error indicating the change was not found.

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

#### Scenario: Archive nonexistent change
- **WHEN** `SpecManager.ArchiveChange(repoDir, "nonexistent")` is called
- **AND** `openspec/changes/nonexistent/` does not exist
- **THEN** the function returns an error containing "not found"
- **AND** no directories are created or modified
