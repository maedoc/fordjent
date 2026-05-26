## 1. Package Scaffold and Harness Core

- [ ] 1.1 Create `internal/eval/` package with `Harness` struct (fields: ForgejoURL, FordjentURL, tokens, PIDs, LocalDir, Client) and `NewHarness(t)` / `TearDown()` methods. Implement startForgejo and startFordjent using `os/exec` with `sandbox-exec`, matching `bootstrap-local.sh` patterns. [spec: eval-harness/Harness lifecycle management]
- [ ] 1.2 Implement `TearDown()` — kill processes by PID, remove workdir, release ports. Add `EVAL_SKIP_TEARDOWN` flag support to preserve state for debugging. [spec: eval-harness/Harness lifecycle management]
- [ ] 1.3 Implement `EVAL_SKIP_SETUP` flag — when set, connect to already-running Forgejo+Fordjent instead of starting new instances. Skip process management entirely. [spec: eval-harness/Harness lifecycle management]
- [ ] 1.4 Generate Fordjent config YAML from template (`fordjent.yaml`) with environment variable substitution for tokens and ports. Write config and sandbox profile files to the workdir. [spec: eval-harness/Harness lifecycle management]

## 2. Forgejo API Helpers

- [ ] 1.5 Implement `forgejo.go` with `CreateRepo()`, `SeedFiles()` (base64-encode content + POST to contents API), `CreateLabels()` (FSM + role labels), `CreateWebhook()`, `CreateIssue()`. Each function wraps the existing `internal/forgejo.Client` where possible, or falls back to direct HTTP calls. [spec: eval-harness/Forgejo API helpers]
- [ ] 1.6 Implement `WaitForCompletion()` — poll Forgejo for issue state, Fordjent `/status` for lifecycle state, and timeout after configurable duration. Return `TrialResult` with success/failure reason. [spec: eval-harness/Completion detection]

## 3. Metrics Collection

- [ ] 2.1 Implement `metrics.go` with `MetricsSnapshot` struct (TotalTokens, InputTokens, OutputTokens, TotalTurns, ToolCalls, CostUSD, ByModel, SessionCount, FailedSessionCount). Parse from Fordjent `/status` JSON response. [spec: eval-harness/Metrics collection]
- [ ] 2.2 Implement `RecordBaseline()` and `ComputeDelta()` — snapshot metrics before and after each trial, compute per-trial deltas. Handle append-only databases by computing end-minus-start. [spec: eval-harness/Metrics collection]
- [ ] 2.3 Implement `CountSystemRoleErrors()` — grep Fordjent log file for "Unexpected role" and HTTP 400 responses. Return count. [spec: eval-harness/Metrics collection]
- [ ] 2.4 Implement `CountFalseErrorLabels()` — query Forgejo API for issues with `fordjent/failed:error` label, check each for merged PRs or commits. Return count of false labels. [spec: eval-harness/Metrics collection]

## 4. Verification Functions

- [ ] 2.5 Implement `verify.go` with `runCommand()` and `runCommandOutput()` helpers. Implement `GreenfieldVerify()` — clone repo, `go build`, `go test`, functional checks (`stringutil reverse "hello"` → "olleh", `stringutil wordcount "hello world"` → "2"), file existence checks. [spec: eval-harness/Verification functions, eval-scenarios/Greenfield CLI scenario definition]
- [ ] 2.6 Implement `BugfixVerify()` — `go test ./pkg/search/...` passes (all 5 tests), diff is < 10 lines in `search.go` only, no other files modified. [spec: eval-harness/Verification functions, eval-scenarios/Bug fix scenario definition]

## 5. Scenario Definitions

