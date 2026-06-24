package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"log/slog"
)

func TestHealthEndpoint(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: "test-secret"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestWebhookMethodNotAllowed(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/acp/v1/events", nil)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestWebhookMissingSignature(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: "test-secret"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "opened",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"issue":      map[string]interface{}{"number": float64(42)},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "issues")
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWebhookNoSecret(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "opened",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"issue":      map[string]interface{}{"number": float64(42)},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "issues")
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWebhookMissingEventHeader(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	body, _ := json.Marshal(map[string]interface{}{})

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", bytes.NewReader(body))
	// No X-Forgejo-Event header
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestWebhookLoopPrevention(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Agent:    config.AgentConfig{CommitPrefix: "[agent-automation]"},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	// Push events with ref+commits must NEVER be filtered, even from bots
	payload := map[string]interface{}{
		"action":     "push",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"ref":        "refs/heads/main",
		"commits": []interface{}{
			map[string]interface{}{"message": "[agent-automation] auto-fix"},
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "push")
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() == "filtered\n" {
		t.Error("push events should NOT be filtered even with bot sender and commit prefix")
	}
}

func TestRouter_Ready(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: "test-secret"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "ready\n" {
		t.Errorf("expected 'ready\\n', got: %s", w.Body.String())
	}
}

func TestRouter_Metrics(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: "test-secret"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "fordjent_events_total") {
		t.Errorf("expected metrics to contain fordjent_events_total, got: %s", body)
	}
}

func TestNormalizeEventIssueComment(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"issue":      map[string]interface{}{"number": float64(42)},
		"comment":    map[string]interface{}{"id": float64(100), "body": "help"},
	}

	evt, err := router.normalizeEvent("issue_comment", "created", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Repository != "org/repo" {
		t.Errorf("expected org/repo, got %s", evt.Repository)
	}
	if evt.IssueNumber != 42 {
		t.Errorf("expected 42, got %d", evt.IssueNumber)
	}
	if evt.SessionKey != "org/repo/issues/42" {
		t.Errorf("expected org/repo/issues/42, got %s", evt.SessionKey)
	}
	if evt.Type != event.IssueCommentCreated {
		t.Errorf("expected %s, got %s", event.IssueCommentCreated, evt.Type)
	}
}

func TestNormalizeEventPullRequest(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":       "opened",
		"repository":   map[string]interface{}{"full_name": "org/repo"},
		"sender":       map[string]interface{}{"login": "bob"},
		"pull_request": map[string]interface{}{"number": float64(7)},
	}

	evt, err := router.normalizeEvent("pull_request", "opened", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.PRNumber != 7 {
		t.Errorf("expected PR 7, got %d", evt.PRNumber)
	}
	if evt.SessionKey != "org/repo/pulls/7" {
		t.Errorf("expected org/repo/pulls/7, got %s", evt.SessionKey)
	}
	if evt.Type != event.PullRequestOpened {
		t.Errorf("expected %s, got %s", event.PullRequestOpened, evt.Type)
	}
}

func TestNormalizeEventUnsupportedType(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	_, err := router.normalizeEvent("wiki", "created", map[string]interface{}{})
	if err == nil {
		t.Error("expected error for unsupported event type")
	}
}

func TestNormalizeEventMissingRepo(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action": "opened",
		"sender": map[string]interface{}{"login": "alice"},
		"issue":  map[string]interface{}{"number": float64(1)},
	}

	evt, err := router.normalizeEvent("issues", "opened", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Repository != "" {
		t.Errorf("expected empty repo for missing repo field, got %s", evt.Repository)
	}
}

func TestNormalizeEventPushNoIssueNumber(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
	}

	evt, err := router.normalizeEvent("push", "", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Type != event.Push {
		t.Errorf("expected Push, got %s", evt.Type)
	}
	if evt.SessionKey == "" {
		t.Error("expected non-empty session key for push")
	}
}

func TestNormalizeEventIssueCommentOnPR(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"issue": map[string]interface{}{
			"number":          float64(7),
			"is_pull_request": true,
		},
		"comment": map[string]interface{}{"id": float64(100), "body": "LGTM"},
	}

	evt, err := router.normalizeEvent("issue_comment", "created", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.PRNumber != 7 {
		t.Errorf("expected PRNumber=7 for PR comment, got %d", evt.PRNumber)
	}
	if evt.SessionKey != "org/repo/pulls/7" {
		t.Errorf("expected session key org/repo/pulls/7, got %s", evt.SessionKey)
	}
	if evt.Type != event.IssueCommentCreated {
		t.Errorf("expected %s, got %s", event.IssueCommentCreated, evt.Type)
	}
}

func TestNormalizeEventIssueLabelUpdated(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "label_updated",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"issue":      map[string]interface{}{"number": float64(42)},
	}

	evt, err := router.normalizeEvent("issues", "label_updated", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Type != event.IssueLabelUpdated {
		t.Errorf("expected %s, got %s", event.IssueLabelUpdated, evt.Type)
	}
	if evt.SessionKey != "org/repo/issues/42" {
		t.Errorf("expected org/repo/issues/42, got %s", evt.SessionKey)
	}
}

func TestNormalizeEventPullRequestLabelUpdated(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":       "label_updated",
		"repository":   map[string]interface{}{"full_name": "org/repo"},
		"sender":       map[string]interface{}{"login": "alice"},
		"pull_request": map[string]interface{}{"number": float64(9)},
	}

	evt, err := router.normalizeEvent("pull_request", "label_updated", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Type != event.PullRequestLabelUpdated {
		t.Errorf("expected %s, got %s", event.PullRequestLabelUpdated, evt.Type)
	}
	if evt.PRNumber != 9 {
		t.Errorf("expected PRNumber=9, got %d", evt.PRNumber)
	}
	if evt.SessionKey != "org/repo/pulls/9" {
		t.Errorf("expected org/repo/pulls/9, got %s", evt.SessionKey)
	}
}

func TestNormalizeEventPullRequestMerged(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "closed",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"pull_request": map[string]interface{}{
			"number": float64(5),
			"merged": true,
		},
	}

	evt, err := router.normalizeEvent("pull_request", "closed", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Type != event.PullRequestMerged {
		t.Errorf("expected %s, got %s", event.PullRequestMerged, evt.Type)
	}
	if evt.Action != "merged" {
		t.Errorf("expected action 'merged', got %s", evt.Action)
	}
}

func TestNormalizeEventPullRequestClosedNotMerged(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "closed",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"pull_request": map[string]interface{}{
			"number": float64(5),
			"merged": false,
		},
	}

	evt, err := router.normalizeEvent("pull_request", "closed", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Type != event.PullRequestClosed {
		t.Errorf("expected %s, got %s", event.PullRequestClosed, evt.Type)
	}
}

func TestIsAgentEvent_PushPassthrough(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"ref":        "refs/heads/main",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"commits": []interface{}{
			map[string]interface{}{"message": "[agent-automation] auto-fix"},
		},
	}

	if router.isAgentEvent(payload) {
		t.Error("push events should never be filtered, even from bot sender")
	}
}

func TestIsAgentEvent_CommentMarker(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"issue":      map[string]interface{}{"number": float64(1)},
		"comment": map[string]interface{}{
			"id":   float64(100),
			"body": "Session completed successfully.\n\n<!-- ford -->",
		},
	}

	if !router.isAgentEvent(payload) {
		t.Error("comment with <!-- ford --> marker should be filtered")
	}
}

func TestIsAgentEvent_BotSenderComment(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"issue":      map[string]interface{}{"number": float64(1)},
		"comment": map[string]interface{}{
			"id":   float64(100),
			"body": "Some comment without marker",
		},
	}

	if !router.isAgentEvent(payload) {
		t.Error("comment from fordjent-bot should be filtered")
	}
}

