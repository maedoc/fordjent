# Fordjent Eval Harness — Local Testing Spec

This document specifies how the eval harness runs on a developer's macOS
machine. It complements `docs/eval-harness.md` (the design spec) with concrete
implementation details: code structure, Forgejo API calls, polling logic,
scenario definitions, and verification.

## Overview

```
go test ./internal/eval/... -v -run TestEvalBenchGreenfield
go test ./internal/eval/... -v -run TestEvalBenchBugfix
go test ./internal/eval/... -v -run TestEvalSmoke           # 5 min fast path
```

The harness is a Go test suite that:
1. Starts Forgejo + Fordjent locally (native macOS, using `sandbox-exec`)
2. Sets up a benchmark repo with known seed content
3. Creates a test issue and waits for the agent to finish
4. Verifies the outcome against expected results
5. Collects metrics and compares against baseline
6. Tears everything down

No Docker. No cloud. Native macOS processes. This gives us:
- Full control over the environment (no container networking)
- Fast startup (~5s for Forgejo, ~2s for Fordjent)
- Direct filesystem access for verification
- No LLM provider variance (same provider for all trials)

## Prerequisites

The `scripts/bootstrap-local.sh` already does 90% of the setup. The eval
harness uses the same mechanisms but wraps them in Go test functions.

Required on the host:
- Go 1.22+
- Forgejo installed via Homebrew (`brew install forgejo`)
- `sandbox-exec` (macOS built-in)
- LLM provider API key (Wafer or Scaleway) in environment

The harness will:
- Use `os/exec` to run `forgejo web` and `fordjent` under `sandbox-exec`
- Use the Forgejo API (`http://127.0.0.1:3000/api/v1/...`) for repo+issue setup
- Use the Fordjent status API (`http://127.0.0.1:8080/status`) for metrics
- Use `git clone` + `go build` + `go test` for verification

## Package Structure

```
internal/eval/
├── eval_test.go        # Test functions (TestEvalSmoke, TestEvalBenchGreenfield, etc.)
├── harness.go          # Harness: start services, create repo, poll, teardown
├── forgejo.go          # Forgejo API helpers: create repo, seed files, create labels, etc.
├── verify.go           # Verification: git clone, go build, go test, diff analysis
├── metrics.go          # Metric collection from /status, /trace, /activity APIs
├── baseline.go         # Load/save/compare baseline.json
├── baseline.json       # Committed baseline metrics (updated on --update-baseline)
├── scenarios.go         # Scenario definitions (issue templates, seed content, verify funcs)
├── scenarios_greenfield.go  # Greenfield CLI scenario
└── scenarios_bugfix.go      # Bugfix scenario
```

## Harness Lifecycle

### 1. Start Services

```go
// harness.go

type Harness struct {
    ForgejoURL    string // http://127.0.0.1:3000
    FordjentURL   string // http://127.0.0.1:8080
    ForgejoToken  string // bot token
    AdminToken    string // admin token
    AdminUser     string // fjadmin
    AdminPass     string // generated
    WebhookSecret string // generated
    LocalDir      string // ~/fordjent-eval-XXXX
    ForgejoPID    int
    FordjentPID   int
    Client        *forgejo.Client
}

func NewHarness(t *testing.T) *Harness {
    h := &Harness{...}

    // 1. Generate random workdir
    h.LocalDir = filepath.Join(os.TempDir(), "fordjent-eval-"+randSuffix())

    // 2. Write app.ini, fordjent.yaml, sandbox profiles
    //    (same as bootstrap-local.sh, but in Go)

    // 3. Start Forgejo (sandbox-exec -f forgejo.sb forgejo web ...)
    h.startForgejo()

    // 4. Create admin user + tokens (forgejo admin user create ...)
    h.createAdmin()

    // 5. Start Fordjent (sandbox-exec -f fordjent.sb fordjent -config ...)
    h.startFordjent()

    // 6. Wait for health
    h.waitForHealthy()

    return h
}

func (h *Harness) TearDown() {
    // Kill processes, remove workdir
    os.Kill(h.ForgejoPID)
    os.Kill(h.FordjentPID)
    os.RemoveAll(h.LocalDir)
}
```

### 2. Create Benchmark Repo

