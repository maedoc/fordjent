package eval

import (
	"bufio"
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
	Now       string                   `json:"now"`
	Costs     map[string]interface{}   `json:"costs"`
	ByModel   []map[string]interface{} `json:"by_model"`
	Lifecycle map[string]interface{}   `json:"lifecycle"`
	Metrics   map[string]interface{}   `json:"metrics"`
}

// MetricsSnapshot represents a point-in-time snapshot of Fordjent metrics.
type MetricsSnapshot struct {
	Timestamp          time.Time
	TotalTokens        int64
	InputTokens        int64
	OutputTokens       int64
	TotalTurns         int
	TotalLLMCalls      int
	ToolCallsTotal     int
	LLMRetries        int
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
	TotalTokens      int64
	InputTokens      int64
	OutputTokens     int64
	TotalTurns       int
	TotalLLMCalls    int
	ToolCallsTotal   int
	CostUSD          float64
	WallTime         time.Duration
	SystemRoleErrors int
	FalseErrorLabels  int
	ByModel          map[string]ModelMetrics
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
	// Per-session analysis from memory.jsonl
	TurnCount        int
	ToolCallsTotal   int
	ToolCallsByType  map[string]int // tool_name -> count
	LLMCalls         int
	JudgeScore       float64 // 0-10 quality score from LLM judge
	JudgeFeedback     string  // detailed feedback
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
	Scenario         string
	Commit           string
	Timestamp        time.Time
	Trials           int
	Passes           int
	ProviderFailures int
	Medians          MedianMetrics
	AllResults       []TrialResult
}

// MedianMetrics holds median values across successful trials.
type MedianMetrics struct {
	TotalTokens      int64
	TotalTurns       int
	WallTimeS        float64
	SystemRoleErrors int
	FalseErrorLabels  int
	ToolCallsWrite   int
	ToolCallsBash    int
	ToolCallsTotal   int
}

// RecordBaseline captures a metrics snapshot before a trial begins.
func (h *Harness) RecordBaseline() (*MetricsSnapshot, error) {
	return h.CollectMetrics()
}

