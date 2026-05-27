package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvalSmoke(t *testing.T) {
	cfg := DefaultHarnessConfig()
	h := NewHarnessWithConfig(t, cfg)
	defer h.TearDown()

	// 1. Set up bugfix benchmark repo
	repo := h.AdminUser + "/" + BugfixScenario.RepoName
	if err := h.CreateRepo(BugfixScenario.RepoName); err != nil {
		t.Fatalf("CreateRepo failed: %v", err)
	}
	if err := h.SeedFiles(repo, BugfixScenario.SeedFiles); err != nil {
		t.Fatalf("SeedFiles failed: %v", err)
	}
	if err := h.CreateLabels(repo); err != nil {
		t.Fatalf("CreateLabels failed: %v", err)
	}
	if err := h.CreateWebhook(repo); err != nil {
		t.Fatalf("CreateWebhook failed: %v", err)
	}

	// 2. Record baseline metrics
	baseline, err := h.RecordBaseline()
	if err != nil {
		t.Logf("Warning: could not record baseline metrics: %v", err)
	}

	// 3. Create the issue
	issueNum, err := h.CreateIssue(repo, BugfixScenario.IssueTitle, BugfixScenario.IssueBody)
	if err != nil {
		t.Fatalf("CreateIssue failed: %v", err)
	}

	// 4. Wait for completion
	result, err := h.WaitForCompletion(repo, issueNum, BugfixScenario.Timeout)
	if err != nil {
		t.Fatalf("WaitForCompletion failed: %v", err)
	}

	// 5. Collect metrics delta
	if baseline != nil {
		after, err := h.CollectMetrics()
		if err == nil {
			result.Metrics = ComputeDelta(baseline, after)
		}
	}
	result.Metrics.WallTime = time.Since(time.Now().Add(-result.WallTime))

	// 6. Count system-role errors
	sysErrors, err := h.CountSystemRoleErrors()
	if err != nil {
		t.Logf("Warning: could not count system-role errors: %v", err)
	} else {
		result.SystemRoleErrors = sysErrors
		if sysErrors > 0 {
			t.Errorf("System-role errors detected: %d", sysErrors)
		}
	}

	// 7. Clone repo and verify
	cloneDir := filepath.Join(h.LocalDir, "verify-"+BugfixScenario.RepoName)
	if err := cloneRepo(h.ForgejoURL, repo, h.AdminToken, cloneDir); err != nil {
		t.Fatalf("clone repo failed: %v", err)
	}

	verification := BugfixScenario.Verify(cloneDir)
	result.Verification = verification

	t.Logf("Smoke test result: success=%v checks=%d errors=%v",
		verification.Passed, len(verification.Checks), verification.Errors)

	if !verification.Passed {
		t.Errorf("Verification failed: %v", verification.Errors)
	}

	// 8. Compare against baseline
	bl, err := LoadBaseline(defaultBaselinePath())
	if err != nil {
		t.Logf("Warning: could not load baseline: %v", err)
	} else {
		scenarioResult := &ScenarioResult{
			Scenario:  BugfixScenario.Name,
			Commit:    "current",
			Timestamp: time.Now(),
			Trials:    1,
			Passes:    boolToInt(result.Success),
		}
		scenarioResult.Medians = ComputeMedianMetrics([]TrialResult{*result})

		cmp := bl.Compare(scenarioResult)
		for _, r := range cmp.Regressions {
			t.Errorf("REGRESSION: %s", r)
		}
		for _, w := range cmp.Warnings {
			t.Logf("WARNING: %s", w)
		}
		for _, imp := range cmp.Improvements {
			t.Logf("IMPROVEMENT: %s: %v → %v (%s)", imp.Metric, imp.Baseline, imp.Current, imp.Change)
		}
	}

	// 9. Write report
	writeReport(t, BugfixScenario.Name, []TrialResult{*result})
}

func TestEvalBenchGreenfield(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark in short mode")
	}
	runBenchmark(t, &GreenfieldScenario, 5)
}

