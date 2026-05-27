# Fordjent Eval Harness — Design & Architecture

## Purpose

A repeatable benchmark suite that measures Fordjent's end-to-end agent
performance with statistical confidence. The harness answers one question:
**did this change make Fordjent better or worse?**

Without this, every feature change is evaluated by anecdote ("seems faster").
With it, every change produces a before/after comparison of concrete metrics.

## Design Principles

1. **Local only.** Spins up its own Forgejo + Fordjent natively on macOS.
   No Docker, no cloud, no interference from other sessions.
2. **Self-contained.** A single `go test` command runs everything: start
   services, run benchmarks, collect metrics, tear down, produce report.
3. **Statistical.** Multiple trials per benchmark (N=10) with median reporting.
   Binary signals (pass/fail) provide instant feedback. Continuous signals
   (tokens, turns) need N≥3 for confidence.
4. **Fast enough to iterate.** ~2 minutes per trial for the bugfix scenario.
   Full 10-trial run: ~40 minutes on a local GPU.
5. **Regression-aware.** Baseline metrics are committed to the repo. Tests
   fail if metrics regress beyond tolerance thresholds.

## Architecture

```
go test ./internal/eval/... -v -run TestEvalBenchBugfix

  eval_test.go
    │
    ├── TestEvalSmoke          (N=1, ~2 min, pre-commit)
    ├── TestEvalBenchBugfix   (N=10, ~40 min, full benchmark)
    ├── TestEvalBenchGreenfield (N=5, ~30 min)
    │
    └── harness.go
          ├── startForgejo()        native macOS Forgejo process
          ├── startFordjent()       native macOS Fordjent process
          ├── setupRepo()           create repo, labels, webhook
          ├── createIssue()         seed issue via Forgejo API
          ├── waitForCompletion()   poll /status for session completion
          ├── verifyOutcome()       git clone + go test + diff check
          ├── collectMetrics()      parse Fordjent /status endpoint
          ├── analyzeSession()      parse Fordjent logs for turn data
          └── compareBaseline()     fail test on regression
```

## Metrics Collection

### Per-Trial Metrics

Each trial collects:

| Metric | Source | Description |
|--------|--------|-------------|
| `Success` | Verification | Strict pass/fail (tests pass + minimal diff) |
| `WallTime` | Clock | End-to-end time from issue creation |
| `TurnCount` | Fordjent log | Number of LLM turns |
| `ToolCallsTotal` | Fordjent log | Total tool calls |
| `ToolCallsByType` | Fordjent log | Per-tool breakdown (write_file, bash, git, forgejo_*) |
| `WriteFile` | Fordjent log | Number of write_file calls (code production) |
| `BashCommands` | Fordjent log | Number of bash tool calls (exploration) |
| `GitCommands` | Fordjent log | Number of git tool calls |
| `Metrics` | /status | Token counts, LLM calls, cost from SQLite |
| `SystemRoleErrors` | Log scan | Count of API 400 errors from system-role messages |
| `FalseErrorLabels` | Forgejo API | Count of fordjent/failed:error labels on successful PRs |

### Log-Based Analysis

Instead of parsing `memory.jsonl` (which is cleaned up after sessions), we
parse Fordjent's structured stdout log for `turn complete` messages:

```json
{"msg":"turn complete","session_key":"fjadmin/bench-0/issues/1","turn":5,
 "latency_ms":2842,"tokens_in":5976,"tokens_out":163,"tool_calls":2,
 "tools_used":{"bash":1,"read_file":1},"compacted":false}
```

This gives per-turn, per-session granularity without relying on ephemeral files.

### WaitForCompletion Logic

The harness polls `/status` every 5 seconds until either:
1. The issue state becomes "closed" (agent finished + PR merged)
2. No active sessions remain AND a session has been started (agent done)

The `sessionStarted` flag prevents false positives when Fordjent crashes on
startup — we only consider "no sessions" as complete if we've seen at least
one session start.

## Benchmark Scenarios

### Bugfix Scenario (`bench-bugfix`)

**What it tests**: Targeted bug fix — can the agent find and fix a specific
off-by-one error in a binary search implementation?

**Setup**: Repo seeded with a Go module containing a broken `BinarySearch`
function and its test file. The test passes for the happy path but fails
for edge cases (empty slice, single element, value not found).

**Seed files**:
- `go.mod` — module definition
- `.gitignore` — Go gitignore
- `pkg/search/search.go` — broken binary search (off-by-one in return)
- `pkg/search/search_test.go` — tests that expose the bug

**Issue**:
```
title: "[implementer] Binary search returns wrong index for edge cases"
body: |
  The BinarySearch function in pkg/search/search.go returns wrong results
  for several edge cases:
  
  1. Empty slice should return -1
  2. Single-element slice should find or return -1
  3. Value not in slice should return -1
  
  The current implementation has an off-by-one error. Fix it so all tests pass.
  Run `go test ./pkg/search/...` to verify.
```

**Verification** (3 checks):
1. `tests_pass` — `go test ./pkg/search/...` passes
2. `minimal_diff` — only files in `pkg/search/` were modified
3. `small_change` — fewer than 50 lines changed

### Greenfield Scenario (`bench-greenfield`)

**What it tests**: Full pipeline — scaffold detection, code generation from
scratch, test writing, build verification.

**Setup**: Empty repository seeded only with `go.mod` and `.gitignore`.