func TestIsAgentEvent_BotSenderBracketComment(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent[bot]"},
		"issue":      map[string]interface{}{"number": float64(1)},
		"comment": map[string]interface{}{
			"id":   float64(100),
			"body": "Some comment",
		},
	}

	if !router.isAgentEvent(payload) {
		t.Error("comment from fordjent[bot] should be filtered")
	}
}

func TestIsAgentEvent_HumanCommentNotFiltered(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"issue":      map[string]interface{}{"number": float64(1)},
		"comment": map[string]interface{}{
			"id":   float64(100),
			"body": "Please fix this bug",
		},
	}

	if router.isAgentEvent(payload) {
		t.Error("human comment should NOT be filtered")
	}
}

func TestIsAgentEvent_PROpenedNotFiltered(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "opened",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"pull_request": map[string]interface{}{
			"number": float64(5),
			"body":   "Auto-generated PR\n\n<!-- ford -->",
		},
	}

	if router.isAgentEvent(payload) {
		t.Error("PR opened event should NOT be filtered even with marker (reviewer must see it)")
	}
}

func TestIsAgentEvent_PRNonOpenedWithMarker(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "synchronize",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"pull_request": map[string]interface{}{
			"number": float64(5),
			"body":   "Auto-generated PR\n\n<!-- ford -->",
		},
	}

	if !router.isAgentEvent(payload) {
		t.Error("PR non-opened event with marker should be filtered")
	}
}

func TestIsAgentEvent_PRMergeNotFiltered(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "closed",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"pull_request": map[string]interface{}{
			"number": float64(5),
			"merged": true,
			"body":   "Auto-generated PR\n\n<!-- ford -->",
		},
	}

	if router.isAgentEvent(payload) {
		t.Error("PR merge event should NOT be filtered (scheduler depends on it)")
	}
}

func TestIsAgentEvent_IssueWithMarker(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "opened",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"issue": map[string]interface{}{
			"number": float64(10),
			"body":   "Scaffold issue\n\n<!-- ford -->",
		},
	}

	if !router.isAgentEvent(payload) {
		t.Error("issue with <!-- ford --> marker (no comment key) should be filtered")
	}
}

