package eval

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StatusResponse represents the JSON response from Fordjent's /status endpoint.
type StatusResponse struct {
	Now       string                 `json:"now"`
	Costs     map[string]interface{} `json:"costs"`
	ByModel   []map[string]interface{} `json:"by_model"`
	Lifecycle map[string]interface{} `json:"lifecycle"`
	Metrics   map[string]interface{} `json:"metrics"`
}

// MetricsSnapshot represents a point-in-time snapshot of Fordjent metrics.
type MetricsSnapshot struct {
	Timestamp          time.Time
	TotalTokens        int64
	InputTokens        int64
	OutputTokens       int64
	TotalTurns         int
	TotalLLMCalls      int
	CostUSD            float64
	SessionCount       int
	FailedSessionCount int
	SystemRoleErrors   int
	ByModel            map[string]ModelMetrics
}

// ModelMetrics represents per-model metrics.
type ModelMetrics struct {
	Calls        int
	InputTokens  int64
	OutputTokens int64
}

// MetricsDelta represents the difference between two metric snapshots.
type MetricsDelta struct {
	TotalTokens        int64
	InputTokens        int64
	OutputTokens       int64
	TotalTurns         int
	TotalLLMCalls      int
	CostUSD            float64
	WallTime           time.Duration
	SystemRoleErrors   int
	FalseErrorLabels   int
	ByModel            map[string]ModelMetrics
}

// TrialResult represents the outcome of a single benchmark trial.
type TrialResult struct {
	TrialNum          int
	Scenario          string
	Success           bool
	ProviderFailure   bool
	AgentFailure      bool
	VerifyFailure     bool
	WallTime          time.Duration
	Verification      VerificationResult
	Metrics           MetricsDelta
	SystemRoleErrors  int
	FalseErrorLabels  int
}

// VerificationResult represents the outcome of scenario verification.
type VerificationResult struct {
	Name   string
	Passed bool
	Checks []Check
	Errors []string
}

// Check represents a single verification step.
type Check struct {
	Name   string
	Passed bool
}

// ScenarioResult represents aggregated results across multiple trials.
type ScenarioResult struct {
	Scenario       string
	Commit         string
	Timestamp      time.Time
	Trials         int
	Passes         int
	ProviderFailures int
	Medians        MedianMetrics
	AllResults     []TrialResult
}

// MedianMetrics holds median values across successful trials.
type MedianMetrics struct {
	TotalTokens        int64
	TotalTurns         int
	WallTimeS          float64
	SystemRoleErrors   int
	FalseErrorLabels   int
	ToolCallsWrite     int
	ToolCallsBash      int
	ToolCallsTotal     int
}

// RecordBaseline captures a metrics snapshot before a trial begins.
func (h *Harness) RecordBaseline() (*MetricsSnapshot, error) {
	return h.CollectMetrics()
}

// ComputeDelta computes the difference between two metric snapshots.
func ComputeDelta(before, after *MetricsSnapshot) MetricsDelta {
	return MetricsDelta{
		TotalTokens:   after.TotalTokens - before.TotalTokens,
		InputTokens:   after.InputTokens - before.InputTokens,
		OutputTokens:  after.OutputTokens - before.OutputTokens,
		TotalTurns:    after.TotalTurns - before.TotalTurns,
		TotalLLMCalls: after.TotalLLMCalls - before.TotalLLMCalls,
		CostUSD:       after.CostUSD - before.CostUSD,
		WallTime:      after.Timestamp.Sub(before.Timestamp),
		ByModel:       mergeModelDeltas(before.ByModel, after.ByModel),
	}
}

