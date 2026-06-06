## MODIFIED Requirements

### Requirement: Path extraction uses language-aware prefix whitelist
`extractPathPrefixes` SHALL detect the repository's primary programming language via `scaffold.DetectProjectLang(repoDir)` and use a language-specific whitelist of first-segment directory prefixes. The whitelist SHALL include standard Go project directories for Go repos, Python project directories for Python repos, and JavaScript project directories for JavaScript repos. For unknown languages, no prefixes SHALL be extracted (empty result, same as current behavior for non-whitelisted paths).

#### Scenario: Go project — whitelisted prefix extracted
- **WHEN** `extractPathPrefixes` is called on a repo with `go.mod`
- **AND** a task description contains `pkg/auth/handler.go`
- **THEN** `pkg/auth` SHALL be extracted

#### Scenario: Python project — whitelisted prefix extracted
- **WHEN** `extractPathPrefixes` is called on a repo with `requirements.txt`
- **AND** a task description contains `app/auth/handler.py`
- **THEN** `app/auth` SHALL be extracted

#### Scenario: JavaScript project — whitelisted prefix extracted
- **WHEN** `extractPathPrefixes` is called on a repo with `package.json`
- **AND** a task description contains `components/Header.jsx`
- **THEN** `components/Header` SHALL be extracted

#### Scenario: Unknown language — no prefixes extracted
- **WHEN** `extractPathPrefixes` is called on a repo with no recognized language manifest
- **AND** a task description contains `some/path/file.ext`
- **THEN** no prefixes SHALL be extracted

#### Scenario: Whitelist is extensible
- **WHEN** a caller adds a new language to the `langPrefixes` map
- **THEN** the corresponding prefixes SHALL be extracted from task descriptions

### Requirement: Downgrade traceability in issue bodies
When `validateParallelTasks` downgrades a parallel task to serial due to file-path overlap, the system SHALL include `*(auto-downgraded from parallel)*` in the issue body after the standard content. The annotation SHALL appear on a separate line.

#### Scenario: Overlapping parallel tasks produce annotated issue
- **WHEN** two `[parallel]` tasks are detected as overlapping (same path prefix)
- **AND** `handleSpecPRMerged` creates implementer issues
- **THEN** the downgraded task's body SHALL contain `*(auto-downgraded from parallel)*`
- **AND** the non-downgraded task's body SHALL NOT contain the annotation

#### Scenario: Non-overlapping parallel tasks have no annotation
- **WHEN** two `[parallel]` tasks have no overlapping path prefixes
- **AND** `handleSpecPRMerged` creates implementer issues
- **THEN** neither issue body SHALL contain `*(auto-downgraded from parallel)*`
