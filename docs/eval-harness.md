# Fordjent Eval Harness — Design Spec

## Purpose

A repeatable, local-Docker-based benchmark suite that measures Fordjent's
end-to-end agent performance with statistical confidence. The harness answers
one question: **did this change make Fordjent better or worse?**

Without this, every feature change is evaluated by anecdote ("seems faster").
With it, every change produces a before/after comparison of concrete metrics.

## Design Principles

1. **Local only.** Spins up its own Forgejo + Fordjent in Docker. No cloud
   dependency, no interference from other sessions, no network variance.
2. **Self-contained.** A single `go test` command runs everything: start
   services, run benchmarks, tear down, produce report.
3. **Statistical.** Multiple trials per benchmark (N=5) with median reporting.
   Binary signals (system-role errors) provide instant feedback. Continuous
   signals (tokens, turns) need N≥3 for confidence.
4. **Fast enough to iterate.** Target: ~30 minutes for a full run. Fast-path
   mode (N=1, single scenario): ~5 minutes for pre-commit smoke testing.
5. **Regression-aware.** Baseline metrics are committed to the repo. Tests
   fail if metrics regress beyond tolerance thresholds.

## Architecture

```
go test ./internal/eval/... -v

  eval_test.go
    │
    ├── TestGreenfieldCLI        (N=5 × ~6 min = 30 min)
    ├── TestMaintenanceBugFix    (N=5 × ~3 min = 15 min)
    ├── TestSmoke                 (N=1 × ~5 min = 5 min, pre-commit)
    │
    └── harness.go
          ├── dockerLifecycle()        docker compose up/down
          ├── forgejoSetup()           create repo, labels, users, webhook
          ├── runTrial()              seed → issue → poll → record → verify → reset
          ├── collectMetrics()         from /status endpoint + git verification
          └── compareBaseline()        fail test on regression
```

## Benchmark Scenarios

### Scenario A: Greenfield CLI (`bench-greenfield`)

**What it tests**: Full pipeline — scaffold detection, PM decomposition,
implementer code generation, reviewer validation, milestone tracking.

**Setup**: Empty repository seeded with `go.mod`, `.gitignore`, `README.md`.

**Issue**:
```
title: "[pm] Build a string utility CLI with reverse and wordcount commands"

body: |
  ## Project

  Build a Go CLI tool `stringutil` with two subcommands.

  ### Commands
  - `reverse` — reverses the input string
  - `wordcount` — counts words in the input string

  ### Structure
  - cmd/stringutil/main.go — CLI entry point (cobra or flag-based)
  - pkg/stringutil/reverse.go — reverse implementation
  - pkg/stringutil/wordcount.go — word count implementation
  - pkg/stringutil/reverse_test.go — tests
  - pkg/stringutil/wordcount_test.go — tests

  ### Please
  1. Decompose into sub-issues with [implementer] and [tester] tags
  2. Create milestone, attach sub-issues
  3. Use Depends on: #N
```

**Expected outcome**: 
- 5+ sub-issues created with correct role tags
- Milestone created
- All sub-issues resolved
- Code on main: `cmd/stringutil/main.go`, `pkg/stringutil/reverse.go`,
  `pkg/stringutil/wordcount.go`, `*_test.go`
- `go build ./...` passes, `go test ./...` passes
- Manual verification: `go run ./cmd/stringutil reverse "hello"` → "olleh"
- Manual verification: `go run ./cmd/stringutil wordcount "hello world"` → "2"

**Expected duration**: 3-8 minutes per trial (PM ~1 min, 2+ implementer
sessions ~2 min each, reviewer ~1 min).

**Failure modes it catches**:
- PM doesn't include role tags → sub-issues unprocessed
- PM doesn't create milestone → no progress tracking
- Implementer pushes to main instead of feature branch → protected branch violation
- Implementer creates code that doesn't compile → build gate failure
- Reviewer doesn't validate → broken code on main
- False `fordjent/failed:error` labels → lifecycle accuracy
- Scaffold detection incorrectly blocks → PM can't start

### Scenario B: Maintenance Bug Fix (`bench-bugfix`)

