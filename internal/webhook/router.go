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
	cfg          *config.Config
	bus          *event.Bus
	logger       *slog.Logger
	mux          *http.ServeMux
	server       *http.Server
	mu           sync.Mutex
	shuttingDown bool
	lc           *lifecycle.Lifecycle // optional: set post-construction for webhook delivery tracking
	forgejo      *forgejo.Client      // optional: set post-construction for PR state checks
	routeTable   *RouteTable          // optional: set post-construction for PR label-based routing
	seenEvents   sync.Map             // event_id -> time.Time for dedup (TTL 30s)
}

// RouteResult is computed by the routing table for each event.
// The routing table is the SOLE source of truth for determining which role
// handles which event. No component may create sessions independently;
// all Fordjent session creation SHALL flow through this routing table.
type RouteResult struct {
	Role       string // "pm", "implementer", "reviewer"
	SessionKey string // e.g. "fjadmin/testbed/pulls/42"
	IsFix      bool   // true for pulls/N-fix sessions
}

// RouteTable matches events to roles using the priority-ordered routing table
// defined in the spec-driven-lifecycle spec (Requirement: Event-to-Role Routing Table).
//
// Routing table sovereignty: No other component creates sessions independently.
// All Fordjent session creation SHALL flow through this routing table.
type RouteTable struct {
	forgejo *forgejo.Client
}

// NewRouteTable creates a routing table with the given Forgejo client for label lookups.
func NewRouteTable(forgejoClient *forgejo.Client) *RouteTable {
	return &RouteTable{forgejo: forgejoClient}
}