```go
// forgejo.go

func (h *Harness) CreateRepo(name string) (*RepoInfo, error) {
    // POST /api/v1/user/repos → create repo
    // POST .../contents/go.mod → seed file
    // POST .../contents/.gitignore → seed file
    // POST .../labels → create FSM + role labels
    // POST .../hooks → register webhook to Fordjent
    // POST .../collaborators/djent-pm → add role users (if configured)
    return repo, nil
}

func (h *Harness) SeedFiles(repo string, files map[string]string) error {
    // For each file: POST /api/v1/repos/{repo}/contents/{path}
    // files is a map: {"go.mod": "module testbed\n\ngo 1.26"}
}

func (h *Harness) CreateIssue(repo, title, body string) (int, error) {
    // POST /api/v1/repos/{repo}/issues
    return issueNumber, nil
}

func (h *Harness) WaitForIssueState(repo string, issueNum int, state string, timeout time.Duration) error {
    // Poll GET /api/v1/repos/{repo}/issues/{num} until state matches
}

func (h *Harness) GetPrList(repo string) ([]PR, error) {
    // GET /api/v1/repos/{repo}/pulls?state=all
}
```

### 3. Poll for Completion

The tricky part is detecting when Fordjent has finished processing.

```go
// harness.go

func (h *Harness) WaitForCompletion(repo string, issueNum int, timeout time.Duration) (*TrialResult, error) {
    deadline := time.Now().Add(timeout)
    sessionKey := fmt.Sprintf("%s/issues/%d", repo, issueNum)

    for time.Now().Before(deadline) {
        // 1. Check /status for lifecycle state
        status := h.FetchStatus()
        activeCount := status.Lifecycle.ActiveSessions
        failedCount := status.Lifecycle.FailedSessions

        // 2. Check issue state in Forgejo
        issue := h.GetIssue(repo, issueNum)
        if issue.State == "closed" {
            return h.collectResult(repo, issueNum), nil
        }

        // 3. Check if any PRs exist and are merged
        prs := h.GetPrList(repo)
        for _, pr := range prs {
            if pr.Merged {
                return h.collectResult(repo, issueNum), nil
            }
        }

        // 4. Check lifecycle DB for session completion
        transitions := h.GetTransitions(sessionKey)
        lastTransition := transitions[len(transitions)-1]
        if lastTransition.To == "completed" || lastTransition.To == "failed_max_turns" || lastTransition.To == "failed_error" {
            return h.collectResult(repo, issueNum), nil
        }

        time.Sleep(5 * time.Second)
    }

    return nil, fmt.Errorf("timeout waiting for completion")
}
```

Completion detection strategy (in priority order):
1. Issue closed in Forgejo → success
2. PR merged on main → success
3. Lifecycle state = "completed" → success
4. Lifecycle state = "failed_max_turns" → failure (but record metrics)
5. Lifecycle state = "failed_error" → failure (but record metrics)
6. Timeout (15 min default) → hard failure

### 4. Reset Between Trials

```go
func (h *Harness) ResetRepo(repo string) error {
    // 1. Delete all issues (admin API)
    //    GET /api/v1/repos/{repo}/issues → list
    //    PATCH /api/v1/repos/{repo}/issues/{num} {"state": "closed"} for each

    // 2. Delete all PRs (close them)
    //    GET /api/v1/repos/{repo}/pulls?state=all → list
    //    PATCH /api/v1/repos/{repo}/pulls/{num} {"state": "closed"} for each

    // 3. Reset main branch to seed state
    //    git clone → git reset --hard <seed-commit> → git push -f

    // 4. Delete feature branches
    //    git branch -r | grep -v main → delete remote branches

    // 5. Wait for Fordjent to settle (no active sessions)
    h.WaitForIdle(30 * time.Second)

    return nil
}
```

## Scenario Definitions

### Greenfield CLI

```go
// scenarios_greenfield.go

var GreenfieldScenario = Scenario{
    Name:        "greenfield",
    Description: "Build a stringutil CLI from empty repo",
    RepoName:    "bench-greenfield",
    IssueTitle:   "[pm] Build a string utility CLI with reverse and wordcount commands",
    IssueBody: `## Project

Build a Go CLI tool called 'stringutil' with two subcommands.

### Commands
- reverse — reverses the input string
- wordcount — counts words in the input string

### Structure
- cmd/stringutil/main.go — CLI entry point
- pkg/stringutil/reverse.go — reverse implementation
- pkg/stringutil/wordcount.go — word count implementation
- pkg/stringutil/reverse_test.go — tests
- pkg/stringutil/wordcount_test.go — tests

### Please
1. Decompose into sub-issues with [implementer] and [tester] tags
2. Create milestone, attach sub-issues
3. Use Depends on: #N for dependencies`,
    SeedFiles: map[string]string{
        "go.mod":     "module bench-greenfield\n\ngo 1.26",
        ".gitignore": "*.o\n*.exe\nstringutil\n",
        "README.md":  "# bench-greenfield\n\nA string utility CLI.\n",
    },
    Verify: GreenfieldVerify,
    Timeout: 15 * time.Minute,
}

