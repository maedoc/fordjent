package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/metrics"
	_ "modernc.org/sqlite"
)

// Router receives Forgejo webhooks, validates them, normalizes events,
// and publishes to the event bus.
type Router struct {
	cfg    *config.Config
	bus    *event.Bus
	logger *slog.Logger
	mux    *http.ServeMux
	server *http.Server
}

func NewRouter(cfg *config.Config, bus *event.Bus, logger *slog.Logger) *Router {
	r := &Router{
		cfg:    cfg,
		bus:    bus,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	r.mux.HandleFunc("/acp/v1/events", r.handleWebhook)
	r.mux.HandleFunc("/healthz", r.handleHealth)
	r.mux.HandleFunc("/readyz", r.handleReadyz)
	r.mux.HandleFunc("/metrics", metrics.Handler())
	r.mux.HandleFunc("/status", r.handleStatus)

	return r
}

func (r *Router) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (r *Router) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ready")
}

func (r *Router) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]interface{}{"now": time.Now().UTC().Format(time.RFC3339)}

	if r.cfg.Agent.WorkDir != "" {
		// Cost summary
		costDB := filepath.Join(r.cfg.Agent.WorkDir, "costs.db")
		if data, err := queryCostDB(costDB); err == nil {
			resp["costs"] = data
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

func queryCostDB(dbPath string) (map[string]interface{}, error) {
	db, err := sql.Open("sqlite", dbPath)
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
	rows, err := db.Query("SELECT session_key, provider, model, input_tokens, output_tokens, cost_usd, timestamp FROM usage ORDER BY timestamp DESC LIMIT 20")
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

func queryLifecycleDB(dbPath string) (map[string]interface{}, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	result := map[string]interface{}{}
	var active, failed int
	_ = db.QueryRow("SELECT COUNT(*) FROM session_transitions WHERE to_state = 'working'").Scan(&active)
	_ = db.QueryRow("SELECT COUNT(*) FROM session_transitions WHERE to_state LIKE 'failed%'").Scan(&failed)
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
	return result, nil
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

	// Normalize to internal event
	evt, err := r.normalizeEvent(eventType, action, payload)
	if err != nil {
		r.logger.Warn("unhandled event type", "type", eventType, "action", action, "error", err)
		w.WriteHeader(http.StatusOK) // Ack but ignore
		fmt.Fprintln(w, "ignored")
		return
	}

	metrics.IncEvents()
	if r.cfg.Security.FilterAgentEvents && r.isAgentEvent(payload) {
		r.logger.Info("filtered agent-originated event", "event_id", evt.ID)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "filtered")
		return
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
func (r *Router) isAgentEvent(payload map[string]interface{}) bool {
	marker := "<!-- ford -->"

	// Check commits in push events
	if commits, ok := payload["commits"].([]interface{}); ok {
		for _, c := range commits {
			if commit, ok := c.(map[string]interface{}); ok {
				if msg, ok := commit["message"].(string); ok {
					if strings.HasPrefix(msg, r.cfg.Agent.CommitPrefix) {
						return true
					}
				}
			}
		}
	}

	// Check sender (agent bot user)
	if sender, ok := payload["sender"].(map[string]interface{}); ok {
		if login, ok := sender["login"].(string); ok {
			if login == "fordjent[bot]" || login == "fordjent-bot" {
				return true
			}
		}
	}

	// Check comment body for the hidden agent marker (prevent self-comment loops)
	if comment, ok := payload["comment"].(map[string]interface{}); ok {
		if body, ok := comment["body"].(string); ok {
			if strings.Contains(body, marker) {
				return true
			}
		}
	}

	// Check issue body for the hidden agent marker
	if issue, ok := payload["issue"].(map[string]interface{}); ok {
		if body, ok := issue["body"].(string); ok {
			if strings.Contains(body, marker) {
				return true
			}
		}
	}

	// Check pull_request body for the hidden agent marker
	if pr, ok := payload["pull_request"].(map[string]interface{}); ok {
		if body, ok := pr["body"].(string); ok {
			if strings.Contains(body, marker) {
				return true
			}
		}
	}

	return false
}
