## ADDED Requirements

### Requirement: Harness lifecycle management
The eval harness SHALL start and stop Forgejo and Fordjent as native macOS processes under `sandbox-exec`. The harness SHALL store PIDs and kill processes in `TearDown`. The harness SHALL use random temporary directories for each test run to avoid state leakage.

#### Scenario: Start services successfully
- **WHEN** `NewHarness(t)` is called
- **THEN** Forgejo starts on port 3000 (configurable via `EVAL_FORGEJO_PORT`)
- **AND** Fordjent starts on port 8080 (configurable via `EVAL_FORDJENT_PORT`)
- **AND** both services respond to health checks within 30 seconds
- **AND** PIDs are stored for later cleanup

#### Scenario: TearDown kills processes and cleans up
- **WHEN** `h.TearDown()` is called
- **THEN** Forgejo and Fordjent processes are killed
- **AND** the temporary workdir is removed
- **AND** ports 3000 and 8080 are freed

#### Scenario: Skip setup with EVAL_SKIP_SETUP
- **WHEN** `EVAL_SKIP_SETUP=true` is set in the environment
- **THEN** the harness connects to already-running Forgejo and Fordjent instances
- **AND** no processes are started or stopped
- **AND** `TearDown()` does not kill processes or remove directories

#### Scenario: Skip teardown with EVAL_SKIP_TEARDOWN
- **WHEN** `EVAL_SKIP_TEARDOWN=true` is set in the environment
- **THEN** after the test completes, Forgejo and Fordjent processes remain running
- **AND** the temporary workdir is preserved for manual inspection
- **AND** the harness logs the workdir path for debugging

### Requirement: Forgejo API helpers
The harness SHALL provide Go functions for all Forgejo API operations needed by benchmarks: creating repos, seeding files, creating labels, creating webhooks, creating issues, and polling issue state.

#### Scenario: Create benchmark repo with seed content
- **WHEN** `h.CreateRepo("bench-greenfield")` is called
- **THEN** a Forgejo repository named `bench-greenfield` is created
- **AND** `go.mod`, `.gitignore`, and `README.md` are seeded via the contents API
- **AND** FSM labels (planning, implementing, ready, blocked, done) and role labels (role:implementer, role:pm, etc.) are created
- **AND** a webhook is registered pointing to Fordjent's events endpoint

#### Scenario: Create issue with role tag
- **WHEN** `h.CreateIssue(repo, "[implementer] Fix binary search", issueBody)` is called
- **THEN** an issue is created in the repo with the given title and body
- **AND** the issue number is returned for polling and verification

### Requirement: Completion detection
The harness SHALL detect when Fordjent has finished processing an issue by polling multiple signals, in priority order: issue closed in Forgejo, PR merged, lifecycle state transition, or timeout.

#### Scenario: Issue resolved successfully
- **WHEN** Fordjent processes an issue and the lifecycle state transitions to `completed`
- **THEN** `WaitForCompletion()` returns with `Success = true`
- **AND** the result includes metrics (tokens, turns, time) from the session

#### Scenario: Issue fails with max turns
- **WHEN** Fordjent processes an issue and hits the max-turns limit
- **THEN** `WaitForCompletion()` returns with `Success = false` and `AgentFailure = true`
- **AND** the result includes the `fordjent/failed:max-turns` label on the issue

#### Scenario: Provider outage during processing
- **WHEN** the LLM provider returns errors consistently (timeout, 500, etc.)
- **THEN** `WaitForCompletion()` returns with `Success = false` and `ProviderFailure = true`
- **AND** the trial is excluded from median efficiency calculations but included in pass rate

#### Scenario: Timeout waiting for completion
- **WHEN** Fordjent does not reach a terminal state within 15 minutes
- **THEN** `WaitForCompletion()` returns with `Success = false`
- **AND** the result includes whatever metrics were collected up to the timeout

### Requirement: Metrics collection
The harness SHALL collect metrics from Fordjent's `/status` endpoint, `/trace` endpoint, and log files. Metrics SHALL be recorded per-trial as deltas (end minus start) to handle append-only databases.

#### Scenario: Collect per-trial token and turn metrics
- **WHEN** a trial completes
- **THEN** the harness computes `MetricsDelta{TotalTokens, TotalTurns, WallTime, CostUSD}` as the difference between end-of-trial and start-of-trial `/status` snapshots
- **AND** per-model breakdown (calls, input tokens, output tokens per model) is included