// Route evaluates the 10 priority rules from the spec and returns the result.
// It returns (result, matched=false) if no rule matches.
//
// Priority table (from spec-driven-lifecycle spec):
//
// | Priority | Event Type                         | Condition                                       | Session Key      | Role              |
// |----------|----------------------------------- |-------------------------------------------------|------------------|-------------------|
// | 1        | issue_comment.created              | PR labels: spec-proposed or spec-approved        | pulls/N          | pm                |
// | 2        | pull_request_review_comment.created| PR labels: spec-proposed or spec-approved        | pulls/N          | pm                |
// | 3        | pull_request_review_comment.created| PR labels: changes_requested or actionable body   | pulls/N-fix      | implementer       |
// | 4        | issue_comment.created              | Human sender, is PR, no spec labels              | pulls/N          | reviewer          |
// | 5        | pull_request_review_comment.created| PR labels none of above                          | pulls/N          | reviewer          |
// | 6        | pull_request.merged                | Any                                               | pulls/N          | scheduler         |
// | 9        | issue.closed                       | Issue is a task issue linked to change            | issues/N         | scheduler         |
// | 10       | (derived)                          | All task issues for change closed                 | issues/<parent>  | pm                |
func (rt *RouteTable) Route(ctx context.Context, evt *event.Event) (RouteResult, bool) {
	prLabels := rt.fetchPRLabels(ctx, evt)

	// Rule 8: ArchiveChangeRequested — all task issues closed
	if evt.Type == event.ArchiveChangeRequested {
		return RouteResult{
			Role:       "pm",
			SessionKey: evt.SessionKey,
		}, true
	}

	// Rule 1: issue_comment.created on spec PR → PM
	if evt.Type == event.IssueCommentCreated && evt.PRNumber > 0 {
		if hasLabel(prLabels, "spec-proposed") || hasLabel(prLabels, "spec-approved") {
			return RouteResult{
				Role:       "pm",
				SessionKey: fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber),
			}, true
		}
	}

	// Rule 2: pull_request_review_comment.created on spec PR → PM
	if evt.Type == event.PullRequestReviewComment && evt.PRNumber > 0 {
		if hasLabel(prLabels, "spec-proposed") || hasLabel(prLabels, "spec-approved") {
			return RouteResult{
				Role:       "pm",
				SessionKey: fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber),
			}, true
		}
	}

	// Rule 3: pull_request_review_comment with changes_requested → Implementer fix
	if evt.Type == event.PullRequestReviewComment && evt.PRNumber > 0 {
		if hasLabel(prLabels, "changes_requested") || rt.isActionableReview(evt) {
			return RouteResult{
				Role:       "implementer",
				SessionKey: fmt.Sprintf("%s/pulls/%d-fix", evt.Repository, evt.PRNumber),
				IsFix:      true,
			}, true
		}
	}

	// Rule 3b: PullRequestReview (formal review) on a spec-free PR.
	// Routes to an implementer fix session when the review verdict is
	// changes_requested OR the PR carries the `changes_requested` label
	// (label-based routing is version-stable: the forgejo_submit_review tool
	// sets the label, and a human can also set it from the UI even when the
	// review webhook isn't subscribed). Approved/dismissed reviews are
	// non-actionable. `commented` reviews are informational and do NOT spawn
	// a session (a bare comment is not change-requesting; actionable human
	// feedback arrives via issue_comment.created → Rule 4 instead).
	if evt.Type == event.PullRequestReview && evt.PRNumber > 0 {
		if hasLabel(prLabels, "spec-proposed") || hasLabel(prLabels, "spec-approved") {
			// Spec PRs route to PM, not implementer.
			return RouteResult{
				Role:       "pm",
				SessionKey: fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber),
			}, true
		}
		switch evt.ReviewState {
		case "dismissed":
			return RouteResult{}, false
		case "approved":
			// Approved reviews don't spawn a new session on their own — the
			// gated automerge watcher re-evaluates when it sees the event.
			return RouteResult{}, false
		case "changes_requested", "":
			// An explicit changes_requested verdict, OR a PR carrying the
			// changes_requested label (with a non-approved review state).
			if evt.ReviewState == "changes_requested" || hasLabel(prLabels, "changes_requested") {
				return RouteResult{
					Role:       "implementer",
					SessionKey: fmt.Sprintf("%s/pulls/%d-fix", evt.Repository, evt.PRNumber),
					IsFix:      true,
				}, true
			}
			// Empty ReviewState + no label: fall through (non-actionable).
		}
		// `commented` (and any other non-decisive state) falls through without
		// spawning a session — see comment above.
	}

	// Rule 3c: ReviewRequested (internal synthetic event from yolo-repo PR
	// open) → spawn reviewer (djent-qa) on pulls/N.
	if evt.Type == event.ReviewRequested && evt.PRNumber > 0 {
		return RouteResult{
			Role:       "reviewer",
			SessionKey: fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber),
		}, true
	}

	// Rule 7: failed check_run.completed on a dev PR → Implementer fix.
	// Pre-conditions: not on spec/ralph/merging PRs; PR is real.
	if evt.Type == event.CheckRunCompleted && evt.PRNumber > 0 {
		if !hasLabel(prLabels, "spec-proposed") &&
			!hasLabel(prLabels, "spec-approved") &&
			!hasLabel(prLabels, "ralph") &&
			!hasLabel(prLabels, "merging") {
			// Only failure-class conclusions rework; success re-evaluates automerge in the manager.
			if isFailedConclusion(evt.CheckConclusion) {
				return RouteResult{
					Role:       "implementer",
					SessionKey: fmt.Sprintf("%s/pulls/%d-fix", evt.Repository, evt.PRNumber),
					IsFix:      true,
				}, true
			}
		}
	}

	// Rule 8: failed workflow_run.completed on a dev PR → Implementer fix.
	if evt.Type == event.WorkflowRunCompleted && evt.PRNumber > 0 {
		if !hasLabel(prLabels, "spec-proposed") &&
			!hasLabel(prLabels, "spec-approved") &&
			!hasLabel(prLabels, "ralph") &&
			!hasLabel(prLabels, "merging") {
			if isFailedConclusion(evt.CheckConclusion) {
				return RouteResult{
					Role:       "implementer",
					SessionKey: fmt.Sprintf("%s/pulls/%d-fix", evt.Repository, evt.PRNumber),
					IsFix:      true,
				}, true
			}
		}
	}

	// Rule 4: issue_comment.created on normal PR (human, no spec) → Reviewer
	if evt.Type == event.IssueCommentCreated && evt.PRNumber > 0 {
		sender := evt.Sender
		isAgent := strings.Contains(sender, "fordjent") || strings.Contains(sender, "djent")
		if !isAgent {
			return RouteResult{
				Role:       "reviewer",
				SessionKey: fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber),
			}, true
		}
	}

	// Rule 5: pull_request_review_comment on normal PR → Reviewer
	if evt.Type == event.PullRequestReviewComment && evt.PRNumber > 0 {
		return RouteResult{
			Role:       "reviewer",
			SessionKey: fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber),
		}, true
	}

	// Rule 6: pull_request.merged → Scheduler
	if evt.Type == event.PullRequestMerged && evt.PRNumber > 0 {
		return RouteResult{
			Role:       "scheduler",
			SessionKey: fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber),
		}, true
	}

	// Rule 7: issue.closed on task issue → Scheduler
	if evt.Type == event.IssueClosed && evt.IssueNumber > 0 {
		return RouteResult{
			Role:       "scheduler",
			SessionKey: fmt.Sprintf("%s/issues/%d", evt.Repository, evt.IssueNumber),
		}, true
	}

	return RouteResult{}, false
}