func GreenfieldVerify(repoDir string) VerificationResult {
    result := VerificationResult{Name: "greenfield"}

    // 1. go build ./...
    if err := runCommand(repoDir, "go", "build", "./..."); err != nil {
        result.Errors = append(result.Errors, "go build failed: "+err.Error())
        result.Passed = false
        return result
    }
    result.Checks = append(result.Checks, Check{Name: "build", Passed: true})

    // 2. go test ./...
    if err := runCommand(repoDir, "go", "test", "./..."); err != nil {
        result.Errors = append(result.Errors, "go test failed: "+err.Error())
        result.Passed = false
        return result
    }
    result.Checks = append(result.Checks, Check{Name: "test", Passed: true})

    // 3. Functional test: stringutil reverse "hello"
    out, err := runCommandOutput(repoDir, "go", "run", "./cmd/stringutil", "reverse", "hello")
    if err != nil {
        result.Errors = append(result.Errors, "reverse failed: "+err.Error())
        result.Passed = false
        return result
    }
    if strings.TrimSpace(out) != "olleh" {
        result.Errors = append(result.Errors, fmt.Sprintf("reverse: expected 'olleh', got '%s'", out))
        result.Passed = false
    }
    result.Checks = append(result.Checks, Check{Name: "reverse_hello", Passed: strings.TrimSpace(out) == "olleh"})

    // 4. Functional test: stringutil wordcount "hello world"
    out, err = runCommandOutput(repoDir, "go", "run", "./cmd/stringutil", "wordcount", "hello world")
    if err != nil {
        result.Errors = append(result.Errors, "wordcount failed: "+err.Error())
        result.Passed = false
        return result
    }
    if strings.TrimSpace(out) != "2" {
        result.Errors = append(result.Errors, fmt.Sprintf("wordcount: expected '2', got '%s'", out))
        result.Passed = false
    }
    result.Checks = append(result.Checks, Check{Name: "wordcount_hello_world", Passed: strings.TrimSpace(out) == "2"})

    // 5. Check expected files exist
    requiredFiles := []string{
        "cmd/stringutil/main.go",
        "pkg/stringutil/reverse.go",
        "pkg/stringutil/wordcount.go",
    }
    for _, f := range requiredFiles {
        if _, err := os.Stat(filepath.Join(repoDir, f)); err != nil {
            result.Errors = append(result.Errors, "missing file: "+f)
            result.Passed = false
        }
    }
    result.Checks = append(result.Checks, Check{Name: "files_exist", Passed: len(result.Errors) == 0})

    result.Passed = len(result.Errors) == 0
    return result
}
```

### Bugfix Scenario

```go
// scenarios_bugfix.go

var BugfixScenario = Scenario{
    Name:        "bugfix",
    Description: "Fix off-by-one error in binary search",
    RepoName:    "bench-bugfix",
    IssueTitle:   "[implementer] Binary search returns wrong index for edge cases",
    IssueBody: `## Bug

The BinarySearch function in pkg/search/search.go fails TestFindLastElement.

### Steps to reproduce
Run go test ./pkg/search/... — TestFindLastElement fails.

### Expected behavior
- BinarySearch should correctly find elements at all positions
- Should handle empty slices
- Should handle single-element slices
- Should not overflow for large slices`,
    SeedFiles: map[string]string{
        "go.mod": "module bench-bugfix\n\ngo 1.26",
        ".gitignore": "*.o\n*.exe\nbenchbug\n",
        "pkg/search/search.go": bugfixBuggyCode,
        "pkg/search/search_test.go": bugfixTestCode,
    },
    Verify: BugfixVerify,
    Timeout: 10 * time.Minute,
}

const bugfixBuggyCode = `package search

// BinarySearch returns the index of target in sorted arr, or -1 if not found.
// BUG: loop condition should be low <= high, and mid should use
// overflow-safe calculation low + (high-low)/2.
func BinarySearch(arr []int, target int) int {
    low, high := 0, len(arr)-1
    for low < high {
        mid := (low + high) / 2
        if arr[mid] < target {
            low = mid + 1
        } else {
            high = mid
        }
    }
    if len(arr) == 0 {
        return -1
    }
    if arr[low] == target {
        return low
    }
    return -1
}
`