func TestEvalBenchBugfix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark in short mode")
	}
	runBenchmark(t, &BugfixScenario, 5)
}

func runBenchmark(t *testing.T, scenario *Scenario, trials int) {
	t.Helper()

	cfg := DefaultHarnessConfig()
	h := NewHarnessWithConfig(t, cfg)
	defer h.TearDown()

	var results []TrialResult

	for i := 0; i < trials; i++ {
		t.Logf("=== %s trial %d/%d ===", scenario.Name, i+1, trials)

		// Set up repo
		repoName := fmt.Sprintf("%s-%d", scenario.RepoName, i)
		fullRepo := h.AdminUser + "/" + repoName
		if err := h.CreateRepo(repoName); err != nil {
			t.Fatalf("CreateRepo failed: %v", err)
		}
		if err := h.SeedFiles(fullRepo, scenario.SeedFiles); err != nil {
			t.Fatalf("SeedFiles failed: %v", err)
		}
		if err := h.CreateLabels(fullRepo); err != nil {
			t.Fatalf("CreateLabels failed: %v", err)
		}
		if err := h.CreateWebhook(fullRepo); err != nil {
			t.Fatalf("CreateWebhook failed: %v", err)
		}

		// Record baseline
		baseline, _ := h.RecordBaseline()
		startTime := time.Now()

		// Create issue
		issueNum, err := h.CreateIssue(fullRepo, scenario.IssueTitle, scenario.IssueBody)
		if err != nil {
			t.Fatalf("CreateIssue failed: %v", err)
		}

		// Wait for completion
		trialResult, err := h.WaitForCompletion(fullRepo, issueNum, scenario.Timeout)
		if err != nil {
			t.Errorf("Trial %d failed: %v", i+1, err)
			trialResult = &TrialResult{
				TrialNum: i + 1,
				Scenario: scenario.Name,
				Success:  false,
			}
		}
		trialResult.TrialNum = i + 1
		trialResult.Scenario = scenario.Name
		trialResult.WallTime = time.Since(startTime)

		// Collect metrics
		if baseline != nil {
			after, err := h.CollectMetrics()
			if err == nil {
				trialResult.Metrics = ComputeDelta(baseline, after)
			}
		}

		// Count errors
		sysErrors, _ := h.CountSystemRoleErrors()
		trialResult.SystemRoleErrors = sysErrors

		// Verify outcome
		cloneDir := filepath.Join(h.LocalDir, fmt.Sprintf("verify-%s-%d", scenario.RepoName, i))
		if err := cloneRepo(h.ForgejoURL, fullRepo, h.AdminToken, cloneDir); err == nil {
			trialResult.Verification = scenario.Verify(cloneDir)
			trialResult.Success = trialResult.Verification.Passed
		}

		// Detect provider failures
		if strings.Contains(fmt.Sprintf("%v", err), "context deadline") ||
			strings.Contains(fmt.Sprintf("%v", err), "503") {
			trialResult.ProviderFailure = true
		}

		results = append(results, *trialResult)

		// Reset between trials (close issues, wait for idle)
		if i < trials-1 {
			t.Logf("Resetting between trials...")
			if err := h.CloseAllIssues(fullRepo); err != nil {
				t.Logf("Warning: CloseAllIssues failed: %v", err)
			}
			h.waitForIdle(60 * time.Second)
		}
	}

	// Aggregate results
	passes := 0
	for _, r := range results {
		if r.Success {
			passes++
		}
	}

	scenarioResult := &ScenarioResult{
		Scenario:       scenario.Name,
		Commit:         "current",
		Timestamp:      time.Now(),
		Trials:         trials,
		Passes:         passes,
		ProviderFailures: countProviderFailures(results),
		Medians:        ComputeMedianMetrics(results),
		AllResults:     results,
	}

	// Compare against baseline
	bl, err := LoadBaseline(defaultBaselinePath())
	if err != nil {
		t.Logf("Warning: could not load baseline: %v", err)
	} else {
		cmp := bl.Compare(scenarioResult)
		for _, r := range cmp.Regressions {
			t.Errorf("REGRESSION: %s", r)
		}
		for _, w := range cmp.Warnings {
			t.Logf("WARNING: %s", w)
		}
		for _, imp := range cmp.Improvements {
			t.Logf("IMPROVEMENT: %s: %v → %v (%s)", imp.Metric, imp.Baseline, imp.Current, imp.Change)
		}

		if !cmp.Passed {
			t.Errorf("Baseline comparison failed")
		}
	}

	// Write report
	writeReport(t, scenario.Name, results)

	t.Logf("=== %s: %d/%d passed ===", scenario.Name, passes, trials)

	// Update baseline if requested
	if UpdateBaselineFlag() && passes > 0 {
		bl.Scenarios[scenario.Name] = ScenarioBase{
			Trials:   trials,
			PassRate: fmt.Sprintf("%d/%d", passes, trials),
			Metrics:  scenarioResult.Medians,
		}
		bl.Commit = "current"
		bl.Timestamp = time.Now().Format(time.RFC3339)
		if err := bl.Save(defaultBaselinePath()); err != nil {
			t.Logf("Warning: could not save baseline: %v", err)
		} else {
			t.Logf("Baseline updated for %s", scenario.Name)
		}
	}
}

