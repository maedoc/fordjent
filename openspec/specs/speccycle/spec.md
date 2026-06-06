## MODIFIED Requirements

### Requirement: SpecManager lists active changes
`SpecManager` SHALL list all non-archived changes by scanning `openspec/changes/` and excluding the `archive/` subdirectory. Each change entry SHALL include its name, schema (read from `.openspec.yaml` if present), and last-modified timestamp. If `.openspec.yaml` does not exist or cannot be parsed, the `Schema` field SHALL be empty.

#### Scenario: List changes with active and archived
- **WHEN** `openspec/changes/` contains `user-auth/` and `archive/2026-05-29-old-feature/`
- **AND** `SpecManager.ListChanges(repoDir)` is called
- **THEN** the result contains `user-auth` only
- **AND** `archive/` entries are excluded
- **AND** each entry includes the change name, schema, and last-modified time

#### Scenario: Change has .openspec.yaml with schema
- **WHEN** `openspec/changes/user-auth/.openspec.yaml` contains `schema: spec-driven`
- **AND** `SpecManager.ListChanges(repoDir)` is called
- **THEN** the `user-auth` entry has `Schema: "spec-driven"`

#### Scenario: Change has no .openspec.yaml
- **WHEN** `openspec/changes/user-auth/` exists but has no `.openspec.yaml`
- **AND** `SpecManager.ListChanges(repoDir)` is called
- **THEN** the `user-auth` entry has `Schema: ""` (empty, not an error)

#### Scenario: No active changes
- **WHEN** `openspec/changes/` contains only `archive/`
- **AND** `SpecManager.ListChanges(repoDir)` is called
- **THEN** the result is an empty list