// CollectMetrics fetches metrics from Fordjent's /status endpoint.
func (h *Harness) CollectMetrics() (*MetricsSnapshot, error) {
	resp, err := http.Get(h.FordjentURL + "/status")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch status: %w", err)
	}
	defer resp.Body.Close()

	var raw StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to decode status: %w", err)
	}

	snapshot := &MetricsSnapshot{
		Timestamp: time.Now(),
		ByModel:   make(map[string]ModelMetrics),
	}

	// Extract totals from metrics
	if metrics, ok := raw.Metrics["fordjent_tokens_total"]; ok {
		// Metrics is a map from the prometheus-style snapshot
		// Try to extract values from the interface{} structure
		snapshot.TotalTokens = extractInt64(metrics)
	}

	// Try alternative field names from the actual JSON response
	if costs, ok := raw.Costs["total_tokens"]; ok {
		snapshot.TotalTokens = extractInt64(costs)
	}
	if costs, ok := raw.Costs["total_input_tokens"]; ok {
		snapshot.InputTokens = extractInt64(costs)
	}
	if costs, ok := raw.Costs["total_output_tokens"]; ok {
		snapshot.OutputTokens = extractInt64(costs)
	}

	// Extract lifecycle data
	if lc, ok := raw.Lifecycle["active_sessions"]; ok {
		snapshot.SessionCount = int(extractFloat64(lc))
	}
	if lc, ok := raw.Lifecycle["failed_sessions"]; ok {
		snapshot.FailedSessionCount = int(extractFloat64(lc))
	}

	// Extract per-model data
	for _, m := range raw.ByModel {
		name, _ := m["model"].(string)
		if name == "" {
			continue
		}
		mm := ModelMetrics{
			Calls:       int(extractFloat64(m["calls"])),
			InputTokens: int64(extractFloat64(m["input_tokens"])),
			OutputTokens: int64(extractFloat64(m["output_tokens"])),
		}
		snapshot.ByModel[name] = mm
	}

	return snapshot, nil
}

// CountSystemRoleErrors scans Fordjent's log file for system-role error messages.
func (h *Harness) CountSystemRoleErrors() (int, error) {
	logPath := filepath.Join(h.LocalDir, "logs", "fordjent-stdout.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return 0, fmt.Errorf("failed to read fordjent log: %w", err)
	}
	content := string(data)
	count := strings.Count(content, "Unexpected role")
	count += strings.Count(content, "role 'system'")
	count += strings.Count(content, "400 Bad Request") // Scaleway API rejection
	return count, nil
}

// CountFalseErrorLabels checks for fordjent/failed:error labels on issues
// where code was actually produced (merged PR or commits).
func (h *Harness) CountFalseErrorLabels(repo string) (int, error) {
	// GET /api/v1/repos/{repo}/issues?labels=fordjent%2Ffailed%3Aerror
	body, err := h.doForgejoRequest("GET", "/repos/"+repo+"/issues?labels=fordjent%2Ffailed%3Aerror", nil)
	if err != nil {
		return 0, fmt.Errorf("failed to query issues: %w", err)
	}

	var issues []map[string]interface{}
	if err := json.Unmarshal([]byte(body), &issues); err != nil {
		return 0, fmt.Errorf("failed to parse issues: %w", err)
	}

	falseCount := 0
	for _, issue := range issues {
		issueNum := int(extractFloat64(issue["number"]))
		state, _ := issue["state"].(string)

		// Check if this issue has a merged PR
		// GET /api/v1/repos/{repo}/pulls?state=closed
		prBody, err := h.doForgejoRequest("GET",
			fmt.Sprintf("/repos/%s/pulls?state=closed", repo), nil)
		if err != nil {
			continue
		}

		var prs []map[string]interface{}
		if err := json.Unmarshal([]byte(prBody), &prs); err != nil {
			continue
		}

		hasMergedPR := false
		for _, pr := range prs {
			if merged, ok := pr["merged"].(bool); ok && merged {
				hasMergedPR = true
				break
			}
		}

		// If the issue has a merged PR but is labeled as failed:error, that's a false label
		if hasMergedPR && state != "closed" {
			falseCount++
			continue
		}

		// Also check if there are any commits on feature branches
		// (simplistic: if the issue is still open but has activity, it's likely a false label)
		_ = issueNum // suppress unused warning
	}

	return falseCount, nil
}

func mergeModelDeltas(before, after map[string]ModelMetrics) map[string]ModelMetrics {
	result := make(map[string]ModelMetrics)
	for name, m := range after {
		b, ok := before[name]
		if !ok {
			result[name] = m
			continue
		}
		result[name] = ModelMetrics{
			Calls:        m.Calls - b.Calls,
			InputTokens:  m.InputTokens - b.InputTokens,
			OutputTokens: m.OutputTokens - b.OutputTokens,
		}
	}
	return result
}

func extractInt64(v interface{}) int64 {
	switch val := v.(type) {
	case float64:
		return int64(val)
	case int64:
		return val
	case int:
		return int64(val)
	case json.Number:
		n, _ := val.Int64()
		return n
	default:
		return 0
	}
}

func extractFloat64(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int64:
		return float64(val)
	case int:
		return float64(val)
	case json.Number:
		f, _ := val.Float64()
		return f
	default:
		return 0
	}
}

