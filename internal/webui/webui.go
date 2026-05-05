// Package webui provides a small HTML dashboard for Fordjent observability.
// It is served alongside the webhook router on /admin.
package webui

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/metrics"
	_ "modernc.org/sqlite"
)

// Handler returns an http.Handler for the admin dashboard.
func Handler(cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin" && r.URL.Path != "/admin/" {
			http.NotFound(w, r)
			return
		}
		data := buildDashboardData(cfg)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := adminTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// DashboardData is passed to the HTML template.
type DashboardData struct {
	Now           string
	Metrics       map[string]interface{}
	Sessions      []SessionRow
	RecentTrans   []TransitionRow
	RecentCosts   []CostRow
	Error         string
}

type SessionRow struct {
	Key        string
	Repo       string
	IssueNum   int
	State      string
	Reason     string
	UpdatedAt  string
	MemoryPath string
}

type TransitionRow struct {
	SessionKey string
	FromState  string
	ToState    string
	Reason     string
	OccurredAt string
}

type CostRow struct {
	SessionKey   string
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	Timestamp    string
}

func buildDashboardData(cfg *config.Config) DashboardData {
	d := DashboardData{
		Now:     time.Now().UTC().Format(time.RFC3339),
		Metrics: metrics.Snapshot(),
	}

	if cfg.Agent.WorkDir == "" {
		d.Error = "WorkDir not configured — no DB data available"
		return d
	}

	lifecycleDB := filepath.Join(cfg.Agent.WorkDir, "lifecycle.db")
	if rows, err := queryLatestSessions(lifecycleDB, cfg.Agent.WorkDir); err == nil {
		d.Sessions = rows
	} else {
		d.Error = err.Error()
	}

	if rows, err := queryRecentTransitions(lifecycleDB); err == nil {
		d.RecentTrans = rows
	}

	costDB := filepath.Join(cfg.Agent.WorkDir, "costs.db")
	if rows, err := queryRecentCosts(costDB); err == nil {
		d.RecentCosts = rows
	}

	return d
}

func queryLatestSessions(dbPath, workDir string) ([]SessionRow, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT session_key, to_state, reason, occurred_at
		FROM session_transitions t1
		WHERE occurred_at = (
			SELECT MAX(occurred_at) FROM session_transitions t2 WHERE t2.session_key = t1.session_key
		)
		ORDER BY occurred_at DESC
		LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionRow
	for rows.Next() {
		var s, state, reason, ts string
		_ = rows.Scan(&s, &state, &reason, &ts)
		parts := strings.Split(s, "/")
		repo := s
		issueNum := 0
		if len(parts) >= 3 {
			repo = strings.Join(parts[:len(parts)-2], "/")
			// parts[len(parts)-2] is "issues" or "pulls", parts[len(parts)-1] is number
			fmt.Sscanf(parts[len(parts)-1], "%d", &issueNum)
		}
		memPath := ""
		if workDir != "" {
			memPath = filepath.Join(workDir, s, "memory.jsonl")
		}
		out = append(out, SessionRow{
			Key:        s,
			Repo:       repo,
			IssueNum:   issueNum,
			State:      state,
			Reason:     reason,
			UpdatedAt:  ts,
			MemoryPath: memPath,
		})
	}
	return out, rows.Err()
}

