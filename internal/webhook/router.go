package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/webui"
	_ "modernc.org/sqlite"
)

// Router receives Forgejo webhooks, validates them, normalizes events,
// and publishes to the event bus.
type Router struct {
	cfg       *config.Config
	bus       *event.Bus
	logger    *slog.Logger
	mux       *http.ServeMux
	server    *http.Server
	mu        sync.Mutex
	shuttingDown bool
	lc        *lifecycle.Lifecycle // optional: set post-construction for webhook delivery tracking
	forgejo   *forgejo.Client      // optional: set post-construction for PR state checks
}

func NewRouter(cfg *config.Config, bus *event.Bus, logger *slog.Logger) *Router {
	r := &Router{
		cfg:    cfg,
		bus:    bus,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	r.mux.HandleFunc("/acp/v1/events", r.handleWebhook)
	r.mux.HandleFunc("/acp/v1/test-merge-webhook", r.handleTestMergeWebhook)
	r.mux.HandleFunc("/healthz", r.handleHealth)
	r.mux.HandleFunc("/readyz", r.handleReadyz)
	r.mux.HandleFunc("/metrics", metrics.Handler())
	r.mux.HandleFunc("/status", r.handleStatus)
	r.mux.HandleFunc("/tokens-per-minute", r.handleTokensPerMinute)
	r.mux.HandleFunc("/activity", r.handleActivity)
	r.mux.HandleFunc("/trace/", r.handleTrace) // /trace/{owner}/{repo}/{issues|pulls}/{N}
	r.mux.HandleFunc("/acp/v1/stream", r.handleStream)
	r.mux.HandleFunc("/dashboard", r.handleDashboard)
	r.mux.Handle("/admin", webui.Handler(cfg))
	r.mux.Handle("/admin/", webui.Handler(cfg))

	return r
}

// Handler returns the http.Handler for the router's mux.
func (r *Router) Handler() http.Handler {
	return r.mux
}

// SetLifecycle wires the lifecycle tracker for webhook delivery logging.
func (r *Router) SetLifecycle(lc *lifecycle.Lifecycle) {
	r.lc = lc
}

// SetForgejoClient wires the Forgejo API client for PR state checks.
func (r *Router) SetForgejoClient(client *forgejo.Client) {
	r.forgejo = client
}

// SetShutdown marks the router as shutting down. New webhooks will receive 503.
func (r *Router) SetShutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shuttingDown = true
}

func (r *Router) isShuttingDown() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shuttingDown
}

func (r *Router) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (r *Router) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if r.isShuttingDown() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ready")
}

func (r *Router) handleStatus(w http.ResponseWriter, req *http.Request) {
	since := parseSince(req)

	resp := map[string]interface{}{"now": time.Now().UTC().Format(time.RFC3339)}

	if r.cfg.Agent.WorkDir != "" {
		// Cost summary
		costDB := filepath.Join(r.cfg.Agent.WorkDir, "costs.db")
		if data, err := queryCostDB(costDB); err == nil {
			resp["costs"] = data
		}

		// Per-model breakdown
		if data, err := queryCostDBPerModel(costDB, since); err == nil {
			resp["by_model"] = data
		}

		// Per-session-per-model breakdown
		if data, err := queryCostDBBySessionModel(costDB, since); err == nil {
			resp["by_session_model"] = data
		}

		// Lifecycle summary
		lifecycleDB := filepath.Join(r.cfg.Agent.WorkDir, "lifecycle.db")
		if data, err := queryLifecycleDB(lifecycleDB); err == nil {
			resp["lifecycle"] = data
		}
	}

	resp["metrics"] = metrics.Snapshot()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func (r *Router) handleTokensPerMinute(w http.ResponseWriter, req *http.Request) {
	resp := map[string]interface{}{"now": time.Now().UTC().Format(time.RFC3339)}

	hours := 1
	if hStr := req.URL.Query().Get("hours"); hStr != "" {
		if h, err := fmt.Sscanf(hStr, "%d", &hours); err != nil || h != 1 || hours < 1 {
			hours = 1
		}
	}

	if r.cfg.Agent.WorkDir != "" {
		costDB := filepath.Join(r.cfg.Agent.WorkDir, "costs.db")
		if data, err := queryTokensPerMinute(costDB, hours); err == nil {
			resp["data"] = data
		} else {
			resp["error"] = err.Error()
		}
	} else {
		resp["error"] = "WorkDir not configured"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func (r *Router) handleActivity(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lifecycleDB := filepath.Join(r.cfg.Agent.WorkDir, "lifecycle.db")
	if lifecycleDB == "" {
		http.Error(w, "no workdir configured", http.StatusInternalServerError)
		return
	}

	db, err := sql.Open("sqlite", lifecycleDB+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintln(w, "<html><head><title>Fordjent Activity</title>")
	fmt.Fprintln(w, "<style>body{font-family:monospace;max-width:900px;margin:2em auto}table{width:100%;border-collapse:collapse}th,td{padding:4px 8px;text-align:left;border-bottom:1px solid #eee}th{background:#f5f5f5}</style>")
	fmt.Fprintln(w, "</head><body><h1>Fordjent Activity Feed</h1>")

	// Recent webhook deliveries
	fmt.Fprintln(w, "<h2>Recent Webhooks</h2><table><tr><th>Time</th><th>Type</th><th>Action</th><th>Repo</th><th>#</th><th>Sender</th><th>Status</th></tr>")
	rows, err := db.Query("SELECT occurred_at, event_type, action, repository, number, sender, status FROM webhook_deliveries ORDER BY occurred_at DESC LIMIT 30")
	if err == nil && rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ts, et, act, repo, sender, status string
			var num int
			rows.Scan(&ts, &et, &act, &repo, &num, &sender, &status)
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td>%s</td></tr>\n",
				html.EscapeString(ts), html.EscapeString(et), html.EscapeString(act),
				html.EscapeString(repo), num, html.EscapeString(sender), html.EscapeString(status))
		}
	}
	fmt.Fprintln(w, "</table>")

	// Recent lifecycle transitions
	fmt.Fprintln(w, "<h2>Recent Sessions</h2><table><tr><th>Time</th><th>Session</th><th>From</th><th>To</th><th>Reason</th></tr>")
	rows2, err := db.Query("SELECT occurred_at, session_key, from_state, to_state, reason FROM session_transitions ORDER BY occurred_at DESC LIMIT 30")
	if err == nil && rows2 != nil {
		defer rows2.Close()
		for rows2.Next() {
			var ts, sk, from, to, reason string
			rows2.Scan(&ts, &sk, &from, &to, &reason)
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				html.EscapeString(ts), html.EscapeString(sk), html.EscapeString(from),
				html.EscapeString(to), html.EscapeString(reason))
		}
	}
	fmt.Fprintln(w, "</table></body></html>")
}

