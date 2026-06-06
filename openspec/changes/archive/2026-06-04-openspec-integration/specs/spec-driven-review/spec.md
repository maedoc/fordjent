## ADDED Requirements

### Requirement: Reviewer checks implementation against spec
When reviewing a PR created from a spec-driven issue, the reviewer SHALL read the relevant capability specs and verify that the implementation satisfies all requirements. The reviewer SHALL flag any requirement that is not met.

#### Scenario: Review against spec finds no issues
- **WHEN** a reviewer processes PR #45 implementing the OAuth flow
- **AND** the reviewer reads `openspec/specs/auth-oauth/spec.md`
- **AND** the implementation satisfies all SHALL/MUST requirements
- **AND** verification criteria all pass
- **THEN** the reviewer approves the PR (or merges if policy allows)

#### Scenario: Review finds missing requirement
- **WHEN** the spec says "The system SHALL return 401 for expired tokens"
- **AND** the implementation returns 500 for expired tokens
- **THEN** the reviewer posts a comment: "Spec requirement not met: expired tokens must return 401, not 500. See `specs/auth-oauth/spec.md` Requirement: Token expiration handling."
- **AND** the reviewer requests changes

#### Scenario: Review of non-spec-driven PR
- **WHEN** the PR was NOT created from a spec-driven issue
- **THEN** the reviewer falls back to existing behavior (check correctness, style, tests)
- **AND** no spec-related checks are performed

### Requirement: Reviewer checks verification contract
The reviewer SHALL independently verify each criterion in the spec's `## Verification` section. The reviewer SHALL NOT rely solely on the implementer's self-reported verification.

#### Scenario: All verification criteria pass
- **WHEN** the spec's verification section has 4 criteria
- **AND** the reviewer runs each check independently
- **AND** all 4 pass
- **THEN** the review comment includes: "✅ Verification: 4/4 criteria met"
- **AND** the reviewer proceeds to code review

#### Scenario: Verification criteria fail
- **WHEN** one verification criterion fails (e.g., test coverage is 60% but spec requires >80%)
- **THEN** the reviewer posts: "❌ Verification: 3/4 criteria met. Failed: test coverage (60% < 80% required)"
- **AND** the reviewer requests changes

### Requirement: Reviewer flags spec-implementation divergences
If the reviewer finds that the implementation deviates from the spec in a way that may be intentional (better approach, simplified implementation), the reviewer SHALL flag it as a divergence rather than a failure, allowing the PM or human to decide whether to update the spec or the code.

#### Scenario: Implementation improves on spec
- **WHEN** the spec says "Use in-memory session store"
- **AND** the implementation uses Redis for sessions (better, but different)
- **THEN** the reviewer posts: "⚠️ Spec divergence: implementation uses Redis instead of in-memory session store. This is an improvement but diverges from the spec. Update the spec or confirm this is acceptable."
- **AND** the reviewer does NOT block the PR on this divergence

### Requirement: Review round cap prevents infinite review loops
The system SHALL limit spec-driven PRs to a maximum of 3 review rounds (implement → review → fix → re-review → fix → re-review). After the third round, the reviewer SHALL escalate to a human with a summary of remaining issues.

> **Implementation note**: The review round cap is **prompt-level enforcement only**. The reviewer's system prompt instructs it to track rounds and escalate after 3. There is no hard counter stored in session metadata — the reviewer derives the round count from PR comment history or session state. If the LLM ignores the instruction, nothing blocks round 4.

#### Scenario: Third review round reached
- **WHEN** a PR has been through 3 review-fix-review cycles
- **AND** issues remain
- **THEN** the reviewer posts: "⚠️ Review round cap (3) reached. Remaining issues: [list]. Escalating to human for decision."
- **AND** the reviewer does NOT request further changes
- **AND** the `needs-human-review` label is added to the PR

#### Scenario: Review converges before cap
- **WHEN** a PR is reviewed in round 1, implementer fixes issues
- **AND** round 2 review finds no remaining issues
- **THEN** the reviewer approves/merges normally
- **AND** the review round counter resets

### Requirement: Reviewer uses spec for merge decisions
When policy allows the reviewer to merge, the reviewer SHALL verify that all spec requirements are met, verification criteria pass, and no unresolved divergences exist before merging.

#### Scenario: Merge when spec is fully satisfied
- **WHEN** all spec requirements are met
- **AND** all verification criteria pass
- **AND** no unresolved divergences exist
- **AND** the repo policy allows agent merging
- **THEN** the reviewer calls `forgejo_merge_pr` to merge the PR

#### Scenario: Block merge on spec failures
- **WHEN** one or more spec requirements are not met
- **THEN** the reviewer does NOT merge
- **AND** the reviewer posts a comment listing unmet requirements
- **AND** the reviewer requests changes or escalates to human