// ComputeDelta computes the difference between two metric snapshots.
func ComputeDelta(before, after *MetricsSnapshot) MetricsDelta {
	return MetricsDelta{
		TotalTokens:    after.TotalTokens - before.TotalTokens,
		InputTokens:    after.InputTokens - before.InputTokens,
		OutputTokens:   after.OutputTokens - before.OutputTokens,
		TotalTurns:     after.TotalTurns - before.TotalTurns,
		TotalLLMCalls:  after.TotalLLMCalls - before.TotalLLMCalls,
		ToolCallsTotal: after.ToolCallsTotal - before.ToolCallsTotal,
		CostUSD:        after.CostUSD - before.CostUSD,
		WallTime:       after.Timestamp.Sub(before.Timestamp),
		ByModel:        mergeModelDeltas(before.ByModel, after.ByModel),
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

	// Extract from /status metrics map (actual keys from Fordjent)
	// Keys: input_tokens, output_tokens, llm_calls_total, tool_calls_total,
	//       llm_retries_total, events_total, sessions_total, sessions_active, cost_usd
	snapshot.InputTokens = extractInt64(raw.Metrics["input_tokens"])
	snapshot.OutputTokens = extractInt64(raw.Metrics["output_tokens"])
	snapshot.TotalTokens = snapshot.InputTokens + snapshot.OutputTokens
	snapshot.TotalLLMCalls = int(extractInt64(raw.Metrics["llm_calls_total"]))
	snapshot.ToolCallsTotal = int(extractInt64(raw.Metrics["tool_calls_total"]))
	snapshot.TotalTurns = snapshot.TotalLLMCalls // turns ≈ LLM calls
	snapshot.CostUSD = extractFloat64(raw.Metrics["cost_usd"])

	// Also try costs from SQLite (more accurate if available)
	if costs, ok := raw.Costs["total_tokens"]; ok {
		if v := extractInt64(costs); v > 0 {
			snapshot.TotalTokens = v
		}
	}
	if costs, ok := raw.Costs["total_input_tokens"]; ok {
		if v := extractInt64(costs); v > 0 {
			snapshot.InputTokens = v
		}
	}
	if costs, ok := raw.Costs["total_output_tokens"]; ok {
		if v := extractInt64(costs); v > 0 {
			snapshot.OutputTokens = v
		}
	}

	// Lifecyle data
	if lc, ok := raw.Lifecycle["active_sessions"]; ok {
		snapshot.SessionCount = int(extractFloat64(lc))
	}
	if lc, ok := raw.Lifecycle["failed_sessions"]; ok {
		snapshot.FailedSessionCount = int(extractFloat64(lc))
	}

	// Per-model data
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

// SessionAnalysis holds per-session analysis data parsed from Fordjent logs.
type SessionAnalysis struct {
	TurnCount       int
	ToolCalls       int            // from turn_complete messages
	ToolCallsTotal  int            // sum of all tool call types
	ToolCallsByType map[string]int // tool_name -> count
	LLMCalls        int
	ToolErrors      int            // count of tool calls that returned errors
	LastToolCall    string         // last tool called
	HasPR           bool           // whether a PR was created
	HasCommit       bool           // whether a commit was made
	FileWrites      int            // count of write_file calls
	BashCommands    int            // count of bash tool calls
	GitCommands     int            // count of git tool calls
}

// AnalyzeSession reads Fordjent logs for turn and tool call statistics.
// Since session directories are cleaned up after completion, we parse
// the stdout log which contains structured turn-complete messages.
func (h *Harness) AnalyzeSession(repo string, issueNum int) (*SessionAnalysis, error) {
	analysis := &SessionAnalysis{
		ToolCallsByType: make(map[string]int),
	}

	// Primary: parse Fordjent log for structured turn data
	logPath := filepath.Join(h.LocalDir, "logs", "fordjent-stdout.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse JSON log lines
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		// Count turns from turn-complete messages
		if msg, ok := entry["msg"].(string); ok && msg == "turn complete" {
			analysis.TurnCount++
			// Extract tool_calls count
			if tc, ok := entry["tool_calls"].(float64); ok {
				analysis.ToolCalls += int(tc)
			}
		}

		// Count LLM calls
		if msg, ok := entry["msg"].(string); ok && strings.Contains(msg, "llm") {
			analysis.LLMCalls++
		}

		// Count specific tool calls from log
		if tool, ok := entry["tool"].(string); ok {
			analysis.ToolCallsByType[tool]++
			switch tool {
			case "write_file":
				analysis.FileWrites++
			case "bash":
				analysis.BashCommands++
			case "git":
				analysis.GitCommands++
			case "forgejo_create_pr":
				analysis.HasPR = true
			}
		}

		// Detect commits
		if msg, ok := entry["msg"].(string); ok && strings.Contains(msg, "commit") {
			analysis.HasCommit = true
		}

		// Count PR creation
		if msg, ok := entry["msg"].(string); ok && strings.Contains(msg, "PR created") {
			analysis.HasPR = true
		}
	}

	analysis.ToolCallsTotal = analysis.ToolCalls // total from turn-complete messages
	// Also sum up known tool types for cross-check
	known := analysis.FileWrites + analysis.BashCommands + analysis.GitCommands
	if known > analysis.ToolCallsTotal {
		analysis.ToolCallsTotal = known
	}

	return analysis, nil
}

// FindLatestSession finds the most recent session directory for a repo.
func (h *Harness) FindLatestSession(repo string) (string, error) {
	issuesDir := filepath.Join(h.LocalDir, "fordjent-work", repo, "issues")
	entries, err := os.ReadDir(issuesDir)
	if err != nil {
		return "", fmt.Errorf("find sessions: %w", err)
	}

	var latest string
	var latestMod time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
			latest = filepath.Join(issuesDir, e.Name())
		}
	}
	if latest == "" {
		return "", fmt.Errorf("no sessions found for %s", repo)
	}
	return latest, nil
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
	body, err := h.doForgejoRequest("GET", "/repos/"+repo+"/issues?labels=fordjent%2Ffailed%3Aerror", nil)
	if err != nil {
		return 0, fmt.Errorf("failed to query issues: %w", err)
	}

	var issues []map[string]interface{}
	if err := json.Unmarshal([]byte(body), &issues); err != nil {
		// Paginated response — try extracting from "data" field
		var pageResp map[string]interface{}
		if err2 := json.Unmarshal([]byte(body), &pageResp); err2 == nil {
			if data, ok := pageResp["data"].([]interface{}); ok {
				for _, item := range data {
					if m, ok := item.(map[string]interface{}); ok {
						issues = append(issues, m)
					}
				}
			}
		}
		if len(issues) == 0 {
			return 0, fmt.Errorf("parse issues: %w", err)
		}
	}

	falseCount := 0
	for _, issue := range issues {
		state, _ := issue["state"].(string)

		// Check if this issue has a merged PR
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
		}
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

// ReadLogFile reads the Fordjent stdout log file for analysis.
func (h *Harness) ReadLogFile() (string, error) {
	logPath := filepath.Join(h.LocalDir, "logs", "fordjent-stdout.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return "", fmt.Errorf("failed to read fordjent log: %w", err)
	}
	return string(data), nil
}

// ReadMemoryFile reads a specific session's memory.jsonl.
func (h *Harness) ReadMemoryFile(sessionDir string) (string, error) {
	memoryPath := filepath.Join(sessionDir, "memory.jsonl")
	data, err := os.ReadFile(memoryPath)
	if err != nil {
		return "", fmt.Errorf("failed to read memory: %w", err)
	}
	return string(data), nil
}

// ParseMemoryFile extracts structured data from memory.jsonl lines.
func ParseMemoryFile(content string) (turns int, toolCalls map[string]int, toolErrors int) {
	toolCalls = make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if role, ok := entry["role"].(string); ok && role == "assistant" {
			turns++
		}
		if tcs, ok := entry["tool_calls"].([]interface{}); ok {
			for _, tc := range tcs {
				if tcMap, ok := tc.(map[string]interface{}); ok {
					if fn, ok := tcMap["function"].(map[string]interface{}); ok {
						if name, ok := fn["name"].(string); ok {
							toolCalls[name]++
						}
					}
				}
			}
		}
		if role, ok := entry["role"].(string); ok && role == "tool" {
			if content, ok := entry["content"].(string); ok {
				lower := strings.ToLower(content)
				if strings.HasPrefix(lower, "error") || strings.Contains(lower, "failed") {
					toolErrors++
				}
			}
		}
	}
	return turns, toolCalls, toolErrors
}