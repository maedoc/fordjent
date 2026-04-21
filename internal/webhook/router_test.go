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
		Webhook: config.WebhookConfig{Secret: ""},
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
		Security: config.SecurityConfig{FilterAgentEvents: true},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action": "push",
		"repository": map[string]interface{}{"full_name": "org/repo"},
		"sender": map[string]interface{}{"login": "alice"},
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
	if w.Body.String() != "filtered\n" {
		t.Errorf("expected filtered, got: %s", w.Body.String())
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
	// Push events get a unique session key
	if evt.SessionKey == "" {
		t.Error("expected non-empty session key for push")
	}
}