const bugfixTestCode = `package search

import "testing"

func TestFindFirstElement(t *testing.T) {
    arr := []int{1, 3, 5, 7, 9}
    if got := BinarySearch(arr, 1); got != 0 {
        t.Errorf("BinarySearch([1,3,5,7,9], 1) = %d, want 0", got)
    }
}

func TestFindMiddleElement(t *testing.T) {
    arr := []int{1, 3, 5, 7, 9}
    if got := BinarySearch(arr, 5); got != 2 {
        t.Errorf("BinarySearch([1,3,5,7,9], 5) = %d, want 2", got)
    }
}

func TestFindLastElement(t *testing.T) {
    arr := []int{1, 3, 5, 7, 9}
    if got := BinarySearch(arr, 9); got != 4 {
        t.Errorf("BinarySearch([1,3,5,7,9], 9) = %d, want 4", got)
    }
}

func TestNotFound(t *testing.T) {
    arr := []int{1, 3, 5, 7, 9}
    if got := BinarySearch(arr, 4); got != -1 {
        t.Errorf("BinarySearch([1,3,5,7,9], 4) = %d, want -1", got)
    }
}

func TestEmptySlice(t *testing.T) {
    arr := []int{}
    if got := BinarySearch(arr, 1); got != -1 {
        t.Errorf("BinarySearch([], 1) = %d, want -1", got)
    }
}
`

func BugfixVerify(repoDir string) VerificationResult {
    result := VerificationResult{Name: "bugfix"}

    // 1. go test ./pkg/search/... must pass (including TestFindLastElement)
    if err := runCommand(repoDir, "go", "test", "./pkg/search/..."); err != nil {
        result.Errors = append(result.Errors, "tests still fail: "+err.Error())
        result.Passed = false
        return result
    }
    result.Checks = append(result.Checks, Check{Name: "tests_pass", Passed: true})

    // 2. The fix must be minimal — only search.go changed
    diff, err := runCommandOutput(repoDir, "git", "diff", "HEAD~1", "--", "pkg/search/search.go")
    if err != nil {
        // Might not be HEAD~1, try getting diff of just the changed file
        diff, err = runCommandOutput(repoDir, "git", "log", "--oneline", "-5")
        // Non-fatal: we can still check file count
    }

    // 3. No files other than search.go should be modified
    changedFiles, _ := runCommandOutput(repoDir, "git", "diff", "--name-only", "HEAD~1")
    changedLines := strings.Split(strings.TrimSpace(changedFiles), "\n")
    onlySearchGo := len(changedLines) == 1 && strings.Contains(changedLines[0], "search.go")
    result.Checks = append(result.Checks, Check{Name: "minimal_diff", Passed: onlySearchGo})

    // 4. No fordjent/failed:error label on the issue (checked separately)

    result.Passed = len(result.Errors) == 0
    return result
}
```

## Metrics Collection

```go
// metrics.go

type TrialResult struct {
    Success           bool
    WallTime          time.Duration
    SystemRoleErrors  int
    FalseErrorLabels  int
    Metrics           *MetricsSnapshot
    Verification      VerificationResult
}

type MetricsSnapshot struct {
    TotalTokens       int64
    InputTokens       int64
    OutputTokens      int64
    TotalTurns        int
    ToolCallsTotal    int
    ToolCallsWrite    int
    ToolCallsRead     int
    ToolCallsBash     int
    ToolCallsGit      int
    ToolCallsForgejo  int
    SessionCount      int
    FailedSessionCount int
    CostUSD           float64
    ByModel           map[string]ModelMetrics
    LifecycleStates    map[string]int
}

type ModelMetrics struct {
    Calls     int
    InputTokens  int64
    OutputTokens int64
}

func (h *Harness) CollectMetrics() (*MetricsSnapshot, error) {
    // GET http://127.0.0.1:8080/status
    resp, err := http.Get(h.FordjentURL + "/status")
    // Parse JSON response
    // Extract:
    //   - metrics → fordjent_tokens_total, fordjent_llm_calls_total, etc.
    //   - lifecycle → active_sessions, failed_sessions, state counts
    //   - costs → total tokens, cost_usd
    //   - by_model → per-model call counts and token counts
    return snapshot, nil
}

func (h *Harness) CollectTrace(repo, sessionType string, issueNum int) (*TraceData, error) {
    // GET http://127.0.0.1:8080/trace/{owner}/{repo}/{sessionType}/{num}
    // Parse memory.jsonl entries
    // Extract: turn count, tool call distribution, duration
}