**What it tests**: Agent's ability to read existing code, understand a bug from
test output, make a minimal fix, and not break other behavior. This is the
hardest real-world scenario — most developer time is maintenance, not greenfield.

**Setup**: Repository pre-seeded with a working Go package that has a deliberate
bug. The bug is an off-by-one error in a binary search implementation. One test
fails (`TestFindLastElement`) while all other tests pass.

**Repo structure**:
```
go.mod
pkg/search/
  search.go       # binary search with off-by-one bug
  search_test.go  # 5 tests, 1 fails (TestFindLastElement)
```

**Known bug** (in `search.go`):
```go
// Bug: mid calculation overflows for large slices,
// and loop condition misses the last element.
func BinarySearch(arr []int, target int) int {
    low, high := 0, len(arr)-1
    for low < high {           // BUG: should be low <= high
        mid := (low + high) / 2 // BUG: overflow risk
        if arr[mid] < target {
            low = mid + 1
        } else {
            high = mid
        }
    }
    if arr[low] == target {    // index out of range when arr is empty
        return low
    }
    return -1
}
```

**Known fix**:
```go
func BinarySearch(arr []int, target int) int {
    low, high := 0, len(arr)-1
    for low <= high {
        mid := low + (high-low)/2
        if arr[mid] < target {
            low = mid + 1
        } else if arr[mid] > target {
            high = mid - 1
        } else {
            return mid
        }
    }
    return -1
}
```

**Issue**:
```
title: "[implementer] Binary search returns wrong index for edge cases"

body: |
  ## Bug

  The `BinarySearch` function in `pkg/search/search.go` fails
  `TestFindLastElement` and may overflow for large slices.

  ### Steps to reproduce
  Run `go test ./pkg/search/...` — TestFindLastElement fails.

  ### Expected behavior
  - BinarySearch should correctly find elements at all positions
  - Should handle empty slices
  - Should handle single-element slices
  - Should not overflow for large (10^6+) slices
```

**Expected outcome**:
- Single PR created with the fix
- `go test ./pkg/search/...` passes (all 5 tests)
- Fix is minimal (< 10 lines changed in `search.go`)
- No other files modified
- No `fordjent/failed:error` label

**Expected duration**: 2-5 minutes per trial.

**Failure modes it catches**:
- Agent over-edits (rewrites entire file instead of fixing bug)
- Agent misses root cause (changes test instead of code)
- Agent breaks other tests while fixing one
- Agent can't read existing code effectively
- Agent pushes to wrong branch
- Agent creates duplicate PRs

## Metrics

All metrics are recorded per-trial and aggregated across N trials.

### Primary Metrics (decide pass/fail)

| Metric | Type | Pass Condition | Rationale |
|--------|------|----------------|-----------|
| **task_success** | binary | Expected code on main, tests pass | The only thing that matters |
| **system_role_errors** | count | Must be zero | Binary: any occurrence = regression |
| **false_error_labels** | binary | `fordjent/failed:error` absent on successful sessions | Binary: present = lifecycle bug |

### Efficiency Metrics (decide "better/worse" magnitude)

| Metric | Type | How to Read |
|--------|------|-------------|
| **total_tokens** | count | Lower = more efficient. Median across successful trials. |
| **total_turns** | count | Lower = more focused agent. Tracks loop prevention. |
| **wall_time_s** | duration | Lower = faster delivery. Median across trials. |
| **tool_calls_write_file** | count | Higher ratio to read_file = more productive agent |
| **tool_calls_bash** | count | Lower = less flailing |
| **tool_calls_total** | count | Lower = more efficient tool use |

### Behavioral Metrics (diagnostic, not pass/fail)

| Metric | Type | How to Read |
|--------|------|-------------|
| **pm_sub_issues_count** | count | Did PM decompose properly? |
| **pm_role_tags_correct** | binary | Did sub-issues have [implementer]/[tester] tags? |
| **milestone_progress** | % | Did milestone track progress? |
| **commit_count** | count | How many commits per trial? |
| **pr_count** | count | How many PRs? Duplicates = problem |
| **recovery_count** | count | Auto-retry attempts per trial |

## Baseline & Regression Detection

### Baseline File

`internal/eval/baseline.json` — committed to the repo, updated only when
a change is confirmed to be an improvement.