func (r *Router) handleStream(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lc := r.lc
	if lc == nil {
		http.Error(w, "lifecycle not configured", http.StatusServiceUnavailable)
		return
	}

	mgr := lc.SSEManager()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	lastEventID := req.Header.Get("Last-Event-ID")
	if lastEventID != "" {
		missed := mgr.ReplaySince(lastEventID)
		for _, evt := range missed {
			fmt.Fprint(w, lifecycle.EncodeSSEEvent(evt))
		}
		flusher.Flush()
	}

	ch := mgr.Subscribe()
	defer mgr.Unsubscribe(ch)

	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, lifecycle.EncodeSSEEvent(evt))
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

func queryCostDB(dbPath string) (map[string]interface{}, error) {
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	result := map[string]interface{}{}
	var totalSessions int
	_ = db.QueryRow("SELECT COUNT(DISTINCT session_key) FROM usage").Scan(&totalSessions)
	result["total_sessions"] = totalSessions

	var totalTokens, totalCost int64
	_ = db.QueryRow("SELECT COALESCE(SUM(input_tokens),0)+COALESCE(SUM(output_tokens),0), COALESCE(SUM(cost_usd*1000000),0) FROM usage").Scan(&totalTokens, &totalCost)
	result["total_tokens"] = totalTokens
	result["total_cost_usd"] = float64(totalCost) / 1e6

	recent := []map[string]interface{}{}
	rows, err := db.Query("SELECT session_key, provider, model, input_tokens, output_tokens, cost_usd, created_at FROM usage ORDER BY created_at DESC LIMIT 20")
	if err == nil && rows != nil {
		defer rows.Close()
		for rows.Next() {
			var s, p, m string
			var it, ot int
			var cost float64
			var ts string
			_ = rows.Scan(&s, &p, &m, &it, &ot, &cost, &ts)
			recent = append(recent, map[string]interface{}{
				"session_key": s, "provider": p, "model": m,
				"input_tokens": it, "output_tokens": ot, "cost_usd": cost, "timestamp": ts,
			})
		}
	}
	result["recent_records"] = recent

	return result, nil
}

func parseSince(req *http.Request) time.Time {
	sinceStr := req.URL.Query().Get("since")
	if sinceStr == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, sinceStr)
	if err != nil {
		return time.Time{}
	}
	return t
}

func queryCostDBPerModel(dbPath string, since time.Time) ([]map[string]interface{}, error) {
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := `
		SELECT provider, model, COUNT(*) as calls,
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_usd), 0)
		FROM usage
		%s
		GROUP BY provider, model
		ORDER BY total_tokens DESC
	`
	var rows *sql.Rows
	if since.IsZero() {
		rows, err = db.Query(fmt.Sprintf(query, ""))
	} else {
		rows, err = db.Query(fmt.Sprintf(query, "WHERE created_at >= ? "), since.Format("2006-01-02 15:04:05"))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var provider, model string
		var calls, inputTokens, outputTokens, totalTokens int64
		var costUSD float64
		if err := rows.Scan(&provider, &model, &calls, &inputTokens, &outputTokens, &totalTokens, &costUSD); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"provider": provider, "model": model, "calls": calls,
			"input_tokens": inputTokens, "output_tokens": outputTokens,
			"total_tokens": totalTokens, "cost_usd": costUSD,
		})
	}
	return out, rows.Err()
}

func queryCostDBBySessionModel(dbPath string, since time.Time) ([]map[string]interface{}, error) {
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := `
		SELECT session_key, provider, model, COUNT(*) as calls,
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_usd), 0)
		FROM usage
		%s
		GROUP BY session_key, provider, model
		ORDER BY session_key, total_tokens DESC
	`
	var rows *sql.Rows
	if since.IsZero() {
		rows, err = db.Query(fmt.Sprintf(query, ""))
	} else {
		rows, err = db.Query(fmt.Sprintf(query, "WHERE created_at >= ? "), since.Format("2006-01-02 15:04:05"))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var sessionKey, provider, model string
		var calls, inputTokens, outputTokens, totalTokens int64
		var costUSD float64
		if err := rows.Scan(&sessionKey, &provider, &model, &calls, &inputTokens, &outputTokens, &totalTokens, &costUSD); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"session_key": sessionKey, "provider": provider, "model": model, "calls": calls,
			"input_tokens": inputTokens, "output_tokens": outputTokens,
			"total_tokens": totalTokens, "cost_usd": costUSD,
		})
	}
	return out, rows.Err()
}