func queryRecentTransitions(dbPath string) ([]TransitionRow, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT session_key, from_state, to_state, reason, occurred_at
		FROM session_transitions
		ORDER BY occurred_at DESC
		LIMIT 50
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TransitionRow
	for rows.Next() {
		var r TransitionRow
		_ = rows.Scan(&r.SessionKey, &r.FromState, &r.ToState, &r.Reason, &r.OccurredAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func queryRecentCosts(dbPath string) ([]CostRow, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT session_key, provider, model, input_tokens, output_tokens, cost_usd, timestamp
		FROM usage
		ORDER BY timestamp DESC
		LIMIT 50
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CostRow
	for rows.Next() {
		var r CostRow
		_ = rows.Scan(&r.SessionKey, &r.Provider, &r.Model, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.Timestamp)
		out = append(out, r)
	}
	return out, rows.Err()
}

var adminTemplate = template.Must(template.New("admin").Parse(`<!DOCTYPE html>
<html>
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>Fordjent Admin</title>
	<style>
		body { font-family: system-ui, -apple-system, sans-serif; margin: 0; padding: 2rem; background: #f6f8fa; color: #24292f; }
		h1, h2 { margin-top: 0; }
		.card { background: #fff; border-radius: 8px; padding: 1.5rem; margin-bottom: 1.5rem; box-shadow: 0 1px 3px rgba(0,0,0,0.1); }
		table { width: 100%; border-collapse: collapse; font-size: 0.875rem; }
		th, td { text-align: left; padding: 0.5rem 0.75rem; border-bottom: 1px solid #d0d7de; }
		th { background: #f6f8fa; font-weight: 600; }
		tr:hover { background: #f6f8fa; }
		.badge { display: inline-block; padding: 0.125rem 0.5rem; border-radius: 9999px; font-size: 0.75rem; font-weight: 600; }
		.badge-working { background: #dafbe1; color: #1a7f37; }
		.badge-blocked { background: #fff8c5; color: #7d4e00; }
		.badge-failed { background: #ffebe9; color: #cf222e; }
		.badge-completed { background: #ddf4ff; color: #0969da; }
		.badge-pr_created { background: #e6dffc; color: #5a32a3; }
		.metrics-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: 1rem; }
		.metric { background: #f6f8fa; padding: 1rem; border-radius: 6px; text-align: center; }
		.metric-value { font-size: 1.5rem; font-weight: 700; color: #0969da; }
		.metric-label { font-size: 0.75rem; color: #57606a; margin-top: 0.25rem; }
		.error { background: #ffebe9; color: #cf222e; padding: 1rem; border-radius: 6px; }
		.mono { font-family: ui-monospace, SFMono-Regular, monospace; font-size: 0.8rem; }
		a { color: #0969da; text-decoration: none; }
		a:hover { text-decoration: underline; }
		.refresh { float: right; font-size: 0.875rem; }
	</style>
</head>
<body>
	<h1>Fordjent Admin <span class="mono">{{.Now}}</span>
		<a class="refresh" href="/admin">↻ Refresh</a>
	</h1>

	{{if .Error}}
	<div class="card error">{{.Error}}</div>
	{{end}}

	<div class="card">
		<h2>Metrics</h2>
		<div class="metrics-grid">
			{{range $k, $v := .Metrics}}
			<div class="metric">
				<div class="metric-value">{{$v}}</div>
				<div class="metric-label">{{$k}}</div>
			</div>
			{{end}}
		</div>
	</div>

	<div class="card">
		<h2>Sessions (latest state)</h2>
		<table>
			<thead>
				<tr>
					<th>Session</th>
					<th>Repo</th>
					<th>Issue/PR</th>
					<th>State</th>
					<th>Reason</th>
					<th>Updated</th>
					<th>Memory</th>
				</tr>
			</thead>
			<tbody>
			{{range .Sessions}}
				<tr>
					<td class="mono">{{.Key}}</td>
					<td>{{.Repo}}</td>
					<td>#{{.IssueNum}}</td>
					<td><span class="badge badge-{{.State}}">{{.State}}</span></td>
					<td>{{.Reason}}</td>
					<td class="mono">{{.UpdatedAt}}</td>
					<td class="mono">{{.MemoryPath}}</td>
				</tr>
			{{end}}
			</tbody>
		</table>
	</div>

	<div class="card">
		<h2>Recent Transitions</h2>
		<table>
			<thead>
				<tr><th>Session</th><th>From</th><th>To</th><th>Reason</th><th>Time</th></tr>
			</thead>
			<tbody>
			{{range .RecentTrans}}
				<tr>
					<td class="mono">{{.SessionKey}}</td>
					<td><span class="badge badge-{{.FromState}}">{{.FromState}}</span></td>
					<td><span class="badge badge-{{.ToState}}">{{.ToState}}</span></td>
					<td>{{.Reason}}</td>
					<td class="mono">{{.OccurredAt}}</td>
				</tr>
			{{end}}
			</tbody>
		</table>
	</div>

	<div class="card">
		<h2>Recent Cost Records</h2>
		<table>
			<thead>
				<tr><th>Session</th><th>Provider</th><th>Model</th><th>Input</th><th>Output</th><th>Cost USD</th><th>Time</th></tr>
			</thead>
			<tbody>
			{{range .RecentCosts}}
				<tr>
					<td class="mono">{{.SessionKey}}</td>
					<td>{{.Provider}}</td>
					<td>{{.Model}}</td>
					<td>{{.InputTokens}}</td>
					<td>{{.OutputTokens}}</td>
					<td>${{printf "%.6f" .CostUSD}}</td>
					<td class="mono">{{.Timestamp}}</td>
				</tr>
			{{end}}
			</tbody>
		</table>
	</div>
</body>
</html>
`))