```json
{
  "commit": "abc1234",
  "timestamp": "2026-05-26T12:00:00Z",
  "scenarios": {
    "greenfield": {
      "trials": 5,
      "pass_rate": "4/5",
      "metrics": {
        "total_tokens_median": 180000,
        "total_turns_median": 35,
        "wall_time_s_median": 280,
        "system_role_errors": 0,
        "false_error_rate": 0.0
      }
    },
    "bugfix": {
      "trials": 5,
      "pass_rate": "5/5",
      "metrics": {
        "total_tokens_median": 45000,
        "total_turns_median": 8,
        "wall_time_s_median": 45,
        "system_role_errors": 0,
        "false_error_rate": 0.0
      }
    }
  }
}
```

### Regression Thresholds

Tests fail when any of these occur:

| Condition | Severity | Action |
|-----------|----------|--------|
| `pass_rate` drops (e.g., 4/5 → 2/5) | **FAIL** | Change is harmful |
| `system_role_errors > 0` | **FAIL** | Infrastructure regression |
| `false_error_rate > 0.2` (i.e., >1 in 5) | **FAIL** | Lifecycle regression |
| `total_tokens_median > baseline × 1.5` | **WARN** | Efficiency regression — investigate |
| `total_turns_median > baseline × 1.5` | **WARN** | Agent loop regression — investigate |
| `wall_time_s_median > baseline × 2.0` | **WARN** | Performance regression — investigate |

Warnings print in test output but don't fail the test. They're for human review.

### Updating Baseline

After a confirmed improvement (e.g., tokens drop from 180K to 120K with same
pass rate), update baseline:

```bash
go test ./internal/eval/... -v -update-baseline
```

This writes new metrics to `baseline.json` but requires the test to pass first.
You can't update baseline if the test is failing — this prevents masking
regressions.

## Smoke Test (Fast Path)

`TestSmoke` runs Scenario B (bugfix) with N=1. Designed for pre-commit hooks:

```bash
go test ./internal/eval/... -v -run TestSmoke -timeout 15m
```

Catches catastrophic regressions only:
- System-role errors
- Complete failure to produce a fix
- False error labels

Does NOT measure efficiency changes (N=1 has no statistical power for
continuous metrics). Full N=5 run is for pre-merge CI or nightly.

## Implementation

### Package Structure

```
internal/eval/
├── eval_test.go         # TestGreenfieldCLI, TestMaintenanceBugFix, TestSmoke
├── harness.go           # Docker lifecycle, Forgejo setup, trial execution
├── metrics.go           # Metric collection and aggregation
├── baseline.go          # Baseline loading, comparison, regression detection
├── baseline.json        # Committed baseline metrics
├── scenarios/
│   ├── greenfield/
│   │   ├── seed.sh      # Creates the seeded repo
│   │   ├── issue.json   # Issue template (title + body)
│   │   └── verify.go    # Verification script (build, test, run)
│   └── bugfix/
│       ├── repo/        # Full repo with buggy code (committed as tar or dir)
│       ├── issue.json   # Issue template
│       └── verify.go    # Verification: test pass, diff < 10 lines, no other files
└── output/              # .gitignored — per-run JSON reports
```

### Key APIs Used

| Component | API | Purpose |
|-----------|-----|---------|
| Docker | `docker compose -f docker-compose.local.yaml up -d` | Start Forgejo+Fordjent |
| Forgejo | `POST /api/v1/user/repos` | Create benchmark repo |
| Forgejo | `POST /api/v1/repos/{repo}/contents` | Seed files |
| Forgejo | `POST /api/v1/repos/{repo}/labels` | Create FSM + role labels |
| Forgejo | `POST /api/v1/repos/{repo}/hooks` | Create webhook |
| Forgejo | `POST /api/v1/repos/{repo}/issues` | Create benchmark issue |
| Forgejo | `GET /api/v1/repos/{repo}/issues` | Poll for sub-issue creation |
| Forgejo | `GET /api/v1/repos/{repo}/pulls` | Poll for PR creation |
| Fordjent | `GET /status` | Poll for session completion, collect metrics |
| Fordjent | `GET /trace/{owner}/{repo}/issues/{N}` | Collect per-turn trace data |
| Git | `git clone` / `git log` / `git diff` | Verify outcomes |