// fetchPRLabels returns the PR labels for the event's PR number.
// Returns nil if the event has no PR number or if the Forgejo client is unavailable.
func (rt *RouteTable) fetchPRLabels(ctx context.Context, evt *event.Event) []string {
	if rt.forgejo == nil || evt.PRNumber <= 0 {
		return nil
	}
	issue, err := rt.forgejo.GetIssue(ctx, evt.Repository, evt.PRNumber)
	if err != nil || issue == nil {
		return nil
	}
	var labels []string
	for _, l := range issue.Labels {
		labels = append(labels, l.Name)
	}
	return labels
}

// hasLabel checks if a label name exists in the label list.
func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if l == name {
			return true
		}
	}
	return false
}

// isFailedConclusion returns true for check_run/workflow_run conclusions that
// should rework the implementation. "success" / "neutral" / "skipped" do not.
// "pending" / "in_progress" / "" are non-conclusive and do not rework either
// (the manager's gated automerge re-evaluates them and decides to wait).
// "cancelled" is intentionally included per the add-pr-rework-loop spec: a
// cancelled run on a dev PR usually indicates a broken pipeline (compile
// error aborting the job) that the dev session should address. If this proves
// too aggressive in practice, drop "cancelled" from the set.
func isFailedConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "cancelled", "action_required", "timed_out":
		return true
	}
	return false
}

// isActionableReview checks if the review comment body contains an actionable directive.
func (rt *RouteTable) isActionableReview(evt *event.Event) bool {
	body := ""
	if comment, ok := evt.Payload["comment"].(map[string]interface{}); ok {
		if b, ok := comment["body"].(string); ok {
			body = strings.ToLower(b)
		}
	}
	if body == "" {
		return false
	}
	actionableWords := []string{"fix", "rename", "add", "remove", "update", "change", "refactor", "delete"}
	for _, w := range actionableWords {
		if strings.Contains(body, w) {
			return true
		}
	}
	return false
}