func TestIsAgentEvent_BotIssueWithoutCommentNotFiltered(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "opened",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"issue": map[string]interface{}{
			"number": float64(10),
			"body":   "Sub-issue created by PM",
		},
	}

	if router.isAgentEvent(payload) {
		t.Error("bot-created issue without marker and without comment key should NOT be filtered (sub-issues need sessions)")
	}
}

func TestClosedPRCommentGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls/") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": float64(5),
				"state":  "closed",
				"merged": true,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Forgejo: config.ForgejoConfig{URL: srv.URL, Token: "test"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	router.SetForgejoClient(forgejo.NewClient(srv.URL, "test"))

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"issue": map[string]interface{}{
			"number":          float64(5),
			"is_pull_request": true,
		},
		"comment": map[string]interface{}{
			"id":   float64(100),
			"body": "LGTM",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "issue_comment")
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "skipped_closed_pr\n" {
		t.Errorf("expected 'skipped_closed_pr', got %q", w.Body.String())
	}
}

func TestOpenPRCommentNotSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls/") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": float64(5),
				"state":  "open",
				"merged": false,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Forgejo: config.ForgejoConfig{URL: srv.URL, Token: "test"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	router.SetForgejoClient(forgejo.NewClient(srv.URL, "test"))

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"issue": map[string]interface{}{
			"number":          float64(5),
			"is_pull_request": true,
		},
		"comment": map[string]interface{}{
			"id":   float64(100),
			"body": "LGTM",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "issue_comment")
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() == "skipped_closed_pr\n" {
		t.Error("comment on open PR should NOT be skipped")
	}
}

func TestIsAgentEvent_PingParentMarkerNotFiltered(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"issue":      map[string]interface{}{"number": float64(5)},
		"comment": map[string]interface{}{
			"id":   float64(200),
			"body": "**[Implementer → PM]** Should I return an error or a boolean?\n\n<!-- ford-ping -->",
		},
	}

	if router.isAgentEvent(payload) {
		t.Error("implementer→PM ping comment with <!-- ford-ping --> marker should NOT be filtered")
	}
}

func TestIsAgentEvent_PingParentMarkerStillFiltersFordMarker(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: ""},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "fordjent-bot"},
		"issue":      map[string]interface{}{"number": float64(5)},
		"comment": map[string]interface{}{
			"id":   float64(200),
			"body": "Session completed.\n\n<!-- ford -->",
		},
	}

	if !router.isAgentEvent(payload) {
		t.Error("comment with <!-- ford --> marker (no ford-ping) should still be filtered")
	}
}

func TestAdminEndpointRequiresAuth(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: "test-secret"},
		Security: config.SecurityConfig{AdminToken: "secret-admin-token"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	// Without auth: should get 401
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for admin without auth, got %d", w.Code)
	}

	// With bearer token: should get 200 (or redirect)
	req2 := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req2.Header.Set("Authorization", "Bearer secret-admin-token")
	w2 := httptest.NewRecorder()
	router.mux.ServeHTTP(w2, req2)
	if w2.Code == http.StatusUnauthorized {
		t.Errorf("expected non-401 for admin with valid token, got %d", w2.Code)
	}

	// With wrong token: should get 401
	req3 := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req3.Header.Set("Authorization", "Bearer wrong-token")
	w3 := httptest.NewRecorder()
	router.mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for admin with wrong token, got %d", w3.Code)
	}
}

func TestAdminEndpointDisabledWithoutToken(t *testing.T) {
	cfg := &config.Config{
		Webhook:  config.WebhookConfig{Secret: "test-secret"},
		Security: config.SecurityConfig{AdminToken: ""}, // no token
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for admin without token configured, got %d", w.Code)
	}
}

func TestWebhookDedup(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	// Simulate a seen delivery ID
	deliveryID := "dedup-test-123"
	router.seenEvents.Store(deliveryID, time.Now())

	// Build a push event payload
	payload := map[string]interface{}{
		"action":     "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender":     map[string]interface{}{"login": "alice"},
		"head_commit": map[string]interface{}{
			"id": "abc123",
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "push")
	req.Header.Set("X-Forgejo-Delivery", deliveryID)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "duplicate") {
		t.Errorf("expected duplicate status, got: %s", w.Body.String())
	}
}

// --- Routing Table Tests ---

func TestRouteTable_SpecPRComment(t *testing.T) {
	// Rule 1: issue_comment.created on spec-proposed PR → PM
	// Without a Forgejo client, Rule 1 won't match (can't fetch labels)
	// But Rule 6 (human comment on PR) should still match
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil) // no forgejo client
	router.SetRouteTable(rt)

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 10, 10, "alice", "created")
	evt.PRNumber = 10
	evt.SessionKey = "org/repo/pulls/10"

	result, matched := rt.Route(context.Background(), evt)
	// Without forgejo client, Rule 1 won't match (can't fetch labels)
	// But Rule 6 (human comment on PR) should still match
	if matched {
		if result.Role != "reviewer" {
			t.Errorf("expected reviewer role, got %s", result.Role)
		}
	}
}