#### Scenario: Count system-role errors from logs
- **WHEN** the harness collects system-role errors
- **THEN** it greps Fordjent's log file for "Unexpected role" and HTTP 400 responses from the LLM provider
- **AND** the count is included in the `TrialResult.SystemRoleErrors` field

#### Scenario: Count false error labels on issues
- **WHEN** an issue has the `fordjent/failed:error` label
- **THEN** the harness checks whether a merged PR or commits exist for that issue
- **AND** if code was successfully produced despite the label, `FalseErrorLabels` is incremented

### Requirement: Verification functions
Each scenario SHALL have a verification function that clones the repo, builds code, runs tests, and checks functional correctness.

#### Scenario: Greenfield verification passes
- **WHEN** `GreenfieldVerify(repoDir)` is called after a successful greenfield trial
- **THEN** `go build ./...` passes
- **AND** `go test ./...` passes
- **AND** `go run ./cmd/stringutil reverse "hello"` outputs "olleh"
- **AND** `go run ./cmd/stringutil wordcount "hello world"` outputs "2"
- **AND** all expected files exist (`cmd/stringutil/main.go`, `pkg/stringutil/reverse.go`, `pkg/stringutil/wordcount.go`)

#### Scenario: Bugfix verification passes
- **WHEN** `BugfixVerify(repoDir)` is called after a successful bugfix trial
- **THEN** `go test ./pkg/search/...` passes (all 5 tests including `TestFindLastElement`)
- **AND** the diff is minimal (only `search.go` changed, < 10 lines)
- **AND** no other files were modified

#### Scenario: Verification identifies failure
- **WHEN** the agent produced non-compiling code
- **THEN** `go build ./...` fails
- **AND** `VerificationResult.Passed = false`
- **AND** `VerificationResult.Errors` includes the build error

### Requirement: Inter-trial isolation
Between trials, the harness SHALL reset the repository to its seed state and wait for Fordjent to become idle.

#### Scenario: Reset repo between trials
- **WHEN** `h.ResetRepo(repo)` is called
- **THEN** all issues in the repo are closed
- **AND** all PRs in the repo are closed
- **AND** all feature branches are deleted
- **AND** the main branch is reset to the seed commit via `git push -f`
- **AND** the harness waits for Fordjent's active session count to reach 0

### Requirement: Baseline comparison and regression detection
The harness SHALL load a committed `baseline.json`, compare current trial results against it, and fail tests on regressions.

#### Scenario: Regression in pass rate
- **WHEN** the current pass rate drops below the baseline pass rate
- **THEN** the test fails with a message showing the regression (e.g., "pass_rate: 5/5 → 3/5")

#### Scenario: System-role errors detected
- **WHEN** any system-role errors are found in the logs
- **THEN** the test fails with a message showing the error count

#### Scenario: Efficiency regression
- **WHEN** `median_total_tokens` exceeds `baseline_median_total_tokens × 1.5`
- **THEN** the test logs a warning but does not fail (informational, not blocking)

#### Scenario: Baseline update
- **WHEN** `go test ./internal/eval/... -v -update-baseline` is run and all tests pass
- **THEN** `baseline.json` is updated with current metrics
- **AND** the test logs "Baseline updated" with the new commit hash

### Requirement: Test functions
The harness SHALL expose three Go test functions: `TestEvalSmoke` (N=1, bugfix only, ~5 min), `TestEvalBenchGreenfield` (N=5, ~30 min), and `TestEvalBenchBugfix` (N=5, ~25 min).

#### Scenario: Smoke test runs in under 10 minutes
- **WHEN** `go test ./internal/eval/... -v -run TestEvalSmoke -timeout 15m` is executed
- **THEN** the test creates a single bugfix trial, runs it, and verifies the outcome
- **AND** the test completes within 10 minutes (including service startup)
- **AND** the test fails if system-role errors are detected or verification fails

#### Scenario: Full benchmark runs with statistical comparison
- **WHEN** `go test ./internal/eval/... -v -timeout 90m` is executed
- **THEN** 5 greenfield trials and 5 bugfix trials are run
- **AND** median metrics are computed across successful trials
- **AND** results are compared against `baseline.json` for regression detection
- **AND** a JSON report is written to `internal/eval/output/YYYY-MM-DD-HHmmss.json`

#### Scenario: Short mode skips benchmarks
- **WHEN** `go test ./internal/eval/... -v -short` is executed
- **THEN** `TestEvalBenchGreenfield` and `TestEvalBenchBugfix` are skipped
- **AND** only unit tests for harness helpers run