- [ ] 3.1 Implement `scenarios.go` with `Scenario` struct (Name, Description, RepoName, IssueTitle, IssueBody, SeedFiles map[string]string, Verify func(string) VerificationResult, Timeout time.Duration). [spec: eval-scenarios/Scenario struct interface]
- [ ] 3.2 Implement `scenarios_greenfield.go` — `GreenfieldScenario` with seed files (go.mod, .gitignore, README.md), issue body specifying reverse+wordcount commands, and `GreenfieldVerify` function. [spec: eval-scenarios/Greenfield CLI scenario definition]
- [ ] 3.3 Implement `scenarios_bugfix.go` — `BugfixScenario` with seed files (go.mod, .gitignore, pkg/search/search.go with deliberate off-by-one, pkg/search/search_test.go with `TestFindLastElement`), and `BugfixVerify` function. [spec: eval-scenarios/Bug fix scenario definition]

## 6. Trial Execution and Inter-Trial Isolation

- [ ] 3.4 Implement `RunTrial()` — create repo, seed files, create labels, create webhook, record baseline metrics, create issue, wait for completion, collect metrics, verify outcome, return `TrialResult`. [spec: eval-harness/Completion detection]
- [ ] 3.5 Implement `ResetRepo()` — close all issues via Forgejo API, close all PRs, delete feature branches, git reset main to seed commit, wait for idle. [spec: eval-harness/Inter-trial isolation]
- [ ] 3.6 Implement provider failure detection — log scanning for "context deadline exceeded" and rate-limit HTTP status codes. Mark trial results with `ProviderFailure` field. [spec: eval-harness/Completion detection]

## 7. Baseline Comparison

- [ ] 4.1 Implement `baseline.go` with `Baseline` struct, `LoadBaseline()`, `Save()`, and `Compare()` methods. Compare pass rate (FAIL on regression), system-role errors (FAIL on any), false error rate (FAIL if > 0.2), token/turn/time medians (WARN if > 1.5x baseline). [spec: eval-harness/Baseline comparison and regression detection]
- [ ] 4.2 Create `baseline.json` with initial empty structure. This will be populated after the first real benchmark run. [spec: eval-harness/Baseline comparison and regression detection]
- [ ] 4.3 Implement `-update-baseline` flag in test functions — when set, write new baseline metrics after a successful run. Fail with a message if the current run has regressions. [spec: eval-harness/Baseline comparison and regression detection]

## 8. Test Functions

- [ ] 4.4 Implement `TestEvalSmoke` — single bugfix trial, ~5 min, fail on system-role errors or verification failure. [spec: eval-harness/Test functions]
- [ ] 4.5 Implement `TestEvalBenchGreenfield` — 5 greenfield trials with `ResetRepo()` between each, aggregate results, compare against baseline. [spec: eval-harness/Test functions]
- [ ] 4.6 Implement `TestEvalBenchBugfix` — 5 bugfix trials with `ResetRepo()` between each, aggregate results, compare against baseline. [spec: eval-harness/Test functions]
- [ ] 4.7 Add `-short` flag support — `TestEvalBenchGreenfield` and `TestEvalBenchBugfix` skip when `testing.Short()` is true. [spec: eval-harness/Test functions]

## 9. Output and Reporting

- [ ] 5.1 Implement JSON report output — write per-trial results to `internal/eval/output/YYYY-MM-DD-HHmmss.json`. Include trial number, scenario name, success/failure, verification checks, metrics delta, system-role error count, false error label count. [spec: eval-harness/Metrics collection]
- [ ] 5.2 Implement aggregate report — after N trials, compute median metrics, compare against baseline, print comparison diff to test log. [spec: eval-harness/Baseline comparison and regression detection]

## 10. Integration and Documentation

- [ ] 5.3 Run `go vet ./internal/eval/...` and fix all warnings. Verify `go test ./internal/eval/... -short -v` passes (short mode should skip benchmarks and run only unit tests for harness helpers). [spec: eval-harness/Test functions]
- [ ] 5.4 Update `docs/eval-harness.md` and `docs/eval-harness-local.md` to reference the OpenSpec change and add a "Running the Eval" section with concrete `go test` commands matching the implemented interface. [n/a — documentation]
- [ ] 5.5 Create initial baseline by running `TestEvalSmoke` against a working Forgejo+Fordjent instance. Commit `baseline.json` with the results. [spec: eval-harness/Baseline comparison and regression detection]