// ApplyRoute sets the Role, SessionKey, and IsFix fields on the event based on
// the routing table result. Returns true if a route matched.
func ApplyRoute(ctx context.Context, rt *RouteTable, evt *event.Event) bool {
	if rt == nil {
		return false
	}
	result, matched := rt.Route(ctx, evt)
	if !matched {
		return false
	}
	evt.Role = result.Role
	if result.SessionKey != "" {
		evt.SessionKey = result.SessionKey
	}
	return true
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
	// /status is registered below (with or without auth)
	r.mux.HandleFunc("/tokens-per-minute", r.handleTokensPerMinute)
	r.mux.HandleFunc("/activity", r.handleActivity)
	r.mux.HandleFunc("/trace/", r.handleTrace) // /trace/{owner}/{repo}/{issues|pulls}/{N}
	r.mux.HandleFunc("/acp/v1/stream", r.handleStream)
	r.mux.HandleFunc("/dashboard", r.handleDashboard)

	// Admin endpoint: require auth if admin_token is set
	if cfg.Security.AdminToken != "" {
		r.mux.Handle("/admin", requireAuth(cfg.Security.AdminToken, webui.Handler(cfg)))
		r.mux.Handle("/admin/", requireAuth(cfg.Security.AdminToken, webui.Handler(cfg)))
		r.mux.HandleFunc("/status", requireAuthFunc(cfg.Security.AdminToken, r.handleStatus))
	} else {
		r.logger.Warn("admin_token not set — admin/status endpoints disabled for security")
		r.mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "admin endpoint disabled: set admin_token in config", http.StatusForbidden)
		})
		r.mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "admin endpoint disabled: set admin_token in config", http.StatusForbidden)
		})
		// /status remains public for health checks; /metrics is also public
		r.mux.HandleFunc("/status", r.handleStatus)
	}

	// Periodic cleanup of seen event IDs (TTL ~30s)
	go func() {
		for {
			time.Sleep(30 * time.Second)
			now := time.Now()
			r.seenEvents.Range(func(key, value interface{}) bool {
				if t, ok := value.(time.Time); ok && now.Sub(t) > 30*time.Second {
					r.seenEvents.Delete(key)
				}
				return true
			})
		}
	}()

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
	// Also create the route table now that we have a Forgejo client for label lookups
	r.routeTable = NewRouteTable(client)
}

