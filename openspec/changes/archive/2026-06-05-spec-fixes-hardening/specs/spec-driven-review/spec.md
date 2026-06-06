## ADDED Requirements

### Requirement: `handleSpecLifecycleLabels` logs label-operation errors
The `handleSpecLifecycleLabels` function SHALL log errors from `RemoveIssueLabel` and `AddIssueLabels` operations instead of silently discarding them with `_ =`. Each error SHALL be logged at `slog.Warn` level with the operation name, repository, PR number, and error message.

#### Scenario: Remove label fails
- **WHEN** `handleSpecLifecycleLabels` calls `RemoveIssueLabel` to remove `spec-proposed`
- **AND** the Forgejo API returns an error (e.g., label not found, permission denied)
- **THEN** a warning log is emitted with: "spec labels: failed to remove spec-proposed", the error, repo, and PR number
- **AND** the function continues to attempt `AddIssueLabels`

#### Scenario: Add label fails
- **WHEN** `handleSpecLifecycleLabels` calls `AddIssueLabels` to add `spec-approved`
- **AND** the Forgejo API returns an error
- **THEN** a warning log is emitted with: "spec labels: failed to add spec-approved", the error, repo, and PR number
- **AND** the function continues (non-fatal)

#### Scenario: Both operations succeed
- **WHEN** both `RemoveIssueLabel` and `AddIssueLabels` succeed
- **THEN** no warning logs are emitted
- **AND** the existing info log "spec labels: transitioned spec-proposed → spec-approved" is emitted
