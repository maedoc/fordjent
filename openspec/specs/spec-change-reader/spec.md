## ADDED Requirements

### Requirement: Agent reads spec artifacts within a change
The system SHALL provide an `openspec_read_change` tool that reads a file within an OpenSpec change directory. The tool SHALL accept a change name and a relative file path (e.g., `proposal.md`, `design.md`, `specs/auth-core/spec.md`) and return the file content. The tool SHALL reject paths that escape the change directory.

#### Scenario: Read proposal from change
- **WHEN** `openspec_read_change("user-auth", "proposal.md")` is called
- **AND** `openspec/changes/user-auth/proposal.md` exists
- **THEN** the tool returns the full content of `proposal.md`

#### Scenario: Read design from change
- **WHEN** `openspec_read_change("user-auth", "design.md")` is called
- **AND** `openspec/changes/user-auth/design.md` exists
- **THEN** the tool returns the full content of `design.md`

#### Scenario: Read spec from change
- **WHEN** `openspec_read_change("user-auth", "specs/auth-core/spec.md")` is called
- **AND** `openspec/changes/user-auth/specs/auth-core/spec.md` exists
- **THEN** the tool returns the full spec content

#### Scenario: Path traversal rejected
- **WHEN** `openspec_read_change("user-auth", "../../../etc/passwd")` is called
- **THEN** the tool returns an error: "path escapes change directory"
- **AND** no file content is returned

#### Scenario: File not found in change
- **WHEN** `openspec_read_change("user-auth", "nonexistent.md")` is called
- **AND** the file does not exist in the change directory
- **THEN** the tool returns an error indicating the file was not found

#### Scenario: Change not found
- **WHEN** `openspec_read_change("nonexistent", "proposal.md")` is called
- **AND** the change directory does not exist
- **THEN** the tool returns an error indicating the change was not found