// WaitForCompletion polls until Fordjent finishes processing an issue.
func (h *Harness) WaitForCompletion(repo string, issueNum int, timeout time.Duration) (*TrialResult, error) {
	result := &TrialResult{}
	startTime := time.Now()
	deadline := startTime.Add(timeout)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Now().After(deadline) {
				result.Success = false
				result.AgentFailure = true
				return result, fmt.Errorf("timeout after %v waiting for issue #%d", timeout, issueNum)
			}

			// Check issue state
			state, err := h.GetIssueState(repo, issueNum)
			if err == nil && state == "closed" {
				result.Success = true
				result.WallTime = time.Since(startTime)
				return result, nil
			}

			// Check lifecycle state via /status
			status, err := h.FetchStatus()
			if err == nil {
				lc, ok := status.Lifecycle["active_sessions"]
				if ok {
					active, _ := lc.(float64)
					if active == 0 && time.Since(startTime) > 30*time.Second {
						// No active sessions and some time has passed — likely done
						result.Success = true
						result.WallTime = time.Since(startTime)
						return result, nil
					}
				}
			}
		}
	}
}

// writeReport writes per-trial results to a JSON file.
func writeReport(t *testing.T, scenarioName string, results []TrialResult) {
	outputDir := "internal/eval/output"
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		t.Logf("Warning: could not create output dir: %v", err)
		return
	}

	filename := filepath.Join(outputDir, time.Now().Format("2006-01-02-150405")+"-"+scenarioName+".json")
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Logf("Warning: could not marshal report: %v", err)
		return
	}
	if err := os.WriteFile(filename, data, 0644); err != nil {
		t.Logf("Warning: could not write report: %v", err)
		return
	}
	t.Logf("Report written to %s", filename)
}

// cloneRepo clones a Forgejo repository to a local directory.
func cloneRepo(forgejoURL, repo, token, dir string) error {
	// Remove existing directory
	os.RemoveAll(dir)

	repoURL := fmt.Sprintf("%s/%s.git", forgejoURL, repo)
	if token != "" {
		// Forgejo accepts token in URL: http://token@host/owner/repo.git
		// Strip scheme, reassemble with token
		host := strings.TrimPrefix(forgejoURL, "http://")
		repoURL = fmt.Sprintf("http://%s@%s/%s.git", token, host, repo)
	}

	cmd := exec.Command("git", "clone", repoURL, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone failed: %v\n%s", err, string(out))
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func countProviderFailures(results []TrialResult) int {
	count := 0
	for _, r := range results {
		if r.ProviderFailure {
			count++
		}
	}
	return count
}