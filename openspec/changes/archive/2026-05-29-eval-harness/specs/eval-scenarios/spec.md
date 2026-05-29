## ADDED Requirements

### Requirement: Greenfield CLI scenario definition
The eval harness SHALL include a greenfield scenario that tests the full PM→implementer→reviewer pipeline by asking the agent to build a `stringutil` CLI from a seeded empty repo.

#### Scenario: Greenfield scenario is correctly defined
- **WHEN** the greenfield scenario struct is initialized
- **THEN** `Name` is `"greenfield"`
- **AND** `RepoName` is `"bench-greenfield"`
- **AND** `IssueTitle` starts with `"[pm]"`
- **AND** `SeedFiles` contains `go.mod`, `.gitignore`, and `README.md`
- **AND** `Timeout` is at least 15 minutes
- **AND** `Verify` is set to `GreenfieldVerify`

#### Scenario: Greenfield seed files create a valid Go module
- **WHEN** the seed files are committed to a new repo
- **THEN** `go build ./...` succeeds (the module compiles)
- **AND** `go test ./...` succeeds (no test failures)
- **AND** the repo has at least 3 files (passes scaffold detection threshold)

#### Scenario: Greenfield issue body specifies deliverables
- **WHEN** the greenfield issue is created
- **THEN** the body describes two commands (`reverse`, `wordcount`)
- **AND** the body specifies the expected file structure
- **AND** the body instructs the PM to use `[implementer]` and `[tester]` tags

### Requirement: Bug fix scenario definition
The eval harness SHALL include a bug fix scenario that tests targeted maintenance by asking the agent to fix a known off-by-one error in existing code.

#### Scenario: Bug fix scenario is correctly defined
- **WHEN** the bug fix scenario struct is initialized
- **THEN** `Name` is `"bugfix"`
- **AND** `RepoName` is `"bench-bugfix"`
- **AND** `IssueTitle` starts with `"[implementer]"`
- **AND** `SeedFiles` contains `go.mod`, `.gitignore`, `pkg/search/search.go`, and `pkg/search/search_test.go`
- **AND** `Timeout` is at least 10 minutes
- **AND** `Verify` is set to `BugfixVerify`

#### Scenario: Bug fix seed code has a deliberate bug
- **WHEN** `go test ./pkg/search/...` is run on the seed code
- **THEN** `TestFindLastElement` fails (the known bug)
- **AND** `TestFindFirstElement`, `TestFindMiddleElement`, `TestNotFound`, and `TestEmptySlice` pass

#### Scenario: Bug fix known solution is minimal
- **WHEN** the known fix is applied to `search.go`
- **THEN** only `search.go` is modified
- **AND** the diff is fewer than 10 lines
- **AND** all 5 tests pass

### Requirement: Scenario struct interface
Each scenario SHALL be defined as a Go struct implementing a standard interface with seed content, issue definition, timeout, and verification function.

#### Scenario: Scenario struct initialization
- **WHEN** a scenario is defined
- **THEN** the struct contains: `Name`, `Description`, `RepoName`, `IssueTitle`, `IssueBody` (string), `SeedFiles` (map[string]string), `Verify` (function), `Timeout` (time.Duration)
- **AND** `SeedFiles` keys are file paths relative to the repo root
- **AND** `SeedFiles` values are file content strings (not base64 — base64 encoding is done by the harness)

#### Scenario: Scenario verification function signature
- **WHEN** a verify function is called
- **THEN** it receives the cloned repo directory path as a string
- **AND** it returns a `VerificationResult` struct containing `Name` (string), `Passed` (bool), `Checks` (slice of Check structs), and `Errors` (slice of strings)

#### Scenario: Check struct captures individual verification steps
- **WHEN** a verification check is recorded
- **THEN** the Check struct contains `Name` (string, e.g., "build", "test", "reverse_hello") and `Passed` (bool)
- **AND** each check represents one atomic verification step (e.g., "go build passes", "test X passes", "functional check Y produces correct output")