func queryLifecycleDB(dbPath string) (map[string]interface{}, error) {
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	result := map[string]interface{}{}
	var active, failed int
	_ = db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT session_key, MAX(occurred_at) AS max_at
			FROM session_transitions
			GROUP BY session_key
		) grouped
		JOIN session_transitions t
			ON t.session_key = grouped.session_key AND t.occurred_at = grouped.max_at
		WHERE t.to_state = 'working'
	`).Scan(&active)
	_ = db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT session_key, MAX(occurred_at) AS max_at
			FROM session_transitions
			GROUP BY session_key
		) grouped
		JOIN session_transitions t
			ON t.session_key = grouped.session_key AND t.occurred_at = grouped.max_at
		WHERE t.to_state LIKE 'failed%'
	`).Scan(&failed)
	result["active_sessions"] = active
	result["failed_sessions"] = failed

	recent := []map[string]interface{}{}
	rows, err := db.Query("SELECT session_key, from_state, to_state, occurred_at FROM session_transitions ORDER BY occurred_at DESC LIMIT 20")
	if err == nil && rows != nil {
		defer rows.Close()
		for rows.Next() {
			var s, fromSt, toSt, ts string
			_ = rows.Scan(&s, &fromSt, &toSt, &ts)
			recent = append(recent, map[string]interface{}{
				"session_key": s, "from_state": fromSt, "to_state": toSt, "timestamp": ts,
			})
		}
	}
	result["recent_transitions"] = recent

	// Recent turn progress
	turns := []map[string]interface{}{}
	tRows, err := db.Query("SELECT session_key, turn, tool_calls, latency_ms, tokens_in, tokens_out, error, occurred_at FROM session_turns ORDER BY occurred_at DESC LIMIT 30")
	if err == nil && tRows != nil {
		defer tRows.Close()
		for tRows.Next() {
			var sk string; var turn, tc, lat, tin, tout int; var errMsg, ts string
			_ = tRows.Scan(&sk, &turn, &tc, &lat, &tin, &tout, &errMsg, &ts)
			entry := map[string]interface{}{"session_key": sk, "turn": turn, "tool_calls": tc, "latency_ms": lat, "tokens_in": tin, "tokens_out": tout, "timestamp": ts}
			if errMsg != "" { entry["error"] = errMsg }
			turns = append(turns, entry)
		}
	}
	result["recent_turns"] = turns

	return result, nil
}