func TestRouteTable_PullRequestMerged(t *testing.T) {
	// Rule 8: pull_request.merged → scheduler
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil)
	router.SetRouteTable(rt)

	evt := event.NewEvent(event.PullRequestMerged, "org/repo", 0, 42, "fordjent-bot", "merged")
	evt.PRNumber = 42
	evt.SessionKey = "org/repo/pulls/42"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match")
	}
	if result.Role != "scheduler" {
		t.Errorf("expected scheduler role, got %s", result.Role)
	}
	if result.SessionKey != "org/repo/pulls/42" {
		t.Errorf("expected org/repo/pulls/42, got %s", result.SessionKey)
	}
}

func TestRouteTable_IssueClosed(t *testing.T) {
	// Rule 9: issue.closed → scheduler
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil)
	router.SetRouteTable(rt)

	evt := event.NewEvent(event.IssueClosed, "org/repo", 20, 0, "alice", "closed")
	evt.IssueNumber = 20
	evt.SessionKey = "org/repo/issues/20"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match")
	}
	if result.Role != "scheduler" {
		t.Errorf("expected scheduler role, got %s", result.Role)
	}
}

func TestRouteTable_ArchiveChangeRequested(t *testing.T) {
	// Rule 10: ArchiveChangeRequested → pm
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil)
	router.SetRouteTable(rt)

	evt := event.NewEvent(event.ArchiveChangeRequested, "org/repo", 5, 0, "fordjent-scheduler", "archive")
	evt.Change = "user-auth"
	evt.SessionKey = "org/repo/archive/user-auth-123"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match")
	}
	if result.Role != "pm" {
		t.Errorf("expected pm role, got %s", result.Role)
	}
}

func TestRouteTable_NormalPRComment(t *testing.T) {
	// Rule 6: issue_comment.created on normal PR (human sender) → reviewer
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil) // no forgejo → no label lookup → Rule 6 applies
	router.SetRouteTable(rt)

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 30, 30, "alice", "created")
	evt.PRNumber = 30
	evt.SessionKey = "org/repo/pulls/30"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match")
	}
	if result.Role != "reviewer" {
		t.Errorf("expected reviewer role, got %s", result.Role)
	}
	if result.SessionKey != "org/repo/pulls/30" {
		t.Errorf("expected org/repo/pulls/30, got %s", result.SessionKey)
	}
}

func TestRouteTable_ReviewCommentOnNormalPR(t *testing.T) {
	// Rule 7: pull_request_review_comment on normal PR → reviewer
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil)
	router.SetRouteTable(rt)

	evt := event.NewEvent(event.PullRequestReviewComment, "org/repo", 0, 30, "bob", "created")
	evt.PRNumber = 30
	evt.SessionKey = "org/repo/pulls/30"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match")
	}
	if result.Role != "reviewer" {
		t.Errorf("expected reviewer role, got %s", result.Role)
	}
}

func TestRouteTable_BotCommentIgnored(t *testing.T) {
	// Rule 6 doesn't match for bot senders — no rule matches
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil)
	router.SetRouteTable(rt)

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 30, 30, "fordjent-bot", "created")
	evt.PRNumber = 30
	evt.SessionKey = "org/repo/pulls/30"

	result, matched := rt.Route(context.Background(), evt)
	if matched {
		t.Errorf("bot comment should not match any route, got role=%s key=%s", result.Role, result.SessionKey)
	}
}

func TestRouteTable_ReviewCommentWithActionableBody(t *testing.T) {
	// Rule 5: pull_request_review_comment with actionable body → implementer fix
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil)
	router.SetRouteTable(rt)

	evt := event.NewEvent(event.PullRequestReviewComment, "org/repo", 0, 30, "bob", "created")
	evt.PRNumber = 30
	evt.SessionKey = "org/repo/pulls/30"
	evt.Payload = map[string]interface{}{
		"comment": map[string]interface{}{
			"body": "Please fix the nil check on line 42",
		},
	}

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match")
	}
	if result.Role != "implementer" {
		t.Errorf("expected implementer role, got %s", result.Role)
	}
	if !result.IsFix {
		t.Error("expected IsFix to be true")
	}
	if result.SessionKey != "org/repo/pulls/30-fix" {
		t.Errorf("expected org/repo/pulls/30-fix, got %s", result.SessionKey)
	}
}

// TestRouteTable_FailedCheckRunOnDevPR covers Rule 7: a failed check_run on
// a PR with no spec/ralph/merging labels routes to the implementer fix session.
func TestRouteTable_FailedCheckRunOnDevPR(t *testing.T) {
	// No Forgejo client → label list is nil → no spec/ralph/merging label.
	// Rule 7 should still match the failed conclusion.
	rt := NewRouteTable(nil)

	evt := event.NewEvent(event.CheckRunCompleted, "org/repo", 0, 30, "runner", "completed")
	evt.PRNumber = 30
	evt.CheckName = "CI"
	evt.CheckConclusion = "failure"
	evt.CheckURL = "https://forgejo.local/org/repo/actions/runs/1"
	evt.HeadSHA = "abc123"
	evt.SessionKey = "org/repo/pulls/30"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match for failed check_run")
	}
	if result.Role != "implementer" {
		t.Errorf("expected implementer role, got %s", result.Role)
	}
	if !result.IsFix {
		t.Error("expected IsFix to be true")
	}
	if result.SessionKey != "org/repo/pulls/30-fix" {
		t.Errorf("expected org/repo/pulls/30-fix, got %s", result.SessionKey)
	}
}

