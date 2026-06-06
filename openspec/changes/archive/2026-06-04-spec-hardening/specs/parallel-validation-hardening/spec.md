## ADDED Requirements

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

### Requirement: Path extraction uses Go prefix whitelist
`extractPathPrefixes` SHALL only accept path-like tokens whose first segment matches a known Go project directory prefix. The whitelist SHALL include: `cmd`, `internal`, `pkg`, `api`, `web`, `docs`, `configs`, `scripts`.

#### Scenario: Whitelisted prefix extracted
- **WHEN** a task description contains `pkg/auth/handler.go`
- **AND** `extractPathPrefixes` is called
- **THEN** `pkg/auth` SHALL be extracted

#### Scenario: Non-whitelisted prefix ignored
- **WHEN** a task description contains `v1.49/linux-amd64`
- **AND** `extractPathPrefixes` is called
- **THEN** `v1.49` SHALL NOT be extracted

#### Scenario: Whitelist is extensible
- **WHEN** a caller adds `src` to the whitelist
- **THEN** `src/utils` SHALL be extracted from descriptions containing it

### Requirement: Regex compiled once at package level
The path-like regex pattern SHALL be compiled once at package initialization via a package-level `var`, not inside the function body.

#### Scenario: No per-call regexp compilation
- **WHEN** `extractPathPrefixes` is called multiple times
- **THEN** the regex SHALL be compiled exactly once (at init time)
- **AND** the function SHALL reference the package-level `var`