func queryTokensPerMinute(dbPath string, hours int) ([]map[string]interface{}, error) {
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// Read raw rows — SQLite strftime can't parse Go RFC3339Nano timestamps.
	// We parse and group in Go.
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	rows, err := db.Query(`
		SELECT created_at, input_tokens, output_tokens, total_tokens
		FROM usage
		WHERE created_at >= ?
		ORDER BY created_at DESC
		LIMIT 5000
	`, since.Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type agg struct {
		inputTokens  int64
		outputTokens int64
		totalTokens  int64
		calls        int64
	}
	buckets := make(map[string]*agg)

	for rows.Next() {
		var tsStr string
		var inTok, outTok, totalTok int64
		_ = rows.Scan(&tsStr, &inTok, &outTok, &totalTok)
		ts, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			continue
		}
		minute := ts.UTC().Format("2006-01-02 15:04")
		b := buckets[minute]
		if b == nil {
			b = &agg{}
			buckets[minute] = b
		}
		b.inputTokens += inTok
		b.outputTokens += outTok
		b.totalTokens += totalTok
		b.calls++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sort minutes descending
	var minutes []string
	for m := range buckets {
		minutes = append(minutes, m)
	}
	sort.Strings(minutes)
	for i, j := 0, len(minutes)-1; i < j; i, j = i+1, j-1 {
		minutes[i], minutes[j] = minutes[j], minutes[i]
	}

	var out []map[string]interface{}
	for _, m := range minutes {
		b := buckets[m]
		out = append(out, map[string]interface{}{
			"minute":        m,
			"input_tokens":  b.inputTokens,
			"output_tokens": b.outputTokens,
			"total_tokens":  b.totalTokens,
			"calls":         b.calls,
		})
	}
	return out, nil
}
func (r *Router) ListenAndServe(ctx context.Context, addr string) error {
	r.server = &http.Server{
		Addr:              addr,
		Handler:           r.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.server.Shutdown(shutdownCtx)
	}()

	return r.server.ListenAndServe()
}

func (r *Router) handleWebhook(w http.ResponseWriter, req *http.Request) {
	if r.isShuttingDown() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read body
	body, err := io.ReadAll(io.LimitReader(req.Body, 10<<20)) // 10MB max
	if err != nil {
		r.logger.Error("failed to read body", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	// Validate HMAC signature
	if !r.validateSignature(body, req.Header.Get("X-Hub-Signature-256")) {
		r.logger.Warn("invalid webhook signature")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Determine event type from Forgejo headers
	eventType := req.Header.Get("X-Forgejo-Event")
	if eventType == "" {
		eventType = req.Header.Get("X-Gitea-Event")
	}
	if eventType == "" {
		r.logger.Warn("missing event type header")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Parse the webhook payload
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		r.logger.Error("failed to parse payload", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Extract action
	action, _ := payload["action"].(string)

	// Verbose logging for every received event (before filtering)
	repoName := ""
	if repo, ok := payload["repository"].(map[string]interface{}); ok {
		if full, ok := repo["full_name"].(string); ok {
			repoName = full
		}
	}
	num := 0
	if issue, ok := payload["issue"].(map[string]interface{}); ok {
		if n, ok := issue["number"].(float64); ok {
			num = int(n)
		}
	}
	if pr, ok := payload["pull_request"].(map[string]interface{}); ok {
		if n, ok := pr["number"].(float64); ok {
			num = int(n)
		}
	}
	r.logger.Info("webhook received",
		"event_type", eventType,
		"action", action,
		"repository", repoName,
		"number", num,
	)

	evt, err := r.normalizeEvent(eventType, action, payload)
	if err != nil {
		r.logger.Warn("unhandled event type", "type", eventType, "action", action, "error", err)
		if r.lc != nil {
			r.lc.RecordDelivery(req.Context(), eventType, action, repoName, 0, "", "ignored", err)
		}
		w.WriteHeader(http.StatusOK) // Ack but ignore
		fmt.Fprintln(w, "ignored")
		return
	}

	// Forgejo v9 does not include is_pull_request or pull_request in
	// issue_comment webhook payloads, so comments on PRs arrive with prNum==0
	// and session key repo/issues/N. Fix the session key by checking via API.
	if evt.PRNumber == 0 && evt.IssueNumber > 0 && strings.HasPrefix(string(evt.Type), "issue_comment") && r.forgejo != nil {
		issue, err := r.forgejo.GetIssue(req.Context(), evt.Repository, evt.IssueNumber)
		if err == nil && issue.PullRequest.IsPR() {
			r.logger.Info("corrected issue_comment session key to pulls",
				"issue", evt.IssueNumber, "repo", evt.Repository)
			evt.PRNumber = evt.IssueNumber
			evt.SessionKey = fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.IssueNumber)
		}
	}

	metrics.IncEvents()
	if r.cfg.Security.FilterAgentEvents && r.isAgentEvent(payload) {
		r.logger.Info("filtered agent-originated event", "event_id", evt.ID)
		if r.lc != nil {
			r.lc.RecordDelivery(req.Context(), string(evt.Type), evt.Action, evt.Repository, evt.IssueNumber, evt.Sender, "filtered", nil)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "filtered")
		return
	}

	// Skip issue_comment events on closed/merged PRs to prevent runaway loops.
	// Cost summary comments on completed PRs were the #1 cause of token burn.
	if evt.Type == event.Type("issue_comment.created") && evt.PRNumber > 0 && r.forgejo != nil {
		pr, err := r.forgejo.GetPR(req.Context(), evt.Repository, evt.PRNumber)
		if err == nil && pr.State == "closed" {
			r.logger.Info("skipped comment on closed PR", "event_id", evt.ID, "pr", evt.PRNumber)
			if r.lc != nil {
				r.lc.RecordDelivery(req.Context(), string(evt.Type), evt.Action, evt.Repository, evt.IssueNumber, evt.Sender, "skipped_closed_pr", nil)
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "skipped_closed_pr")
			return
		}
	}

	r.logger.Info("received event",
		"event_id", evt.ID,
		"type", evt.Type,
		"repository", evt.Repository,
		"sender", evt.Sender,
		"session_key", evt.SessionKey,
	)

	// Publish to event bus
	r.bus.Publish(req.Context(), evt)

	// Record webhook delivery for tracking
	if r.lc != nil {
		r.lc.RecordDelivery(req.Context(), string(evt.Type), evt.Action, evt.Repository, evt.IssueNumber, evt.Sender, "accepted", nil)
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"event_id": "%s", "status": "accepted"}`, evt.ID)
}

// handleTestMergeWebhook accepts a synthetic pull_request.closed payload for
// manual testing of the scheduler/merge-event path. No HMAC validation.
func (r *Router) handleTestMergeWebhook(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, 10<<20))
	if err != nil {
		r.logger.Error("failed to read test body", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		r.logger.Error("failed to parse test payload", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	action, _ := payload["action"].(string)
	repoName := ""
	if repo, ok := payload["repository"].(map[string]interface{}); ok {
		if full, ok := repo["full_name"].(string); ok {
			repoName = full
		}
	}
	num := 0
	if pr, ok := payload["pull_request"].(map[string]interface{}); ok {
		if n, ok := pr["number"].(float64); ok {
			num = int(n)
		}
	}

	r.logger.Info("test webhook received",
		"event_type", "pull_request",
		"action", action,
		"repository", repoName,
		"number", num,
	)

	evt, err := r.normalizeEvent("pull_request", action, payload)
	if err != nil {
		r.logger.Warn("unhandled test event", "action", action, "error", err)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ignored")
		return
	}

	metrics.IncEvents()
	if r.cfg.Security.FilterAgentEvents && r.isAgentEvent(payload) {
		r.logger.Info("filtered agent-originated test event", "event_id", evt.ID)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "filtered")
		return
	}

	r.logger.Info("test event accepted",
		"event_id", evt.ID,
		"type", evt.Type,
		"repository", evt.Repository,
		"session_key", evt.SessionKey,
	)

	r.bus.Publish(req.Context(), evt)

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"event_id": "%s", "status": "accepted"}`, evt.ID)
}

func (r *Router) validateSignature(body []byte, sig string) bool {
	if r.cfg.Webhook.Secret == "" {
		return true // No secret configured, skip validation
	}
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(r.cfg.Webhook.Secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	// Format: "sha256=<hex>"
	if strings.HasPrefix(sig, "sha256=") {
		sig = sig[7:]
	}
	return hmac.Equal([]byte(sig), []byte(expected))
}

func (r *Router) normalizeEvent(eventType, action string, payload map[string]interface{}) (*event.Event, error) {
	extractRepo := func() string {
		if repo, ok := payload["repository"].(map[string]interface{}); ok {
			if full, ok := repo["full_name"].(string); ok {
				return full
			}
		}
		return ""
	}
	extractSender := func() string {
		if sender, ok := payload["sender"].(map[string]interface{}); ok {
			if login, ok := sender["login"].(string); ok {
				return login
			}
		}
		return ""
	}
	extractIssueNum := func() int {
		if issue, ok := payload["issue"].(map[string]interface{}); ok {
			if num, ok := issue["number"].(float64); ok {
				return int(num)
			}
		}
		if pr, ok := payload["pull_request"].(map[string]interface{}); ok {
			if num, ok := pr["number"].(float64); ok {
				return int(num)
			}
		}
		return 0
	}
	extractPRNum := func() int {
		if _, ok := payload["pull_request"]; ok {
			return extractIssueNum()
		}
		// issue_comment on a PR has 'issue.is_pull_request: true'
		if issue, ok := payload["issue"].(map[string]interface{}); ok {
			if isPR, ok := issue["is_pull_request"].(bool); ok && isPR {
				return extractIssueNum()
			}
		}
		return 0
	}

	repo := extractRepo()
	sender := extractSender()
	issueNum := extractIssueNum()
	prNum := extractPRNum()

	// Detect merged PRs: Forgejo sends action="closed" with merged=true in the payload
	if eventType == "pull_request" && action == "closed" {
		if pr, ok := payload["pull_request"].(map[string]interface{}); ok {
			if merged, ok := pr["merged"].(bool); ok && merged {
				action = "merged"
			}
		}
	}

	var typ event.Type
	switch eventType {
	case "issues":
		typ = event.Type("issues." + action)
	case "issue_comment":
		typ = event.Type("issue_comment." + action)
	case "pull_request":
		typ = event.Type("pull_request." + action)
	case "pull_request_review_comment":
		typ = event.Type("pull_request_review_comment." + action)
	case "push":
		typ = event.Push
	default:
		return nil, fmt.Errorf("unsupported event type: %s", eventType)
	}

	evt := event.NewEvent(typ, repo, issueNum, prNum, sender, action)
	evt.Payload = payload

	// Compute session key: repository/issues/number or repository/pulls/number
	if prNum > 0 {
		evt.SessionKey = fmt.Sprintf("%s/pulls/%d", repo, prNum)
	} else if issueNum > 0 {
		evt.SessionKey = fmt.Sprintf("%s/issues/%d", repo, issueNum)
	} else {
		evt.SessionKey = fmt.Sprintf("%s/push/%d", repo, time.Now().UnixNano())
	}

	return evt, nil
}

// isAgentEvent detects events originating from the agent itself by checking
// commit message prefixes, sender identity, or a hidden HTML comment marker
// in the body of comments, issues, or PRs. This prevents infinite loops where
// the agent responds to its own comments.
//
// IMPORTANT: Bot-created issues (issues.* events without a comment key) are NOT
// filtered by sender, because the agent legitimately creates sub-issues that
// need downstream sessions spawned.
func (r *Router) isAgentEvent(payload map[string]interface{}) bool {
	// NEVER filter push events — they represent actual code changes
	// that the scheduler and scaffold systems need to process.
	if _, hasRef := payload["ref"]; hasRef {
		if _, hasCommits := payload["commits"]; hasCommits {
			return false // git push — always pass through
		}
	}
	// NEVER filter pull_request closed events that represent a merge — the
	// scheduler depends on seeing these.
	if action, ok := payload["action"].(string); ok && action == "closed" {
		if pr, ok := payload["pull_request"].(map[string]interface{}); ok {
			if merged, ok := pr["merged"].(bool); ok && merged {
				return false
			}
		}
	}

	// Allow implementer→PM ping comments through even if they come from
	// the bot sender. These use a special marker (<!-- ford-ping -->) that
	// signals the PM session should be re-activated to respond to the
	// implementer's question. Must be checked before the generic marker
	// and bot-sender filters below.
	if comment, ok := payload["comment"].(map[string]interface{}); ok {
		if body, ok := comment["body"].(string); ok {
			if strings.Contains(body, "<!-- ford-ping -->") {
				return false
			}
		}
	}

	marker := "<!-- ford -->"

	// Comment events (issue_comment, pull_request_review_comment):
	// Filter by body marker OR by bot sender. The scheduler posts
	// unblock comments WITH the marker, so this is safe.
	if comment, ok := payload["comment"].(map[string]interface{}); ok {
		if body, ok := comment["body"].(string); ok {
			if strings.Contains(body, marker) {
				return true
			}
		}
	}
	// Also filter comment events where the sender is the bot user.
	// This catches cost-summary comments and other auto-generated text.
	// IMPORTANT: only filter comments — bot-created issues and PRs MUST pass
	// through so their sessions can spawn (scaffold issues, sub-issues from PM).
	if _, isCommentEvent := payload["comment"]; isCommentEvent {
		if sender, ok := payload["sender"].(map[string]interface{}); ok {
			if login, ok := sender["login"].(string); ok {
				if login == "fordjent-bot" || login == "fordjent[bot]" {
					return true // bot comments never need agent processing
				}
			}
		}
	}

	// PR events: filter by marker in PR body only, EXCEPT for 'opened' action
	// which must pass through so reviewer sessions can inspect bot-created PRs.
	if pr, ok := payload["pull_request"].(map[string]interface{}); ok {
		action, _ := payload["action"].(string)
		if action != "opened" {
			if body, ok := pr["body"].(string); ok {
				if strings.Contains(body, marker) {
					return true
				}
			}
		}
	}

	// Issue events WITHOUT a comment key: these are issues.* (opened, closed, etc.)
	// Bot-created sub-issues must pass through so downstream sessions spawn.
	// Only filter if the issue body itself contains the hidden agent marker.
	if issue, ok := payload["issue"].(map[string]interface{}); ok {
		if _, isCommentEvent := payload["comment"]; !isCommentEvent {
			if body, ok := issue["body"].(string); ok {
				if strings.Contains(body, marker) {
					return true
				}
			}
		}
	}

	return false
}

// handleTrace

// handleDashboard serves a rich HTML status dashboard.
func (r *Router) handleDashboard(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	workDir := r.cfg.Agent.WorkDir
	if workDir == "" {
		http.Error(w, "WorkDir not configured", http.StatusServiceUnavailable)
		return
	}

	costDBPath := filepath.Join(workDir, "costs.db")
	lifecycleDBPath := filepath.Join(workDir, "lifecycle.db")

	// Gather data
	activeSessions := queryActiveSessions(lifecycleDBPath)
	costSummary, _ := queryCostDB(costDBPath)
	lifecycleSummary, _ := queryLifecycleDB(lifecycleDBPath)
	byModel, _ := queryCostDBPerModel(costDBPath, time.Time{})
	metricsSnap := metrics.Snapshot()

	// Render HTML
	fmt.Fprint(w, `<!DOCTYPE html>
<html><head>
<meta charset="utf-8">
<title>Fordjent Dashboard</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
*{box-sizing:border-box}body{font-family:system-ui,-apple-system,sans-serif;margin:0;padding:1em;background:#0d1117;color:#c9d1d9}
.cards{display:flex;flex-wrap:wrap;gap:.75em;margin-bottom:1em}
.card{flex:1 1 200px;background:#161b22;border:1px solid #30363d;border-radius:8px;padding:1em;text-align:center}
.card .value{font-size:2em;font-weight:700;color:#58a6ff}.card .label{font-size:.8em;color:#8b949e;margin-top:.3em}
.card .stat{font-size:.85em;color:#7ee787}.card .stat.warn{color:#d29922}.card .stat.err{color:#f85149}
table{width:100%;border-collapse:collapse;margin-bottom:1.5em}
th,td{padding:6px 10px;text-align:left;border-bottom:1px solid #21262d;font-size:.9em}
th{background:#161b22;color:#8b949e;position:sticky;top:0}
.section{margin-bottom:2em}.section h2{color:#f0f6fc;border-bottom:1px solid #30363d;padding-bottom:.3em;font-size:1.1em}
.tag{display:inline-block;padding:1px 6px;border-radius:4px;font-size:.8em;font-weight:600}
.tag-green{background:#238636;color:#fff}.tag-amber{background:#9e6a03;color:#fff}.tag-red{background:#da3633;color:#fff}
.role-pm{color:#818cf8}.role-dev{color:#4ade80}.role-qa{color:#fbbf24}
a{color:#58a6ff;text-decoration:none}a:hover{text-decoration:underline}
</style></head><body>
<h1>Fordjent Dashboard</h1>`)

	// Summary cards
	fmt.Fprint(w, `<div class="cards">`)

	activeCount := len(activeSessions)
	activeClass := ""
	if activeCount > 5 {
		activeClass = "warn"
	}
	fmt.Fprintf(w, `<div class="card"><div class="value">%d</div><div class="label">Active Sessions</div><div class="stat %s">in_progress</div></div>`, activeCount, activeClass)

	totalSessions := int64(0)
	if cs, ok := costSummary["total_sessions"]; ok {
		if v, ok := cs.(int64); ok {
			totalSessions = v
		}
	}
	fmt.Fprintf(w, `<div class="card"><div class="value">%d</div><div class="label">Total Sessions</div></div>`, totalSessions)

	failedCount := int64(0)
	if ls, ok := lifecycleSummary["failed_sessions"]; ok {
		if v, ok := ls.(int64); ok {
			failedCount = v
		}
	}
	failedClass := ""
	if failedCount > 0 {
		failedClass = "err"
	}
	fmt.Fprintf(w, `<div class="card"><div class="value">%d</div><div class="label">Failed</div><div class="stat %s">needs attention</div></div>`, failedCount, failedClass)

	eventsTotal := int64(0)
	if m, ok := metricsSnap["fordjent_events_total"]; ok {
		if n, ok := m.(int64); ok {
			eventsTotal = n
		}
	}
	fmt.Fprintf(w, `<div class="card"><div class="value">%d</div><div class="label">Webhook Events</div></div>`, eventsTotal)

	fmt.Fprint(w, `</div>`)

	// Active sessions table
	if len(activeSessions) > 0 {
		fmt.Fprint(w, `<div class="section"><h2>Active Sessions</h2><table><tr><th>Session</th><th>Repo</th><th>Issue</th><th>State</th><th>Since</th></tr>`)
		for _, s := range activeSessions {
			roleClass := ""
			if strings.Contains(s["session_key"].(string), "/pulls/") {
				roleClass = "role-qa"
			} else {
				roleClass = "role-dev"
			}
			traceURL := fmt.Sprintf("/trace/%s", s["session_key"])
			fmt.Fprintf(w, `<tr><td><a href="%s" class="%s">%s</a></td><td>%s</td><td>%s</td><td><span class="tag tag-green">%s</span></td><td>%s</td></tr>`,
				html.EscapeString(traceURL), roleClass, html.EscapeString(fmt.Sprint(s["session_key"])),
				html.EscapeString(fmt.Sprint(s["repo"])), html.EscapeString(fmt.Sprint(s["issue_number"])),
				html.EscapeString(fmt.Sprint(s["to_state"])), html.EscapeString(fmt.Sprint(s["occurred_at"])))
		}
		fmt.Fprint(w, `</table></div>`)
	} else {
		fmt.Fprint(w, `<div class="section"><h2>Active Sessions</h2><p style="color:#8b949e">No active sessions.</p></div>`)
	}

	// Model usage
	if len(byModel) > 0 {
		fmt.Fprint(w, `<div class="section"><h2>Model Usage</h2><table><tr><th>Provider</th><th>Model</th><th>Calls</th><th>Tokens</th><th>Cost</th></tr>`)
		for _, m := range byModel {
			fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%v</td><td>%v</td><td>$%.4f</td></tr>`,
				html.EscapeString(fmt.Sprint(m["provider"])),
				html.EscapeString(fmt.Sprint(m["model"])),
				m["calls"], m["total_tokens"], m["cost_usd"])
		}
		fmt.Fprint(w, `</table></div>`)
	}

	// Recent failures
	recentFailures := queryRecentFailures(lifecycleDBPath, 20)
	if len(recentFailures) > 0 {
		fmt.Fprint(w, `<div class="section"><h2>Recent Failures</h2><table><tr><th>Session</th><th>Repo</th><th>Issue</th><th>Reason</th><th>Time</th></tr>`)
		for _, f := range recentFailures {
			sessionKey := fmt.Sprint(f["session_key"])
			traceURL := fmt.Sprintf("/trace/%s", sessionKey)
			fmt.Fprintf(w, `<tr><td><a href="%s">%s</a></td><td>%s</td><td>%s</td><td><span class="tag tag-red">%s</span></td><td>%s</td></tr>`,
				html.EscapeString(traceURL), html.EscapeString(sessionKey),
				html.EscapeString(fmt.Sprint(f["repo"])), html.EscapeString(fmt.Sprint(f["issue_number"])),
				html.EscapeString(fmt.Sprint(f["to_state"])), html.EscapeString(fmt.Sprint(f["occurred_at"])))
		}
		fmt.Fprint(w, `</table></div>`)
	}

	// Links
	fmt.Fprint(w, `<div class="section"><h2>Explore</h2>`)
	fmt.Fprint(w, `<p><a href="/status">/status</a> — JSON API &middot; <a href="/activity">/activity</a> — Feed &middot; <a href="/metrics">/metrics</a> — Prometheus &middot; <a href="/trace/">/trace/</a> — Session traces</p>`)
	fmt.Fprint(w, `</div>`)

	fmt.Fprint(w, `<p style="color:#8b949e;margin-top:2em;font-size:.85em">Fordjent agent &middot; `)
	fmt.Fprintf(w, `%s</p></body></html>`, time.Now().UTC().Format(time.RFC3339))
}

// queryActiveSessions returns sessions currently in the 'working' state.
func queryActiveSessions(dbPath string) []map[string]interface{} {
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT session_key, to_state, MAX(occurred_at) as occurred_at
		FROM session_transitions
		WHERE to_state = 'working'
		GROUP BY session_key
		ORDER BY occurred_at DESC
		LIMIT 50
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var key, toState, occurred string
		if err := rows.Scan(&key, &toState, &occurred); err != nil {
			continue
		}
		// Parse repo/issue from session key (format: owner/repo/issues/N)
		parts := strings.Split(key, "/")
		repoStr := ""
		issueStr := ""
		if len(parts) >= 4 {
			repoStr = parts[0] + "/" + parts[1]
			issueStr = parts[3]
		}
		out = append(out, map[string]interface{}{
			"session_key": key, "repo": repoStr, "issue_number": issueStr,
			"to_state": toState, "occurred_at": occurred,
		})
	}
	return out
}

// queryRecentFailures returns recent failed sessions.
func queryRecentFailures(dbPath string, limit int) []map[string]interface{} {
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT session_key, to_state, reason, MAX(occurred_at) as occurred_at
		FROM session_transitions
		WHERE to_state IN ('failed_max_turns', 'failed_error', 'failed', 'blocked')
		GROUP BY session_key
		ORDER BY occurred_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var key, toState, reason, occurred string
		if err := rows.Scan(&key, &toState, &reason, &occurred); err != nil {
			continue
		}
		parts := strings.Split(key, "/")
		repoStr := ""
		issueStr := ""
		if len(parts) >= 4 {
			repoStr = parts[0] + "/" + parts[1]
			issueStr = parts[3]
		}
		out = append(out, map[string]interface{}{
			"session_key": key, "repo": repoStr, "issue_number": issueStr,
			"to_state": toState, "reason": reason, "occurred_at": occurred,
		})
	}
	return out
}

// handleTrace serves a session's memory trace as HTML.
// Path: /trace/{owner}/{repo}/{issues|pulls}/{N}
func (r *Router) handleTrace(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse path: /trace/owner/repo/issues/N or /trace/owner/repo/pulls/N
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/trace/"), "/")
	if len(parts) < 4 {
		http.Error(w, "invalid trace path: expected /trace/owner/repo/issues/N", http.StatusBadRequest)
		return
	}
	owner, repo, kind, num := parts[0], parts[1], parts[2], parts[3]
	if kind != "issues" && kind != "pulls" {
		http.Error(w, "path must be /issues/N or /pulls/N", http.StatusBadRequest)
		return
	}

	// Build path to memory JSONL
	workDir := r.cfg.Agent.WorkDir
	if workDir == "" {
		http.Error(w, "WorkDir not configured", http.StatusServiceUnavailable)
		return
	}
	memPath := filepath.Join(workDir, owner, repo, kind, num, "memory.jsonl")

	data, err := os.ReadFile(memPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("session trace not found: %v", err), http.StatusNotFound)
		return
	}

	// Parse JSONL lines
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html><head>
<meta charset="utf-8">
<title>Fordjent Trace — %s/%s/%s/%s</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:960px;margin:2em auto;padding:0 1em;background:#0d1117;color:#c9d1d9}
h1{color:#58a6ff}h2{color:#f0f6fc;border-bottom:1px solid #30363d;padding-bottom:.3em}
.turn{border:1px solid #30363d;border-radius:6px;margin:1em 0;padding:1em;background:#161b22}
.turn-header{color:#8b949e;font-size:.85em;margin-bottom:.5em}
.tool{background:#1c2128;border-left:3px solid #58a6ff;padding:.5em 1em;margin:.5em 0;border-radius:0 4px 4px 0}
.tool-name{color:#7ee787;font-weight:600}
.tool-output{color:#c9d1d9;white-space:pre-wrap;font-size:.9em;max-height:300px;overflow-y:auto}
.response{color:#a5d6ff;white-space:pre-wrap;line-height:1.5}
.error{color:#f85149}pre{overflow-x:auto}
.mark{display:inline-block;padding:1px 6px;border-radius:4px;font-size:.8em;font-weight:600}
.mark-success{background:#238636;color:#fff}.mark-fail{background:#da3633;color:#fff}.mark-info{background:#1f6feb;color:#fff}
</style></head><body>
<h1>Fordjent Session Trace</h1>
<p><strong>Repository:</strong> %s/%s &middot; <strong>%s:</strong> #%s &middot; <strong>Turns:</strong> %d</p>
`,
		html.EscapeString(owner), html.EscapeString(repo), kind, html.EscapeString(num),
		html.EscapeString(owner), html.EscapeString(repo), kind, html.EscapeString(num), len(lines))

	allTools := make(map[string]int)
		for _, line := range lines {
		var entry struct {
			Timestamp  string `json:"timestamp"`
			Turn       int    `json:"turn"`
			EventType  string `json:"event_type"`
			ToolName   string `json:"tool_name"`
			ToolArgs   string `json:"tool_args"`
			ToolResult string `json:"tool_result"`
			Response   string `json:"response"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		fmt.Fprintf(w, `<div class="turn"><div class="turn-header">Turn %d &middot; %s &middot; %s</div>`,
			entry.Turn, entry.Timestamp, entry.EventType)

		if entry.Error != "" {
			fmt.Fprintf(w, `<div class="error"><strong>Error:</strong> %s</div>`, html.EscapeString(entry.Error))
		}

		if entry.ToolName != "" {
			allTools[entry.ToolName]++
			fmt.Fprintf(w, `<div class="tool"><span class="tool-name">%s</span>`, html.EscapeString(entry.ToolName))
			if entry.ToolArgs != "" && entry.ToolArgs != "{}" {
				fmt.Fprintf(w, `<pre>%s</pre>`, html.EscapeString(tryFormatJSON(entry.ToolArgs)))
			}
			if entry.ToolResult != "" {
				displayResult := entry.ToolResult
				if len(displayResult) > 2000 {
					displayResult = displayResult[:2000] + "\n... (truncated)"
				}
				fmt.Fprintf(w, `<div class="tool-output">%s</div>`, html.EscapeString(displayResult))
			}
			fmt.Fprint(w, `</div>`)
		}

		if entry.Response != "" {
			fmt.Fprintf(w, `<div class="response">%s</div>`, html.EscapeString(entry.Response))
		}
		fmt.Fprint(w, `</div>`)
	}

	// Summary
	fmt.Fprint(w, `<h2>Summary</h2><table>`)
	for name, count := range allTools {
		fmt.Fprintf(w, `<tr><td>%s calls</td><td>%d</td></tr>`, html.EscapeString(name), count)
	}
	fmt.Fprint(w, `</table><p style="color:#8b949e;margin-top:2em">Fordjent agent &middot; session trace</p></body></html>`)
}

func tryFormatJSON(raw string) string {
	// Pretty-print if valid JSON
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	formatted, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(formatted)
}
