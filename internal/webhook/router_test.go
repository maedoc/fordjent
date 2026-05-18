package webhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		"action": "opened",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender": map[string]interface{}{"login": "alice"},
		"issue": map[string]interface{}{"number": float64(42)},
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
		"action": "opened",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender": map[string]interface{}{"login": "alice"},
		"issue": map[string]interface{}{"number": float64(42)},
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
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	// Push events with ref+commits must NEVER be filtered, even from bots
	payload := map[string]interface{}{
		"action": "push",
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
		"action": "created",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender": map[string]interface{}{"login": "alice"},
		"issue": map[string]interface{}{"number": float64(42)},
		"comment": map[string]interface{}{"id": float64(100), "body": "help"},
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
		"action": "opened",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender": map[string]interface{}{"login": "bob"},
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
		"issue": map[string]interface{}{"number": float64(1)},
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
		"action": "created",
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
		"action": "label_updated",
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
		"action": "label_updated",
		"repository":  map[string]interface{}{"full_name": "org/repo"},
		"sender":      map[string]interface{}{"login": "alice"},
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
		"action": "closed",
		"repository":  map[string]interface{}{"full_name": "org/repo"},
		"sender":      map[string]interface{}{"login": "alice"},
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
		"action": "closed",
		"repository":  map[string]interface{}{"full_name": "org/repo"},
		"sender":      map[string]interface{}{"login": "alice"},
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
		"action": "created",
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
		"action": "created",
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
		"action": "created",
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
		"action": "created",
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
		"action": "opened",
		"repository":  map[string]interface{}{"full_name": "org/repo"},
		"sender":      map[string]interface{}{"login": "fordjent-bot"},
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
		"action": "synchronize",
		"repository":  map[string]interface{}{"full_name": "org/repo"},
		"sender":      map[string]interface{}{"login": "fordjent-bot"},
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
		"action": "closed",
		"repository":  map[string]interface{}{"full_name": "org/repo"},
		"sender":      map[string]interface{}{"login": "fordjent-bot"},
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
		"action": "opened",
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
		"action": "opened",
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
		Webhook:  config.WebhookConfig{Secret: ""},
		Forgejo:  config.ForgejoConfig{URL: srv.URL, Token: "test"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	router.SetForgejoClient(forgejo.NewClient(srv.URL, "test"))

	payload := map[string]interface{}{
		"action": "created",
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
		Webhook:  config.WebhookConfig{Secret: ""},
		Forgejo:  config.ForgejoConfig{URL: srv.URL, Token: "test"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	router.SetForgejoClient(forgejo.NewClient(srv.URL, "test"))

	payload := map[string]interface{}{
		"action": "created",
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
		"action": "created",
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
		"action": "created",
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
