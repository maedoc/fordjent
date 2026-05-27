package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// Baseline represents committed benchmark metrics for regression detection.
type Baseline struct {
	Commit    string                   `json:"commit"`
	Timestamp string                   `json:"timestamp"`
	Scenarios map[string]ScenarioBase  `json:"scenarios"`
}

// ScenarioBase represents the baseline metrics for a single scenario.
type ScenarioBase struct {
	Trials   int          `json:"trials"`
	PassRate string       `json:"pass_rate"`
	Metrics  MedianMetrics `json:"metrics"`
}

// LoadBaseline reads the committed baseline.json file.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Return empty baseline if file doesn't exist
		return &Baseline{
			Commit:    "none",
			Timestamp: time.Now().Format(time.RFC3339),
			Scenarios: make(map[string]ScenarioBase),
		}, nil
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse baseline: %w", err)
	}
	return &b, nil
}

// Save writes the baseline to a JSON file.
func (b *Baseline) Save(path string) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal baseline: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// ComparisonResult represents the result of comparing current metrics against baseline.
type ComparisonResult struct {
	Regressions  []string // FAIL-level regressions (pass rate drop, system-role errors)
	Warnings     []string // WARN-level issues (efficiency drift)
	Improvements []Improvement
	Passed       bool
}

// Improvement represents a metric improvement over baseline.
type Improvement struct {
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	Change   string  `json:"change"` // e.g. "-6.7%"
}

// Compare compares the current scenario result against the baseline.
func (b *Baseline) Compare(result *ScenarioResult) ComparisonResult {
	cmp := ComparisonResult{Passed: true}

	base, ok := b.Scenarios[result.Scenario]
	if !ok {
		// No baseline for this scenario — first run
		cmp.Improvements = append(cmp.Improvements, Improvement{
			Metric: "baseline", Current: float64(result.Passes), Change: "first run",
		})
		return cmp
	}

	// Check pass rate regression
	currentPassRate := fmt.Sprintf("%d/%d", result.Passes, result.Trials)
	if currentPassRate != base.PassRate {
		cmp.Regressions = append(cmp.Regressions,
			fmt.Sprintf("pass_rate: %s → %s", base.PassRate, currentPassRate))
		cmp.Passed = false
	}

	// Check system-role errors
	if result.Medians.SystemRoleErrors > 0 {
		cmp.Regressions = append(cmp.Regressions,
			fmt.Sprintf("system_role_errors: %d (expected 0)", result.Medians.SystemRoleErrors))
		cmp.Passed = false
	}

	// Check false error rate
	if result.Medians.FalseErrorLabels > 0 {
		falseRate := float64(result.Medians.FalseErrorLabels) / float64(result.Trials)
		if falseRate > 0.2 {
			cmp.Regressions = append(cmp.Regressions,
				fmt.Sprintf("false_error_rate: %.2f (expected ≤ 0.2)", falseRate))
			cmp.Passed = false
		}
	}

	// Check efficiency (WARN, not FAIL)
	if base.Metrics.TotalTokens > 0 && result.Medians.TotalTokens > base.Metrics.TotalTokens*3/2 {
		pctChange := float64(result.Medians.TotalTokens-base.Metrics.TotalTokens) / float64(base.Metrics.TotalTokens) * 100
		cmp.Warnings = append(cmp.Warnings,
			fmt.Sprintf("total_tokens: %d → %d (%.0f%% increase)", base.Metrics.TotalTokens, result.Medians.TotalTokens, pctChange))
	}

	if base.Metrics.TotalTurns > 0 && result.Medians.TotalTurns > base.Metrics.TotalTurns*3/2 {
		pctChange := float64(result.Medians.TotalTurns-base.Metrics.TotalTurns) / float64(base.Metrics.TotalTurns) * 100
		cmp.Warnings = append(cmp.Warnings,
			fmt.Sprintf("total_turns: %d → %d (%.0f%% increase)", base.Metrics.TotalTurns, result.Medians.TotalTurns, pctChange))
	}

	if base.Metrics.WallTimeS > 0 && result.Medians.WallTimeS > base.Metrics.WallTimeS*2 {
		pctChange := (result.Medians.WallTimeS - base.Metrics.WallTimeS) / base.Metrics.WallTimeS * 100
		cmp.Warnings = append(cmp.Warnings,
			fmt.Sprintf("wall_time: %.0fs → %.0fs (%.0f%% increase)", base.Metrics.WallTimeS, result.Medians.WallTimeS, pctChange))
	}

	// Check for improvements
	if base.Metrics.TotalTokens > 0 && result.Medians.TotalTokens < base.Metrics.TotalTokens {
		pctChange := float64(base.Metrics.TotalTokens-result.Medians.TotalTokens) / float64(base.Metrics.TotalTokens) * 100
		cmp.Improvements = append(cmp.Improvements, Improvement{
			Metric: "total_tokens", Baseline: float64(base.Metrics.TotalTokens),
			Current: float64(result.Medians.TotalTokens), Change: fmt.Sprintf("-%.0f%%", pctChange),
		})
	}

	return cmp
}

// ComputeMedianMetrics calculates median metrics across successful trials.
func ComputeMedianMetrics(results []TrialResult) MedianMetrics {
	if len(results) == 0 {
		return MedianMetrics{}
	}

	// Filter successful trials only
	var successes []TrialResult
	for _, r := range results {
		if r.Success && !r.ProviderFailure {
			successes = append(successes, r)
		}
	}
	if len(successes) == 0 {
		return MedianMetrics{}
	}

	// Sort by various metrics to find medians
	tokens := make([]int64, len(successes))
	turns := make([]int, len(successes))
	times := make([]float64, len(successes))
	sysErrors := 0
	falseErrors := 0

	for i, r := range successes {
		tokens[i] = r.Metrics.TotalTokens
		turns[i] = r.Metrics.TotalTurns
		times[i] = r.WallTime.Seconds()
		sysErrors += r.SystemRoleErrors
		falseErrors += r.FalseErrorLabels
	}

	sort.Slice(tokens, func(i, j int) bool { return tokens[i] < tokens[j] })
	sort.Slice(turns, func(i, j int) bool { return turns[i] < turns[j] })
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })

	return MedianMetrics{
		TotalTokens:      tokens[len(tokens)/2],
		TotalTurns:       turns[len(turns)/2],
		WallTimeS:        times[len(times)/2],
		SystemRoleErrors: sysErrors,
		FalseErrorLabels: falseErrors,
	}
}

// UpdateBaselineFlag checks if the UPDATE_BASELINE env var is set.
func UpdateBaselineFlag() bool {
	return os.Getenv("UPDATE_BASELINE") != ""
}

// defaultBaselinePath returns the path to the committed baseline.json.
func defaultBaselinePath() string {
	return "internal/eval/baseline.json"
}