// SetRouteTable wires an explicit route table for event-to-role routing.
func (r *Router) SetRouteTable(rt *RouteTable) {
	r.routeTable = rt
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

	// Cache statistics
	var totalCachedTokens int64
	var totalCacheSavings, totalRequestCost, totalEnergy float64
	_ = db.QueryRow("SELECT COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_savings_usd),0), COALESCE(SUM(request_cost_usd),0), COALESCE(SUM(energy_joules),0) FROM usage").Scan(&totalCachedTokens, &totalCacheSavings, &totalRequestCost, &totalEnergy)
	result["total_cached_tokens"] = totalCachedTokens
	result["total_cache_savings_usd"] = totalCacheSavings
	result["total_request_cost_usd"] = totalRequestCost
	result["total_energy_joules"] = totalEnergy
	if totalTokens > 0 {
		result["cache_hit_rate"] = float64(totalCachedTokens) / float64(totalTokens)
	}

	recent := []map[string]interface{}{}
	rows, err := db.Query("SELECT session_key, provider, model, input_tokens, output_tokens, cost_usd, cached_tokens, cache_savings_usd, request_cost_usd, energy_joules, created_at FROM usage ORDER BY created_at DESC LIMIT 20")
	if err == nil && rows != nil {
		defer rows.Close()
		for rows.Next() {
			var s, p, m string
			var it, ot int
			var cost, cacheSav, reqCost, energy float64
			var cached int
			var ts string
			_ = rows.Scan(&s, &p, &m, &it, &ot, &cost, &cached, &cacheSav, &reqCost, &energy, &ts)
			recent = append(recent, map[string]interface{}{
				"session_key": s, "provider": p, "model": m,
				"input_tokens": it, "output_tokens": ot, "cost_usd": cost,
				"cached_tokens": cached, "cache_savings_usd": cacheSav,
				"request_cost_usd": reqCost, "energy_joules": energy,
				"timestamp": ts,
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
			var sk string
			var turn, tc, lat, tin, tout int
			var errMsg, ts string
			_ = tRows.Scan(&sk, &turn, &tc, &lat, &tin, &tout, &errMsg, &ts)
			entry := map[string]interface{}{"session_key": sk, "turn": turn, "tool_calls": tc, "latency_ms": lat, "tokens_in": tin, "tokens_out": tout, "timestamp": ts}
			if errMsg != "" {
				entry["error"] = errMsg
			}
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
	// Dedup: skip if we've already processed this delivery ID recently
	// Use the Forgejo delivery header for early dedup before event normalization.
	deliveryID := req.Header.Get("X-Forgejo-Delivery")
	if deliveryID == "" {
		deliveryID = req.Header.Get("X-Gitea-Delivery")
	}
	if deliveryID != "" {
		if _, seen := r.seenEvents.Load(deliveryID); seen {
			r.logger.Info("duplicate webhook delivery, skipping", "delivery_id", deliveryID)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"delivery_id": "%s", "status": "duplicate"}`, deliveryID)
			return
		}
		r.seenEvents.Store(deliveryID, time.Now())
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

	// Apply routing table to determine role and session key.
	// The routing table is the SOLE source of truth for event-to-role dispatch.
	// All session creation SHALL flow through this routing table.
	if r.routeTable != nil {
		if matched := ApplyRoute(req.Context(), r.routeTable, evt); matched {
			r.logger.Info("routing table matched",
				"event_id", evt.ID,
				"role", evt.Role,
				"session_key", evt.SessionKey,
			)
		}
	}

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
	case "pull_request_review":
		// Forgejo review webhook action is typically "submitted"; map to the
		// canonical ".created" constant so downstream routing treats it
		// uniformly (the review state is carried in ReviewState, not action).
		typ = event.PullRequestReview
	case "push":
		typ = event.Push
	case "check_run":
		// Forgejo fires action values "completed", "rerequested", "created".
		// We only care about "completed" — other actions are noise.
		if action == "completed" {
			typ = event.CheckRunCompleted
		} else {
			return nil, fmt.Errorf("check_run action %q ignored", action)
		}
	case "workflow_run":
		if action == "completed" {
			typ = event.WorkflowRunCompleted
		} else {
			return nil, fmt.Errorf("workflow_run action %q ignored", action)
		}
	default:
		return nil, fmt.Errorf("unsupported event type: %s", eventType)
	}

	evt := event.NewEvent(typ, repo, issueNum, prNum, sender, action)
	evt.Payload = payload

	// Populate CI / review carrier fields for the new event types.
	switch typ {
	case event.CheckRunCompleted:
		r.populateCheckRunFields(evt, payload)
	case event.WorkflowRunCompleted:
		r.populateWorkflowRunFields(evt, payload)
	case event.PullRequestReview:
		r.populateReviewFields(evt, payload)
	}

	// Compute session key: repository/issues/number or repository/pulls/number
	// Uses evt.PRNumber so that the populate* helpers (which may resolve the
	// PR after the initial extractPRNum() call couldn't find it, e.g. for
	// check_run payloads without an `issue` field) are reflected here.
	if evt.PRNumber > 0 {
		evt.SessionKey = fmt.Sprintf("%s/pulls/%d", repo, evt.PRNumber)
	} else if issueNum > 0 {
		evt.SessionKey = fmt.Sprintf("%s/issues/%d", repo, issueNum)
	} else {
		evt.SessionKey = fmt.Sprintf("%s/push/%d", repo, time.Now().UnixNano())
	}

	return evt, nil
}

// populateCheckRunFields extracts CheckName, CheckConclusion, CheckURL,
// and HeadSHA from a Forgejo `check_run` payload. If the payload does not
// carry a PR number (`check_run.pull_requests` is empty/missing — common on
// Forgejo/Gitea), the router resolves the PR from the head SHA via the API.
// If no PR is resolved, the event's PRNumber stays 0 and will be dropped
// by routing (the check was on a direct-to-main commit, not a PR).
func (r *Router) populateCheckRunFields(evt *event.Event, payload map[string]interface{}) {
	if cr, ok := payload["check_run"].(map[string]interface{}); ok {
		if name, ok := cr["name"].(string); ok {
			evt.CheckName = name
		}
		if conc, ok := cr["conclusion"].(string); ok {
			evt.CheckConclusion = conc
		}
		if url, ok := cr["html_url"].(string); ok {
			evt.CheckURL = url
		} else if url, ok := cr["url"].(string); ok {
			evt.CheckURL = url
		}
		if hs, ok := cr["head_sha"].(string); ok {
			evt.HeadSHA = hs
		}
		// Prefer PR number from `check_run.pull_requests[0].number`.
		if prs, ok := cr["pull_requests"].([]interface{}); ok && len(prs) > 0 {
			if pr, ok := prs[0].(map[string]interface{}); ok {
				if num, ok := pr["number"].(float64); ok {
					evt.PRNumber = int(num)
				}
			}
		}
	}
	// Head SHA fallback: Forgejo may put it on the top-level check_suite wrapper.
	if evt.HeadSHA == "" {
		if cs, ok := payload["check_suite"].(map[string]interface{}); ok {
			if hs, ok := cs["head_sha"].(string); ok {
				evt.HeadSHA = hs
			}
		}
	}
	// Reconcile PR number when payload didn't carry it.
	if evt.PRNumber == 0 && evt.HeadSHA != "" {
		r.resolvePRBySHA(evt)
	}
}

// populateWorkflowRunFields extracts the workflow run's conclusion + head SHA
// from a Forgejo `workflow_run` payload. The PR is always resolved post-hoc
// via the head SHA, since this webhook type does not carry `pull_requests`.
func (r *Router) populateWorkflowRunFields(evt *event.Event, payload map[string]interface{}) {
	if wr, ok := payload["workflow_run"].(map[string]interface{}); ok {
		if name, ok := wr["name"].(string); ok {
			evt.CheckName = name
		}
		if conc, ok := wr["conclusion"].(string); ok {
			evt.CheckConclusion = conc
		}
		if url, ok := wr["html_url"].(string); ok {
			evt.CheckURL = url
		}
		if hs, ok := wr["head_sha"].(string); ok {
			evt.HeadSHA = hs
		}
	} else {
		// Some Forgejo versions nest the run differently; try top-level fields.
		if name, ok := payload["name"].(string); ok {
			evt.CheckName = name
		}
		if conc, ok := payload["conclusion"].(string); ok {
			evt.CheckConclusion = conc
		}
		if hs, ok := payload["head_sha"].(string); ok {
			evt.HeadSHA = hs
		}
	}
	if evt.PRNumber == 0 && evt.HeadSHA != "" {
		r.resolvePRBySHA(evt)
	}
}

// populateReviewFields extracts the review state from a Forgejo
// `pull_request_review` payload and stamps ReviewState on the event.
// Forgejo/Gitea report review state as lowercase strings
// ("approved" / "changes_requested" / "commented" / "dismissed").
// Dismissed reviews are non-actionable and dropped at routing time.
func (r *Router) populateReviewFields(evt *event.Event, payload map[string]interface{}) {
	state := ""
	if rev, ok := payload["review"].(map[string]interface{}); ok {
		if s, ok := rev["state"].(string); ok {
			state = strings.ToLower(s)
		}
		if body, ok := rev["body"].(string); ok {
			// Stash on the payload's `comment` slot so existing comment-based
			// routing sees the body when it scans Payload for content.
			payload["comment"] = map[string]interface{}{"body": body}
		}
	} else if s, ok := payload["state"].(string); ok {
		state = strings.ToLower(s)
	}
	evt.ReviewState = state
	evt.Action = state // surfaces the review verdict as the event's action verb
}

// resolvePRBySHA attempts to find an open PR whose head SHA matches the given
// SHA and sets evt.PRNumber accordingly. Uses the Forgejo commit-statuses or
// PR-by-head-SHA API. Called as a fallback when a CI-event payload is missing
// `pull_requests`. Silent on failure — events with no resolved PR are dropped
// by the routing table (the check was on a ref that isn't an open PR).
func (r *Router) resolvePRBySHA(evt *event.Event) {
	if r.forgejo == nil || evt.HeadSHA == "" || evt.Repository == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prs, err := r.forgejo.ListPRs(ctx, evt.Repository, "open")
	if err != nil || len(prs) == 0 {
		slog.Debug("check-run: no open PRs to resolve from head SHA",
			"repo", evt.Repository, "head_sha", evt.HeadSHA, "error", err)
		return
	}
	for _, pr := range prs {
		if pr.Head.SHA == evt.HeadSHA {
			evt.PRNumber = pr.Number
			return
		}
	}
	slog.Debug("check-run: head SHA did not match any open PR",
		"repo", evt.Repository, "head_sha", evt.HeadSHA, "open_prs", len(prs))
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
	stuckSessions := queryStuckSessions(lifecycleDBPath)
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

	// Stuck sessions — active but no activity for >15 minutes
	if len(stuckSessions) > 0 {
		fmt.Fprint(w, `<div class="section"><h2>⚠️ Stuck Sessions</h2><table><tr><th>Session</th><th>Repo</th><th>Issue</th><th>Last Activity</th><th>Idle (min)</th></tr>`)
		for _, s := range stuckSessions {
			sessionKey := fmt.Sprint(s["session_key"])
			traceURL := fmt.Sprintf("/trace/%s", sessionKey)
			idleMin := "?"
			if im, ok := s["idle_minutes"]; ok {
				idleMin = fmt.Sprintf("%.0f", im)
			}
			fmt.Fprintf(w, `<tr><td><a href="%s">%s</a></td><td>%s</td><td>%s</td><td>%s</td><td><span class="tag tag-amber">%s min</span></td></tr>`,
				html.EscapeString(traceURL), html.EscapeString(sessionKey),
				html.EscapeString(fmt.Sprint(s["repo"])), html.EscapeString(fmt.Sprint(s["issue_number"])),
				html.EscapeString(fmt.Sprint(s["last_active"])), html.EscapeString(idleMin))
		}
		fmt.Fprint(w, `</table></div>`)
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

// queryStuckSessions returns sessions that have been stuck in 'working' state
// for more than 15 minutes without any state transition.
func queryStuckSessions(dbPath string) []map[string]interface{} {
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT session_key, MAX(occurred_at) as last_active
		FROM session_transitions
		WHERE to_state = 'working'
		GROUP BY session_key
		HAVING last_active < datetime('now', '-15 minutes')
		ORDER BY last_active ASC
		LIMIT 10
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []map[string]interface{}
	now := time.Now().UTC()
	for rows.Next() {
		var key, lastActive string
		if err := rows.Scan(&key, &lastActive); err != nil {
			continue
		}
		// Parse repo/issue from session key (format: owner/repo/issues/N or owner/repo/pulls/N)
		parts := strings.Split(key, "/")
		repoStr := ""
		issueStr := ""
		if len(parts) >= 4 {
			repoStr = parts[0] + "/" + parts[1]
			issueStr = parts[3]
		}
		// Calculate idle minutes
		idleMin := 0.0
		if t, err := time.Parse("2006-01-02 15:04:05", lastActive); err == nil {
			idleMin = now.Sub(t).Minutes()
		}
		out = append(out, map[string]interface{}{
			"session_key": key, "repo": repoStr, "issue_number": issueStr,
			"last_active": lastActive, "idle_minutes": idleMin,
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

// requireAuth wraps an http.Handler with bearer-token or basic-auth authentication.
func requireAuth(token string, handler http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBearerToken(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}
}

// requireAuthFunc wraps an http.HandlerFunc with bearer-token or basic-auth authentication.
func requireAuthFunc(token string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBearerToken(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}
}

// checkBearerToken validates the Authorization header (Bearer token or Basic auth).
func checkBearerToken(r *http.Request, expected string) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ") == expected
	}
	if user, pass, ok := r.BasicAuth(); ok {
		return user == "admin" && pass == expected
	}
	return false
}
