package webhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestWebhookMissingSignature(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: "test-secret"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action": "opened",
		"repository": map[string]interface{}{
			"full_name": "org/repo",
		},
		"sender": map[string]interface{}{
			"login": "alice",
		},
		"issue": map[string]interface{}{
			"number": float64(42),
		},
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
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action": "opened",
		"repository": map[string]interface{}{
			"full_name": "org/repo",
		},
		"sender": map[string]interface{}{
			"login": "alice",
		},
		"issue": map[string]interface{}{
			"number": float64(42),
		},
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

func TestWebhookLoopPrevention(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
		Security: config.SecurityConfig{
			FilterAgentEvents: true,
		},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	// Push with agent commit
	payload := map[string]interface{}{
		"action": "push",
		"repository": map[string]interface{}{
			"full_name": "org/repo",
		},
		"sender": map[string]interface{}{
			"login": "alice",
		},
		"commits": []interface{}{
			map[string]interface{}{
				"message": "[agent-automation] auto-fix: update README",
			},
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
		t.Errorf("expected filtered response, got: %s", w.Body.String())
	}
}

func TestNormalizeEvent(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: ""},
		Agent:   config.AgentConfig{CommitPrefix: "[agent-automation]"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	payload := map[string]interface{}{
		"action": "created",
		"repository": map[string]interface{}{
			"full_name": "org/repo",
		},
		"sender": map[string]interface{}{
			"login": "alice",
		},
		"issue": map[string]interface{}{
			"number": float64(42),
		},
		"comment": map[string]interface{}{
			"id":   float64(100),
			"body": "Hello @fordjent can you help?",
		},
	}

	evt, err := router.normalizeEvent("issue_comment", "created", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Repository != "org/repo" {
		t.Errorf("expected org/repo, got %s", evt.Repository)
	}
	if evt.IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", evt.IssueNumber)
	}
	if evt.Sender != "alice" {
		t.Errorf("expected sender alice, got %s", evt.Sender)
	}
	if evt.SessionKey != "org/repo/issues/42" {
		t.Errorf("expected session key org/repo/issues/42, got %s", evt.SessionKey)
	}
	if evt.Type != event.IssueCommentCreated {
		t.Errorf("expected type %s, got %s", event.IssueCommentCreated, evt.Type)
	}
}
