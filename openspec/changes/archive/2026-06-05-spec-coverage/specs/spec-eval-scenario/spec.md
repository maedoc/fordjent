## ADDED Requirements

### Requirement: Spec lifecycle eval scenario definition
The eval harness SHALL include a spec lifecycle scenario that tests the spec-driven implementer workflow by seeding a repo with a merged spec and firing an implementer issue that references it.

#### Scenario: Scenario struct is correctly defined
- **WHEN** the spec lifecycle scenario struct is initialized
- **THEN** `Name` is `"spec-lifecycle"`
- **AND** `RepoName` is `"bench-spec-lifecycle"`
- **AND** `IssueTitle` starts with `"[implementer]"`
- **AND** `SeedFiles` contains `go.mod`, `.gitignore`, `README.md`, and `openspec/changes/stringutil/proposal.md`, `openspec/changes/stringutil/tasks.md`, `openspec/changes/stringutil/specs/string-util/spec.md`
- **AND** `Timeout` is at least 15 minutes
- **AND** `Verify` is set to `SpecLifecycleVerify`

#### Scenario: Seed spec files create valid spec structure
- **WHEN** the seed files are committed to a new repo
- **THEN** `openspec/changes/stringutil/proposal.md` exists with a valid proposal
- **AND** `openspec/changes/stringutil/tasks.md` exists with checkbox items
- **AND** `openspec/changes/stringutil/specs/string-util/spec.md` exists with SHALL requirements
- **AND** the repo passes scaffold detection (3+ files)

#### Scenario: Issue body references the spec
- **WHEN** the spec lifecycle issue is created
- **THEN** the body contains `Spec: stringutil`
- **AND** the body specifies the task to implement
- **AND** the body references the spec for requirements

### Requirement: Spec lifecycle verification function
The `SpecLifecycleVerify` function SHALL clone the repo after the trial and verify that the implementer wrote code that satisfies the spec's requirements.

#### Scenario: Successful spec-driven implementation
- **WHEN** `SpecLifecycleVerify(repoDir)` is called after a successful trial
- **THEN** `go build ./...` passes
- **AND** `go test ./...` passes
- **AND** the spec's verification criteria are met (if any are machine-checkable)
- **AND** `tasks.md` has the implementer's task marked `- [x]`

#### Scenario: Implementation missing
- **WHEN** `SpecLifecycleVerify(repoDir)` is called after a failed trial
- **THEN** `VerificationResult.Passed` is `false`
- **AND** `VerificationResult.Errors` contains the build/test failure messages
