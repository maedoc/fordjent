## MODIFIED Requirements

### Requirement: openspec_propose validates a change name and creates the directory
The `openspec_propose` tool SHALL validate a change name, create the change directory at `openspec/changes/<name>/` (including the `specs/` subdirectory), and return instructions for creating spec artifacts. The directory creation ensures the PM can immediately use `write_file` to create spec files within the change. If the change directory already exists, the tool SHALL return an error.

#### Scenario: New change — directory created
- **WHEN** `openspec_propose(repository="test/repo", change_name="user-auth")` is called
- **THEN** `openspec/changes/user-auth/` is created with a `specs/` subdirectory
- **AND** the response includes artifact creation instructions

#### Scenario: Duplicate change — error returned
- **WHEN** `openspec_propose(repository="test/repo", change_name="user-auth")` is called
- **AND** `openspec/changes/user-auth/` already exists
- **THEN** the tool returns an error: "change \"user-auth\" already exists"
- **AND** no files are modified

#### Scenario: Invalid change name — error returned
- **WHEN** `openspec_propose(repository="test/repo", change_name="user/auth")` is called
- **AND** the change name contains path separators
- **THEN** the tool returns an error: "change_name must not contain path separators"