func (h *Harness) CountSystemRoleErrors() (int, error) {
    // Grep Fordjent logs for "Unexpected role" or HTTP 400 from provider
    // Log file: h.LocalDir + "/logs/fordjent-stdout.log"
    // This is a string match — no parsing needed
    logPath := filepath.Join(h.LocalDir, "logs", "fordjent-stdout.log")
    data, err := os.ReadFile(logPath)
    // Count occurrences of "Unexpected role" and "400 Bad Request"
    return count, nil
}

func (h *Harness) CountFalseErrorLabels(repo string) (int, error) {
    // GET /api/v1/repos/{repo}/issues?labels=fordjent%2Ffailed%3Aerror
    // For each labeled issue, check if code was actually produced:
    //   - If PR exists and was merged → false label (success misclassified)
    //   - If commits exist on any branch → false label (success misclassified)
    return falseCount, nil
}
```

## Baseline Comparison

```go
// baseline.go

type Baseline struct {
    Commit    string                  `json:"commit"`
    Timestamp string                  `json:"timestamp"`
    Scenarios map[string]ScenarioBase `json:"scenarios"`
}

type ScenarioBase struct {
    Trials   int                `json:"trials"`
    PassRate string             `json:"pass_rate"`
    Metrics  MedianMetrics      `json:"metrics"`
}

type MedianMetrics struct {
    TotalTokens        int64   `json:"total_tokens_median"`
    TotalTurns         int     `json:"total_turns_median"`
    WallTimeS          float64 `json:"wall_time_s_median"`
    SystemRoleErrors   int     `json:"system_role_errors"`
    FalseErrorRate     float64 `json:"false_error_rate"`
    ToolCallsWrite     int     `json:"tool_calls_write_median"`
    ToolCallsBash      int     `json:"tool_calls_bash_median"`
    ToolCallsTotal     int     `json:"tool_calls_total_median"`
}

var defaultBaselinePath = filepath.Join("internal", "eval", "baseline.json")

func LoadBaseline(path string) (*Baseline, error)

func (b *Baseline) Compare(result *ScenarioResult) ComparisonResult {
    // Compare pass_rate, metrics against baseline
    // FAIL if: pass_rate dropped, system_role_errors > 0, false_error_rate > 0.2
    // WARN if: tokens > baseline * 1.5, turns > baseline * 1.5, time > baseline * 2.0
}

func (b *Baseline) Save(path string) error

func UpdateBaseline(path string, result *ScenarioResult) error {
    // Only update if the test passed
    // Write new baseline.json with current commit hash
}
```

## Test Functions

```go
// eval_test.go

func TestEvalSmoke(t *testing.T) {
    // Fast: single bugfix trial, ~5 min
    // Catches: system-role errors, complete failures, false error labels
    h := NewHarness(t)
    defer h.TearDown()

    result := h.RunTrial(&BugfixScenario, 1)
    if !result.Success {
        t.Fatalf("Smoke test failed: %v", result.Verification.Errors)
    }
    if result.SystemRoleErrors > 0 {
        t.Fatalf("System role errors detected: %d", result.SystemRoleErrors)
    }
}

func TestEvalBenchGreenfield(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping benchmark in short mode")
    }

    h := NewHarness(t)
    defer h.TearDown()

    results := make([]*TrialResult, 5)
    for i := 0; i < 5; i++ {
        t.Logf("=== Greenfield trial %d/%d ===", i+1, 5)
        h.CreateRepo(GreenfieldScenario.RepoName + fmt.Sprintf("-%d", i))
        results[i] = h.RunTrial(&GreenfieldScenario, 1)
        h.ResetRepo(GreenfieldScenario.RepoName + fmt.Sprintf("-%d", i))
    }

    agg := AggregateResults(results)
    baseline := LoadBaseline(defaultBaselinePath)

    cmp := baseline.Compare(agg)
    cmp.PrintDiff(t)

    if cmp.ShouldFail() {
        t.Fatalf("Regression detected")
    }
}