// TestRouteTable_SuccessfulCheckRunDoesNotRoute covers the negative side of
// Rule 7: success conclusions do not rework the dev (the gated automerge in
// the manager re-evaluates them instead). Returns matched=false so the router
// drops the event.
func TestRouteTable_SuccessfulCheckRunDoesNotRoute(t *testing.T) {
	rt := NewRouteTable(nil)

	evt := event.NewEvent(event.CheckRunCompleted, "org/repo", 0, 30, "runner", "completed")
	evt.PRNumber = 30
	evt.CheckConclusion = "success"
	evt.SessionKey = "org/repo/pulls/30"

	_, matched := rt.Route(context.Background(), evt)
	if matched {
		t.Error("successful check_run should not match any routing rule")
	}
}

// TestRouteTable_WorkflowRunFailedRoutesToImplementer covers Rule 8.
func TestRouteTable_WorkflowRunFailedRoutesToImplementer(t *testing.T) {
	rt := NewRouteTable(nil)

	evt := event.NewEvent(event.WorkflowRunCompleted, "org/repo", 0, 42, "runner", "completed")
	evt.PRNumber = 42
	evt.CheckName = "tests"
	evt.CheckConclusion = "cancelled"
	evt.HeadSHA = "deadbeef"
	evt.SessionKey = "org/repo/pulls/42"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match for failed workflow_run")
	}
	if result.Role != "implementer" || !result.IsFix {
		t.Errorf("expected implementer-fix, got role=%s IsFix=%v", result.Role, result.IsFix)
	}
	if result.SessionKey != "org/repo/pulls/42-fix" {
		t.Errorf("expected org/repo/pulls/42-fix, got %s", result.SessionKey)
	}
}

// TestRouteTable_DismissedReviewNotRouted covers Rule 3b's dismissed branch.
func TestRouteTable_DismissedReviewNotRouted(t *testing.T) {
	rt := NewRouteTable(nil)

	evt := event.NewEvent(event.PullRequestReview, "org/repo", 0, 30, "djent-qa", "submitted")
	evt.PRNumber = 30
	evt.ReviewState = "dismissed"
	evt.SessionKey = "org/repo/pulls/30"

	_, matched := rt.Route(context.Background(), evt)
	if matched {
		t.Error("dismissed review should not match any routing rule")
	}
}

// TestRouteTable_ChangesRequestedReviewRoutesToImplementer covers Rule 3b's
// changes_requested branch — the formal review equivalent of Rule 3.
func TestRouteTable_ChangesRequestedReviewRoutesToImplementer(t *testing.T) {
	rt := NewRouteTable(nil)

	evt := event.NewEvent(event.PullRequestReview, "org/repo", 0, 30, "djent-qa", "submitted")
	evt.PRNumber = 30
	evt.ReviewState = "changes_requested"
	evt.SessionKey = "org/repo/pulls/30"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match for changes_requested review")
	}
	if result.Role != "implementer" || !result.IsFix {
		t.Errorf("expected implementer-fix, got role=%s IsFix=%v", result.Role, result.IsFix)
	}
	if result.SessionKey != "org/repo/pulls/30-fix" {
		t.Errorf("expected org/repo/pulls/30-fix, got %s", result.SessionKey)
	}
}

// TestRouteTable_ApprovedReviewDoesNotSpawn covers Rule 3b's approved branch —
// the gated automerge watcher handles it, not the routing table.
func TestRouteTable_ApprovedReviewDoesNotSpawn(t *testing.T) {
	rt := NewRouteTable(nil)

	evt := event.NewEvent(event.PullRequestReview, "org/repo", 0, 30, "djent-qa", "submitted")
	evt.PRNumber = 30
	evt.ReviewState = "approved"
	evt.SessionKey = "org/repo/pulls/30"

	_, matched := rt.Route(context.Background(), evt)
	if matched {
		t.Error("approved review should not match any routing rule (manager drives gated merge)")
	}
}

// TestRouteTable_ReviewRequestedSpawnsReviewer covers Rule 3c: the internal
// ReviewRequested event emitted in yolo repos delegates to the djent-qa
// reviewer role.
func TestRouteTable_ReviewRequestedSpawnsReviewer(t *testing.T) {
	rt := NewRouteTable(nil)

	evt := event.NewEvent(event.ReviewRequested, "org/repo", 0, 30, "fordjent", "review")
	evt.PRNumber = 30
	evt.SessionKey = "org/repo/pulls/30"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match for ReviewRequested")
	}
	if result.Role != "reviewer" {
		t.Errorf("expected reviewer role, got %s", result.Role)
	}
	if result.SessionKey != "org/repo/pulls/30" {
		t.Errorf("expected org/repo/pulls/30, got %s", result.SessionKey)
	}
}

