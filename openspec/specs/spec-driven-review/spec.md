## MODIFIED Requirements

### Requirement: Reviewer checks implementation against spec
When reviewing a PR created from a spec-driven issue, the reviewer SHALL read the relevant capability specs and verify that the implementation satisfies all requirements. The reviewer SHALL flag any requirement that is not met. Before attempting to merge a spec-driven PR, the reviewer SHALL check the repo's merge policy (`NoAutoMerge`, `RequireReview`) and respect it — the reviewer MUST NOT call `forgejo_merge_pr` if the policy prohibits it.

#### Scenario: Review against spec finds no issues — merge allowed
- **WHEN** a reviewer processes PR #45 implementing the OAuth flow
- **AND** the reviewer reads `openspec_read_spec("auth-oauth")`
- **AND** the implementation satisfies all SHALL/MUST requirements
- **AND** the repo allows agent merging (no no-auto-merge policy, no required-review gate)
- **THEN** the reviewer approves the PR (or merges if policy allows)

#### Scenario: Review against spec — merge blocked by policy
- **WHEN** the implementation satisfies all spec requirements
- **AND** the repo has a `no-auto-merge` policy
- **THEN** the reviewer posts a review comment instead of merging
- **AND** does NOT call `forgejo_merge_pr`

#### Scenario: Review finds missing requirement
- **WHEN** the spec says "The system SHALL return 401 for expired tokens"
- **AND** the implementation returns 500 for expired tokens
- **THEN** the reviewer posts a comment: "Spec requirement not met: expired tokens must return 401, not 500."
- **AND** the reviewer requests changes

#### Scenario: Review of non-spec-driven PR
- **WHEN** the PR was NOT created from a spec-driven issue
- **THEN** the reviewer falls back to existing behavior (check correctness, style, tests)
- **AND** no spec-related checks are performed