func TestEvalBenchBugfix(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping benchmark in short mode")
    }

    h := NewHarness(t)
    defer h.TearDown()

    results := make([]*TrialResult, 5)
    for i := 0; i < 5; i++ {
        t.Logf("=== Bugfix trial %d/%d ===", i+1, 5)
        h.CreateRepo(BugfixScenario.RepoName + fmt.Sprintf("-%d", i))
        results[i] = h.RunTrial(&BugfixScenario, 1)
        h.ResetRepo(BugfixScenario.RepoName + fmt.Sprintf("-%d", i))
    }

    agg := AggregateResults(results)
    baseline := LoadBaseline(defaultBaselinePath)

    cmp := baseline.Compare(agg)
    cmp.PrintDiff(t)

    if cmp.ShouldFail() {
        t.Fatalf("Regression detected")
    }
}
```

## Trial Flow (step by step)

```
NewHarness(t)
  ├── Generate random workdir (~/fordjent-eval-XXXX/)
  ├── Write app.ini from template
  ├── Write fordjent.yaml from template
  ├── Write sandbox profiles
  ├── Start Forgejo (sandbox-exec -f forgejo.sb forgejo web ...)
  │     └── Wait for GET /api/v1/version → 200
  ├── Create admin user + tokens (forgejo admin user create ...)
  ├── Start Fordjent (sandbox-exec -f fordjent.sb fordjent -config ...)
  │     └── Wait for GET /healthz → 200
  └── Return harness