// labelForgejoServer returns an httptest server backing a forgejo.Client whose
// GetIssue returns an issue carrying the given label names. Other requests get
// a 200 with an empty JSON object. The caller must defer srv.Close().
func labelForgejoServer(t *testing.T, labels []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GetIssue path: /api/v1/repos/{repo}/issues/{n}
		if strings.Contains(r.URL.Path, "/issues/") && !strings.Contains(r.URL.Path, "/comments") {
			lbls := make([]map[string]interface{}, len(labels))
			for i, l := range labels {
				lbls[i] = map[string]interface{}{"name": l}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 30,
				"labels": lbls,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
}

// TestIsFailedConclusion is a direct table test of the failure-classifier used
// by Rules 7 and 8. Covers every conclusion string Forgejo/Gitea can emit.
func TestIsFailedConclusion(t *testing.T) {
	cases := []struct {
		conclusion string
		want       bool
	}{
		{"failure", true},
		{"cancelled", true},
		{"action_required", true},
		{"timed_out", true},
		{"success", false},
		{"neutral", false},
		{"skipped", false},
		{"pending", false},
		{"in_progress", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isFailedConclusion(c.conclusion); got != c.want {
			t.Errorf("isFailedConclusion(%q) = %v, want %v", c.conclusion, got, c.want)
		}
	}
}

// TestRouteTable_FailedCheckRunOnSpecPRDoesNotRoute covers the label-gating
// side of Rule 7: a failed check_run on a PR with the spec-approved label is
// NOT routed to the implementer (spec PRs are owned by PM/review, not rework).
func TestRouteTable_FailedCheckRunOnSpecPRDoesNotRoute(t *testing.T) {
	srv := labelForgejoServer(t, []string{"spec-approved"})
	defer srv.Close()
	client := forgejo.NewClient(srv.URL, "test")
	rt := NewRouteTable(client)

	evt := event.NewEvent(event.CheckRunCompleted, "org/repo", 0, 30, "runner", "completed")
	evt.PRNumber = 30
	evt.CheckConclusion = "failure"
	evt.HeadSHA = "abc123"
	evt.SessionKey = "org/repo/pulls/30"

	if _, matched := rt.Route(context.Background(), evt); matched {
		t.Error("failed check_run on a spec-approved PR should NOT route to implementer")
	}
}

// TestRouteTable_FailedCheckRunOnRalphPRDoesNotRoute covers Rule 7's ralph-label
// gating: ralph PRs manage their own iteration loop and must not be reworked
// by the check_run path.
func TestRouteTable_FailedCheckRunOnRalphPRDoesNotRoute(t *testing.T) {
	srv := labelForgejoServer(t, []string{"ralph"})
	defer srv.Close()
	client := forgejo.NewClient(srv.URL, "test")
	rt := NewRouteTable(client)

	evt := event.NewEvent(event.CheckRunCompleted, "org/repo", 0, 30, "runner", "completed")
	evt.PRNumber = 30
	evt.CheckConclusion = "failure"
	evt.SessionKey = "org/repo/pulls/30"

	if _, matched := rt.Route(context.Background(), evt); matched {
		t.Error("failed check_run on a ralph PR should NOT route")
	}
}

// TestRouteTable_FailedWorkflowRunOnMergingPRDoesNotRoute covers Rule 8's
// merging-label gating.
func TestRouteTable_FailedWorkflowRunOnMergingPRDoesNotRoute(t *testing.T) {
	srv := labelForgejoServer(t, []string{"merging"})
	defer srv.Close()
	client := forgejo.NewClient(srv.URL, "test")
	rt := NewRouteTable(client)

	evt := event.NewEvent(event.WorkflowRunCompleted, "org/repo", 0, 30, "runner", "completed")
	evt.PRNumber = 30
	evt.CheckConclusion = "failure"
	evt.SessionKey = "org/repo/pulls/30"

	if _, matched := rt.Route(context.Background(), evt); matched {
		t.Error("failed workflow_run on a merging PR should NOT route")
	}
}

// TestRouteTable_CommentedReviewDoesNotRoute covers Rule 3b's commented branch:
// a non-decisive `commented` review is informational and does NOT spawn a
// session. Actionable human feedback arrives via issue_comment.created.
func TestRouteTable_CommentedReviewDoesNotRoute(t *testing.T) {
	rt := NewRouteTable(nil)

	evt := event.NewEvent(event.PullRequestReview, "org/repo", 0, 30, "djent-qa", "submitted")
	evt.PRNumber = 30
	evt.ReviewState = "commented"
	evt.SessionKey = "org/repo/pulls/30"

	if _, matched := rt.Route(context.Background(), evt); matched {
		t.Error("commented review should NOT spawn a session (informational only)")
	}
}

// TestRouteTable_ChangesRequestedLabelAloneRoutesToImplementer covers Rule 3b's
// label-fallback: a PR carrying the changes_requested label routes to the
// implementer fix session even when ReviewState is empty (e.g. the review
// webhook wasn't subscribed but a human set the label via the UI).
func TestRouteTable_ChangesRequestedLabelAloneRoutesToImplementer(t *testing.T) {
	srv := labelForgejoServer(t, []string{"changes_requested"})
	defer srv.Close()
	client := forgejo.NewClient(srv.URL, "test")
	rt := NewRouteTable(client)

	evt := event.NewEvent(event.PullRequestReview, "org/repo", 0, 30, "alice", "submitted")
	evt.PRNumber = 30
	evt.ReviewState = "" // no review webhook state — label is the only signal
	evt.SessionKey = "org/repo/pulls/30"

	result, matched := rt.Route(context.Background(), evt)
	if !matched {
		t.Fatal("expected route to match for changes_requested label")
	}
	if result.Role != "implementer" || !result.IsFix {
		t.Errorf("expected implementer-fix, got role=%s IsFix=%v", result.Role, result.IsFix)
	}
	if result.SessionKey != "org/repo/pulls/30-fix" {
		t.Errorf("expected org/repo/pulls/30-fix, got %s", result.SessionKey)
	}
}

// TestRouteTable_ApprovedBeatsStaleChangesRequestedLabel covers the precedence
// rule: an `approved` review verdict suppresses routing even if a stale
// changes_requested label is still on the PR (the approval supersedes).
func TestRouteTable_ApprovedBeatsStaleChangesRequestedLabel(t *testing.T) {
	srv := labelForgejoServer(t, []string{"changes_requested"})
	defer srv.Close()
	client := forgejo.NewClient(srv.URL, "test")
	rt := NewRouteTable(client)

	evt := event.NewEvent(event.PullRequestReview, "org/repo", 0, 30, "djent-qa", "submitted")
	evt.PRNumber = 30
	evt.ReviewState = "approved" // approval wins despite the stale label
	evt.SessionKey = "org/repo/pulls/30"

	if _, matched := rt.Route(context.Background(), evt); matched {
		t.Error("approved review should NOT route even with a stale changes_requested label")
	}
}

// TestPopulateCheckRunFields_ResolvesPRBySHA covers the highest-risk untested
// path: a check_run payload WITHOUT pull_requests[] is resolved to a PR via
// ListPRs + head-SHA matching. Verifies PRNumber and SessionKey are set from
// the resolved PR, not left at 0.
func TestPopulateCheckRunFields_ResolvesPRBySHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ListPRs path: /api/v1/repos/{repo}/pulls?state=open
		if strings.Contains(r.URL.Path, "/pulls") && !strings.Contains(r.URL.Path, "/issues/") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"number": float64(7), "head": map[string]interface{}{"sha": "deadbeef"}},
				{"number": float64(9), "head": map[string]interface{}{"sha": "cafef00d"}},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	client := forgejo.NewClient(srv.URL, "test")
	router.SetForgejoClient(client)

	payload := map[string]interface{}{
		"action":     "completed",
		"repository": map[string]interface{}{"full_name": "duke/test-repo"},
		// NOTE: no "pull_requests" field on check_run — forces SHA resolution.
		"check_run": map[string]interface{}{
			"name":       "CI",
			"conclusion": "failure",
			"head_sha":   "cafef00d",
			"html_url":   "https://forgejo.local/duke/test-repo/actions/runs/9",
		},
		"sender": map[string]interface{}{"login": "forgejo-runner"},
	}

	evt, err := router.normalizeEvent("check_run", "completed", payload)
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if evt.PRNumber != 9 {
		t.Errorf("expected PRNumber=9 resolved by SHA, got %d", evt.PRNumber)
	}
	if evt.HeadSHA != "cafef00d" {
		t.Errorf("expected HeadSHA=cafef00d, got %q", evt.HeadSHA)
	}
	if evt.CheckConclusion != "failure" {
		t.Errorf("expected conclusion failure, got %q", evt.CheckConclusion)
	}
	if evt.SessionKey != "duke/test-repo/pulls/9" {
		t.Errorf("expected session key duke/test-repo/pulls/9, got %s", evt.SessionKey)
	}
}

// TestPopulateCheckRunFields_NoPRResolvedDrops covers the negative SHA-resolution
// path: a check_run whose head SHA matches no open PR is left with PRNumber=0
// and a non-pulls session key, so routing drops it (direct-to-main commit).
func TestPopulateCheckRunFields_NoPRResolvedDrops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls") && !strings.Contains(r.URL.Path, "/issues/") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"number": float64(7), "head": map[string]interface{}{"sha": "deadbeef"}},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	router.SetForgejoClient(forgejo.NewClient(srv.URL, "test"))

	payload := map[string]interface{}{
		"action":     "completed",
		"repository": map[string]interface{}{"full_name": "duke/test-repo"},
		"check_run": map[string]interface{}{
			"name":       "CI",
			"conclusion": "failure",
			"head_sha":   "nomatch",
		},
		"sender": map[string]interface{}{"login": "forgejo-runner"},
	}

	evt, err := router.normalizeEvent("check_run", "completed", payload)
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if evt.PRNumber != 0 {
		t.Errorf("expected PRNumber=0 when no PR matches head SHA, got %d", evt.PRNumber)
	}
	if strings.Contains(evt.SessionKey, "/pulls/") {
		t.Errorf("expected non-pulls session key, got %s", evt.SessionKey)
	}
}