**Seed files**:
- `go.mod` — module definition
- `.gitignore` — Go gitignore

**Issue**:
```
title: "[implementer] Implement a Factorial function with CLI"
body: |
  Create a Go CLI tool that computes factorials.
  
  Structure:
  - cmd/factorial/main.go — CLI entry point
  - pkg/math/factorial.go — factorial implementation
  - pkg/math/factorial_test.go — tests
  
  The CLI should accept a number and print its factorial.
  Include tests for edge cases (0!, 1!, negative numbers).
```

**Verification** (3 checks):
1. `build_passes` — `go build ./...` succeeds
2. `tests_pass` — `go test ./...` passes  
3. `correct_output` — `echo 5 | go run cmd/factorial/main.go` outputs 120

## Running Benchmarks

```bash
# Environment variables
export EVAL_PROVIDER_URL="http://your-llm-server:8181/v1"
export EVAL_PROVIDER_MODEL="qwen3.6-35b"  # or any OpenAI-compatible model
export EVAL_PROVIDER_API_KEY=""            # if needed

# Quick smoke test (single trial, ~2 min)
go test ./internal/eval/... -v -run TestEvalSmoke -timeout 20m

# Full bugfix benchmark (10 trials, ~40 min)
go test ./internal/eval/... -v -run TestEvalBenchBugfix -timeout 180m

# Greenfield benchmark (5 trials, ~30 min)
go test ./internal/eval/... -v -run TestEvalBenchGreenfield -timeout 90m

# Keep services running after test (for log inspection)
EVAL_SKIP_TEARDOWN=true go test ./internal/eval/... -v -run TestEvalSmoke

# Update baseline after deliberate improvements
UPDATE_BASELINE=1 go test ./internal/eval/... -v -run TestEvalSmoke

# Short mode (skip benchmarks, only smoke tests)
go test ./internal/eval/... -v -short -run TestEvalBenchBugfix
```

## Verification Logic

The `BugfixVerify` function:

```go
func BugfixVerify(cloneDir string) VerificationResult {
    // 1. Run go test
    cmd := exec.Command("go", "test", "./pkg/search/...", "-count=1")
    // ... check pass/fail
    
    // 2. Git diff --name-only (only pkg/search/ should change)
    cmd = exec.Command("git", "diff", "--name-only", "main")
    // ... check each file starts with "pkg/search/"
    
    // 3. Git diff --stat (fewer than 50 lines changed)
    cmd = exec.Command("git", "diff", "--stat", "main")
    // ... count total lines
}
```

A trial passes all 3 checks for a `Success = true`. If the bug is fixed
but extraneous files are modified (e.g., go.mod), `minimal_diff` fails.

## Baseline Comparison

Baseline metrics are stored in `baseline.json` (committed). After a full
benchmark run, the harness compares against baseline:

- **Regressions**: Any metric that got worse (more turns, more tokens, lower pass rate)
- **Warnings**: Non-critical regressions within 20% tolerance
- **Improvements**: Any metric that got better

```bash
# Update baseline after deliberate improvements
UPDATE_BASELINE=1 go test ./internal/eval/... -v -run TestEvalBenchBugfix
```

## Provider Configuration

The harness supports three LLM provider configurations:

1. **Custom endpoint** (default): Set `EVAL_PROVIDER_URL`, `EVAL_PROVIDER_MODEL`,
   and `EVAL_PROVIDER_API_KEY` environment variables. All roles use the same
   model: `"eval-provider"`.

2. **Wafer (legacy)**: Hardcoded in `fordjent.yaml` — not recommended, returns 401.

3. **Scaleway AI**: Set provider URL to `https://api.scaleway.ai/v1` with
   appropriate API key. Requires system-role compatibility (Fordjent uses
   `role: "user"` for all injected messages to avoid Scaleway's strict API).

## Package Structure

```
internal/eval/
  ├── harness.go          Setup, teardown, repo creation, issue management
  ├── forgejo.go          Forgejo API client (create repo, seed files, labels)
  ├── metrics.go          /status parsing, session analysis, baseline comparison
  ├── verify.go           BugfixVerify, GreenfieldVerify (go test, git diff)
  ├── scenarios.go         Scenario struct, common fields
  ├── scenarios_bugfix.go  Bugfix seed files, issue text, verify function
  ├── scenarios_greenfield.go  Greenfield seed files, issue text, verify function
  ├── baseline.go          Baseline loading, comparison, regression detection
  ├── baseline.json         Committed baseline metrics
  └── eval_test.go          Test functions, benchmark runner, report generation
```

## Report Format

Reports are written to `internal/eval/output/YYYY-MM-DD-HHMMSS-{scenario}.json`:

```json
[
  {
    "TrialNum": 1,
    "Scenario": "bugfix",
    "Success": true,
    "WallTime": 72000000000,
    "Verification": {
      "Name": "bugfix",
      "Passed": true,
      "Checks": [
        {"Name": "tests_pass", "Passed": true},
        {"Name": "minimal_diff", "Passed": true},
        {"Name": "small_change", "Passed": true}
      ],
      "Errors": null
    },
    "TurnCount": 17,
    "ToolCallsTotal": 19,
    "ToolCallsByType": {"bash": 8, "read_file": 5, "write_file": 4, "git": 2},
    "SystemRoleErrors": 0,
    "FalseErrorLabels": 0
  }
]
```