RunTrial(scenario)
  ├── CreateRepo(scenario.RepoName)
  │     ├── POST /api/v1/user/repos → create repo
  │     ├── SeedFiles(repo, scenario.SeedFiles)
  │     │     └── For each file: POST .../contents/{path} with base64 content
  │     ├── CreateLabels(repo, standardLabels)
  │     │     └── planning, implementing, ready, blocked, done, fordjent/*, role:*
  │     ├── CreateWebhook(repo, fordjentURL, webhookSecret)
  │     └── Git clone + initial pull
  ├── CreateIssue(repo, scenario.IssueTitle, scenario.IssueBody)
  │     └── POST /api/v1/repos/{repo}/issues
  ├── WaitForCompletion(repo, issueNum, timeout)
  │     ├── Every 5s: check lifecycle state via /status
  │     ├── Every 5s: check issue state in Forgejo
  │     ├── If issue closed → success
  │     ├── If PR merged → success
  │     ├── If lifecycle state = failed_* → failure (collect metrics anyway)
  │     └── If timeout → hard failure
  ├── CollectMetrics()
  │     ├── GET /status → tokens, calls, costs, model breakdown
  │     ├── CountSystemRoleErrors() → grep logs
  │     └── CountFalseErrorLabels() → check Forgejo issue labels
  ├── Verify(repo, scenario.Verify)
  │     ├── git clone repo to temp dir
  │     ├── go build ./...
  │     ├── go test ./...
  │     ├── Run functional tests (scenario-specific)
  │     └── Check diff minimality (bugfix scenario)
  └── Return TrialResult

ResetRepo(repo)
  ├── Close all issues via Forgejo API
  ├── Close all PRs via Forgejo API
  ├── Delete all feature branches
  ├── Reset main to seed commit
  └── Wait for Fordjent sessions to drain

TearDown()
  ├── Kill Fordjent process
  ├── Kill Forgejo process
  └── Remove workdir
```

## Metrics Collected Per Trial

From `/status` endpoint:
- `fordjent_tokens_total` (input + output)
- `fordjent_llm_calls_total`
- `fordjent_cost_total`
- Per-model breakdown (calls, tokens, cost)
- Lifecycle state counts (active, completed, failed)
- Active sessions list (for trace links)

From `/trace/{sessionKey}` endpoint:
- Turn count
- Tool call count and breakdown (write_file, read_file, bash, git, forgejo_*)
- Per-turn latency
- Compaction events

From Forgejo API:
- Issue state (open/closed)
- PR state (open/closed/merged)
- Labels on issue (fordjent/failed:*)
- Comments on issue (count, author)
- Milestones (title, progress %)

From git:
- Number of commits on main since seed
- Number of files changed
- Diff size (lines added/removed)

From logs:
- System-role errors (grep for "Unexpected role", HTTP 400)
- Auto-retry events (grep for "auto-retry")
- Compaction events (grep for "compacted context")
- Steering events (grep for "Fordjent Steering")

## Seed File Templates

Files are committed into the repo via Forgejo contents API (base64-encoded).
The harness provides them as Go string maps in `scenarios_*.go`.

For the bugfix scenario, the buggy code is provided as a Go const
(`bugfixBuggyCode`) and the test file as `bugfixTestCode`. The known fix
is provided for verification (to check diff closeness) but is NOT given
to the agent.

## Configuration

The harness reads these environment variables:

| Variable | Purpose | Default |
|----------|---------|---------|
| `EVAL_LLM_PROVIDER` | Which provider config to use | `wafer-qwen` |
| `EVAL_FORGEJO_PORT` | Forgejo HTTP port | `3000` |
| `EVAL_FORDJENT_PORT` | Fordjent HTTP port | `8080` |
| `EVAL_WAFER_API_KEY` | Wafer API key | (required) |
| `EVAL_SCALEWAY_API_KEY` | Scaleway API key | (optional) |
| `EVAL_TRIALS` | Number of trials per scenario | `5` |
| `EVAL_TIMEOUT` | Max wait per trial | `15m` |
| `EVAL_SKIP_SETUP` | Skip Forgejo/Fordjent setup (use existing) | `false` |
| `EVAL_SKIP_TEARDOWN` | Skip teardown (keep instance running) | `false` |

The `EVAL_SKIP_SETUP` flag is useful for iterative development: start
Forgejo + Fordjent once, then run multiple tests against the same instance
without restarting between them.

The `EVAL_SKIP_TEARDOWN` flag lets you inspect the instance after a test
failure (check logs, UI, traces).

## Fordjent Config Template

The harness generates a `fordjent.yaml` from a template, using the same
structure as `fordjent.local.yaml` but with:

```yaml
agent:
  max_turns: 75                # standard budget
  max_turns_implementer: 50    # standard implementer budget
  max_turns_pm: 15             # standard PM budget
  max_turns_reviewer: 20       # standard reviewer budget
  role_providers:
    pm: "${EVAL_LLM_PROVIDER}"
    reviewer: "${EVAL_LLM_PROVIDER}"
    implementer: "${EVAL_LLM_PROVIDER}"
    tester: "${EVAL_LLM_PROVIDER}"
    devops: "${EVAL_LLM_PROVIDER}"
  fallback_provider: "${EVAL_LLM_PROVIDER}"
  require_role_tag: true
  enable_lifecycle: true
  enable_stale_gate: true
  enable_scaffold_detection: true
  session_timeout: "30m"       # generous timeout for evals
  context_window: 131072
  compaction_threshold: 0.85
  compaction_keep_turns: 8

budget:
  enabled: true
  max_session_cost: 5.00       # generous for evals
  max_monthly_cost: 100.00

sandbox:
  enabled: false               # macOS sandbox-exec used instead

providers:
  - name: "wafer-qwen"
    api_base: "https://pass.wafer.ai/v1"
    api_key: "${EVAL_WAFER_API_KEY}"
    model: "Qwen3.5-397B-A17B"
    max_tokens: 32768
    request_timeout: "90s"
    max_retries: 5
```

The provider config is intentionally fixed across all trials of a given
scenario. To compare providers, create a new baseline for each provider
configuration.

## Inter-Trial Isolation

Each trial must be independent. Between trials:

1. **Close all issues** — PATCH each issue to state "closed"
2. **Close all PRs** — PATCH each PR to state "closed"
3. **Delete feature branches** — git push origin --delete feature/*
4. **Reset main to seed commit** — git reset --hard <seed-sha> && git push -f
5. **Wait for idle** — poll /status until active_sessions = 0
6. **Clear lifecycle DB** — not possible (append-only), but recorded
7. **Clear cost DB** — not possible (append-only), but read for metrics

The cost and lifecycle DBs are append-only, so the harness records their
state at the START of each trial and computes the DELTA between start and
end. This gives per-trial metrics without needing to clear the databases.

```go
func (h *Harness) RecordBaseline() *MetricsSnapshot {
    // Snapshot current metrics before trial
    return h.CollectMetrics()
}

func (h *Harness) ComputeDelta(before, after *MetricsSnapshot) *MetricsDelta {
    return &MetricsDelta{
        TotalTokens:     after.TotalTokens - before.TotalTokens,
        InputTokens:     after.InputTokens - before.InputTokens,
        OutputTokens:    after.OutputTokens - before.OutputTokens,
        LLMCalls:        after.LLMCalls - before.LLMCalls,
        CostUSD:         after.CostUSD - before.CostUSD,
        // ... etc
    }
}
```

## Handling Provider Failures

LLM providers can be flaky. The harness must handle:

1. **Provider timeout** → session fails with `failed_error`, recorded in metrics
2. **Rate limiting** → Fordjent retries with backoff (built-in), harness just waits
3. **Total provider outage** → all trials fail, harness reports 0/N pass rate

If a trial fails due to a provider error (evidenced by repeated "context
deadline exceeded" or "500 Internal Server Error" in logs), the trial is
still recorded but marked as `provider_failure: true`. These trials are
excluded from median calculations but included in pass rate.

```go
type TrialResult struct {
    // ...
    ProviderFailure bool // true if LLM provider was down/unreachable
    AgentFailure    bool // true if agent failed (max turns, tool error)
    VerifyFailure   bool // true if produced code doesn't pass verification
}
```

## Output Format

Each trial produces a JSON file in `internal/eval/output/`:

```json
{
  "trial": 1,
  "scenario": "greenfield",
  "timestamp": "2026-05-26T14:30:00Z",
  "commit": "7bc390d",
  "duration_s": 283,
  "result": {
    "success": true,
    "provider_failure": false,
    "agent_failure": false,
    "verify_failure": false,
    "verification": {
      "checks": [
        {"name": "build", "passed": true},
        {"name": "test", "passed": true},
        {"name": "reverse_hello", "passed": true},
        {"name": "wordcount_hello_world", "passed": true},
        {"name": "files_exist", "passed": true}
      ],
      "errors": []
    },
    "metrics": {
      "total_tokens": 182000,
      "total_turns": 35,
      "wall_time_s": 283,
      "system_role_errors": 0,
      "false_error_labels": 0,
      "tool_calls_write": 6,
      "tool_calls_bash": 12,
      "tool_calls_total": 78,
      "cost_usd": 0.00,
      "by_model": {
        "wafer-qwen": {"calls": 35, "input_tokens": 150000, "output_tokens": 32000}
      }
    }
  }
}
```

After all trials, an aggregate report is written:

```json
{
  "scenario": "greenfield",
  "commit": "7bc390d",
  "timestamp": "2026-05-26T15:15:00Z",
  "trials": 5,
  "pass_rate": "4/5",
  "provider_failures": 0,
  "medians": {
    "total_tokens": 182000,
    "total_turns": 35,
    "wall_time_s": 283,
    "system_role_errors": 0,
    "false_error_labels": 0,
    "tool_calls_total": 78
  },
  "comparison": {
    "baseline_commit": "abc1234",
    "regressions": [],
    "warnings": [],
    "improvements": [
      {"metric": "total_tokens", "baseline": 195000, "current": 182000, "change": "-6.7%"}
    ]
  }
}
```

## Running the Eval

The eval harness is implemented in `internal/eval/`. Key commands:

```bash
# Fast smoke test (~5 min, single bugfix trial)
EVAL_WAFER_API_KEY=wfr_... go test ./internal/eval/... -v -run TestEvalSmoke -timeout 15m

# Full greenfield benchmark (~30 min, 5 trials)
EVAL_WAFER_API_KEY=wfr_... go test ./internal/eval/... -v -run TestEvalBenchGreenfield -timeout 60m

# Full bugfix benchmark (~25 min, 5 trials)
EVAL_WAFER_API_KEY=wfr_... go test ./internal/eval/... -v -run TestEvalBenchBugfix -timeout 45m

# All benchmarks (~70 min)
EVAL_WAFER_API_KEY=wfr_... go test ./internal/eval/... -v -timeout 120m

# Skip benchmarks (short mode — unit tests only)
go test ./internal/eval/... -v -short

# Update baseline after confirmed improvement
go test ./internal/eval/... -v -run TestEvalBenchBugfix -update-baseline

# Use existing Forgejo+Fordjent (no setup/teardown)
EVAL_SKIP_SETUP=true EVAL_SKIP_TEARDOWN=true \
  FORGEJO_TOKEN=xxx FORGEJO_ADMIN_TOKEN=xxx \
  go test ./internal/eval/... -v -run TestEvalSmoke
```

Environment variables:
- `EVAL_WAFER_API_KEY` — Wafer API key for LLM provider
- `EVAL_SCALEWAY_API_KEY` — (optional) Scaleway AI key
- `EVAL_FORGEJO_PORT` — Forgejo port (default 3000)
- `EVAL_FORDJENT_PORT` — Fordjent port (default 8080)
- `EVAL_SKIP_SETUP` — Skip service startup, connect to existing
- `EVAL_SKIP_TEARDOWN` — Keep services running after test
- `FORGEJO_TOKEN`/`FORGEJO_ADMIN_TOKEN` — Required when EVAL_SKIP_SETUP=true

OpenSpec change: `openspec/changes/eval-harness/`

## What Not To Test

These are out of scope for the eval harness:

- **k-sample boosting** (parallel implementers): requires multi-session
  infrastructure, separate benchmark
- **Cross-session memory** (recall tool): requires two sequential issues,
  separate benchmark
- **Model comparison** (provider A vs B): same harness, separate baseline
- **Stress test** (N simultaneous issues): same harness, separate benchmark
- **Longevity** (10+ sequential issues): same harness, separate benchmark

The harness infrastructure (Harness, Forgejo, Metrics, Verification) is
designed to be reusable for all of these. Scenarios are additive.