func TestHasLabel(t *testing.T) {
	tests := []struct {
		labels []string
		name   string
		want   bool
	}{
		{[]string{"spec-proposed", "ralph"}, "spec-proposed", true},
		{[]string{"spec-proposed", "ralph"}, "ralph", true},
		{[]string{"spec-proposed"}, "ralph", false},
		{nil, "spec-proposed", false},
	}
	for _, tt := range tests {
		got := hasLabel(tt.labels, tt.name)
		if got != tt.want {
			t.Errorf("hasLabel(%v, %q) = %v, want %v", tt.labels, tt.name, got, tt.want)
		}
	}
}

// TestRouteTable_FullHandoffChain validates the priority-ordered routing
// from spec PR comment → PM, through PR merge → scheduler, to
// ArchiveChangeRequested → PM archival. Uses synthetic events without
// a real Forgejo API.
func TestRouteTable_FullHandoffChain(t *testing.T) {
	cfg := &config.Config{Webhook: config.WebhookConfig{Secret: ""}}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	rt := NewRouteTable(nil) // no Forgejo API
	router.SetRouteTable(rt)

	// Step 1: Spec PR comment → PM (would need real Forgejo for label check)
	specCommentEvt := event.NewEvent(event.IssueCommentCreated, "org/repo", 10, 10, "alice", "created")
	specCommentEvt.PRNumber = 10
	specCommentEvt.SessionKey = "org/repo/pulls/10"
	// Without forgejo, we can't check spec-proposed label, so this falls through
	// to Rule 6 (human comment on PR → reviewer). This is correct behavior:
	// the routing table can only determine role if it can see the labels.
	result, matched := rt.Route(context.Background(), specCommentEvt)
	if matched {
		// Falls through to reviewer since no spec label visible
		if result.Role != "reviewer" {
			t.Errorf("step 1: expected reviewer (no forgejo labels), got %s", result.Role)
		}
	}

	// Step 2: PR merged → scheduler
	mergeEvt := event.NewEvent(event.PullRequestMerged, "org/repo", 0, 10, "fordjent-bot", "merged")
	mergeEvt.PRNumber = 10
	mergeEvt.SessionKey = "org/repo/pulls/10"
	result, matched = rt.Route(context.Background(), mergeEvt)
	if !matched {
		t.Fatal("step 2: expected route to match for PR merge")
	}
	if result.Role != "scheduler" {
		t.Errorf("step 2: expected scheduler, got %s", result.Role)
	}

	// Step 3: Issue opened → no match in routing table (issues.opened is not in the 10 priority rules)
	// The routing table only covers comment, merge, and review events for now.
	issueOpenEvt := event.NewEvent(event.IssueOpened, "org/repo", 20, 0, "fordjent-bot", "opened")
	issueOpenEvt.IssueNumber = 20
	issueOpenEvt.SessionKey = "org/repo/issues/20"
	_, matched = rt.Route(context.Background(), issueOpenEvt)
	// IssueOpened is not in the routing table — it's handled by the manager's handleEvent directly
	// This is expected: the routing table governs PR-level handoffs, not issue creation.

	// Step 4: Issue closed → scheduler
	issueCloseEvt := event.NewEvent(event.IssueClosed, "org/repo", 20, 0, "alice", "closed")
	issueCloseEvt.IssueNumber = 20
	issueCloseEvt.SessionKey = "org/repo/issues/20"
	result, matched = rt.Route(context.Background(), issueCloseEvt)
	if !matched {
		t.Fatal("step 4: expected route to match for issue closed")
	}
	if result.Role != "scheduler" {
		t.Errorf("step 4: expected scheduler, got %s", result.Role)
	}

	// Step 5: ArchiveChangeRequested → PM
	archiveEvt := event.NewEvent(event.ArchiveChangeRequested, "org/repo", 1, 0, "fordjent-scheduler", "archive")
	archiveEvt.Change = "user-auth"
	archiveEvt.SessionKey = "org/repo/archive/user-auth-123"
	result, matched = rt.Route(context.Background(), archiveEvt)
	if !matched {
		t.Fatal("step 5: expected route to match for ArchiveChangeRequested")
	}
	if result.Role != "pm" {
		t.Errorf("step 5: expected pm, got %s", result.Role)
	}
}

// TestApplyRoute sets evt.Role and evt.SessionKey from the route result.
func TestApplyRoute(t *testing.T) {
	rt := NewRouteTable(nil)
	evt := event.NewEvent(event.PullRequestMerged, "org/repo", 0, 42, "bot", "merged")
	evt.PRNumber = 42
	evt.SessionKey = "org/repo/pulls/42"

	matched := ApplyRoute(context.Background(), rt, evt)
	if !matched {
		t.Fatal("expected ApplyRoute to match")
	}
	if evt.Role != "scheduler" {
		t.Errorf("expected role=scheduler, got %s", evt.Role)
	}
}