### Docker Lifecycle

```go
func setupDocker() (cleanup func()) {
    // Start Forgejo + Fordjent with fresh volumes
    exec("docker", "compose", "-f", "docker-compose.local.yaml",
         "down", "-v")  // clean slate
    exec("docker", "compose", "-f", "docker-compose.local.yaml",
         "up", "-d", "--wait")

    // Wait for health
    waitForHTTP("http://localhost:3000/api/v1/version", 30*time.Second)
    waitForHTTP("http://localhost:8080/health", 30*time.Second)

    return func() {
        exec("docker", "compose", "-f", "docker-compose.local.yaml",
             "down", "-v")
    }
}
```

### Trial Execution

```go
func runTrial(repo string, issue Issue, verify VerifyFunc) TrialResult {
    // 1. Reset repo state (delete all issues, PRs, branches)
    resetRepo(repo)

    // 2. Seed repo with benchmark files
    seedRepo(repo, scenarioFiles)

    // 3. Create issue via API
    issueNum := createIssue(repo, issue)

    // 4. Poll /status until session completes or timeout
    start := time.Now()
    result := pollUntilComplete(repo, issueNum, 15*time.Minute)

    // 5. Verify outcome
    result.Success = verify(repo)
    result.WallTime = time.Since(start)
    result.SystemRoleErrors = countLogErrors("Unexpected role")

    // 6. Collect metrics from /status
    result.Metrics = collectMetrics(repo, issueNum)

    return result
}
```

### Polling Strategy

The `/status` endpoint exposes `active_sessions` and `failed_sessions` counts,
plus per-model cost data. For polling:

1. After creating an issue, wait for a session to appear in active count
2. Wait for active count to drop (session ended)
3. Check: did failed count increase? → failure. Else → success.
4. Collect final metrics from `/status` and `/trace/{issue}` endpoints

Max wait: 15 minutes per trial. If timeout, count as failure.

### Verification Functions

Each scenario has a `verify` function that checks correctness:

**Greenfield**:
```go
func verifyGreenfield(repoDir string) bool {
    // Clone the repo
    // Run `go build ./...`
    // Run `go test ./...`
    // Run `go run ./cmd/stringutil reverse "hello"` → "olleh"
    // Run `go run ./cmd/stringutil wordcount "hello world"` → "2"
    // Check that milestone is 100%
    // Check that zero issues have fordjent/failed:error
    // Check that expected files exist on main
    return allPassed
}
```

**Bugfix**:
```go
func verifyBugfix(repoDir string) bool {
    // Clone the repo
    // Run `go test ./pkg/search/...` (must pass)
    // Check diff: only search.go changed, < 10 lines of diff
    // Check no fordjent/failed:error label on issue
    return allPassed
}
```

## Output

### Per-Run Report

`internal/eval/output/YYYY-MM-DD-HHmmss.json`:

```json
{
  "commit": "12dbe0d",
  "timestamp": "2026-05-26T14:30:00Z",
  "scenarios": {
    "greenfield": {
      "trials": 5,
      "passes": 4,
      "results": [
        {"success": true,  "tokens": 165000, "turns": 32, "time_s": 260, ...},
        {"success": true,  "tokens": 192000, "turns": 38, "time_s": 310, ...},
        {"success": false, "tokens": 220000, "turns": 50, "time_s": 420, "error": "max_turns"},
        {"success": true,  "tokens": 178000, "turns": 34, "time_s": 275, ...},
        {"success": true,  "tokens": 185000, "turns": 36, "time_s": 290, ...}
      ],
      "median": {"tokens": 182000, "turns": 35, "time_s": 283},
      "pass_rate": "4/5",
      "system_role_errors": 0,
      "false_error_rate": 0.0
    },
    "bugfix": {
      "trials": 5,
      "passes": 5,
      ...
    }
  },
  "regression_check": {
    "passed": true,
    "warnings": []
  }
}
```

### Comparison Output

When run against a different commit, the test prints a diff:

```
=== Fordjent Eval Harness ===

Scenario: greenfield
  pass_rate:   4/5 → 4/5  (no change)
  tokens:      182K → 145K (-20%)  ← improvement
  turns:       35 → 28 (-20%)      ← improvement
  time:        283s → 230s (-19%)  ← improvement
  sys_errors:  0 → 0              (no change)
  false_err:   0.0 → 0.0           (no change)

Scenario: bugfix
  pass_rate:   5/5 → 5/5  (no change)
  tokens:      45K → 44K  (no change)
  turns:       8 → 8      (no change)
  time:        45s → 42s  (no change)

✅ All regression checks passed
⚠ Warnings: none
```

## Usage

### Pre-Commit (fast)

```bash
go test ./internal/eval/... -v -run TestSmoke -timeout 15m
```
~5 minutes. Catches catastrophic regressions only.

### Pre-Merge CI (full)

```bash
go test ./internal/eval/... -v -timeout 60m
```
~45 minutes. Full statistical comparison against baseline.

### Nightly (baseline update)

```bash
go test ./internal/eval/... -v -timeout 60m -update-baseline
```
Full run + update baseline if all tests pass.

### Manual Comparison

```bash
# Run on commit A
git checkout commit-a
go test ./internal/eval/... -v -timeout 60m

# Run on commit B  
git checkout commit-b
go test ./internal/eval/... -v -timeout 60m

# Compare reports
go run ./cmd/fordjent-eval compare output/2026-05-26-*.json
```

## What This Harness Will Tell Us

### Clear Signals (detectable at N=5)

| Change | Expected Metric Shift |
|--------|----------------------|
| System-role fix | `system_role_errors`: N → 0 |
| False-error fix | `false_error_rate`: 0.6 → 0.0 |
| Steering system | `turns_median`: 40 → 25 |
| Algorithmic compaction | `tokens_median`: 180K → 130K |
| k-sample boosting | `pass_rate`: 3/5 → 5/5 |
| PM role-tag enforcement | `pm_role_tags_correct`: 0.5 → 1.0 |
| Model switch (devstral→qwen) | `turns_median` spikes, `tool_calls_bash` spikes |
| Critic gate (build/test before PR) | `false_error_rate` drops, `tool_calls_total` drops |

### Ambiguous Signals (need investigation)

| Metric Shift | Possible Causes |
|--------------|----------------|
| Tokens up, turns down | Agent is writing more per turn (longer files) — might be better or worse |
| Tokens down, pass rate down | Agent is being too concise and missing requirements |
| Time up, tokens same | Network latency or model serving slowdown — not a Fordjent issue |

## Extensions (Future)

After the core harness works:

1. **k-sample boosting benchmark**: Run N parallel implementers for the same
   issue, measure oracle-gap recovery. Requires infrastructure for parallel
   sessions.

2. **Cross-session memory benchmark**: Two sequential issues where the second
   depends on knowledge from the first. Measures whether the agent "remembers."

3. **Model comparison mode**: Run identical benchmarks against different
   provider/model configs. Outputs a leaderboard.

4. **Stress test**: Fire N simultaneous issues at an empty repo. Measures
   merge-conflict handling and concurrency behavior.

5. **Longevity test**: A single long-running repo with 10+ sequential issues.
   Measures context accumulation, technical debt management, regression rate.

## Deliverables

| File | Purpose |
|------|---------|
| `docs/eval-harness.md` | This spec |
| `internal/eval/harness.go` | Docker lifecycle, Forgejo setup, trial execution |
| `internal/eval/metrics.go` | Metric collection from /status + git verification |
| `internal/eval/baseline.go` | Baseline loading, comparison, regression detection |
| `internal/eval/baseline.json` | Committed baseline metrics |
| `internal/eval/eval_test.go` | `TestGreenfieldCLI`, `TestMaintenanceBugFix`, `TestSmoke` |
| `internal/eval/scenarios/greenfield/` | Seed script, issue template, verification script |
| `internal/eval/scenarios/bugfix/repo/` | Buggy repo template (tar or directory) |
| `internal/eval/output/` | .gitignored per-run JSON reports |
| `docker-compose.local.yaml` | Already exists — local Forgejo+Fordjent stack |
| `scripts/bootstrap-local.sh` | Already exists — may need adaptation for eval harness |
