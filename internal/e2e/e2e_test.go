package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/webhook"
)

func testE2EConfig(t *testing.T) *config.Config {
	return &config.Config{
		Agent: config.AgentConfig{
			WorkDir:             t.TempDir(),
			MaxSessions:         5,
			IdleTimeout:         1 * time.Hour,
			MaxTurns:            25,
			ContextWindow:       128000,
			CompactionThreshold: 0.8,
			SessionTimeout:      30 * time.Minute,
		},
		Forgejo: config.ForgejoConfig{
			URL:   "http://forgejo-local:3000",
			Token: "test-token",
		},
		Webhook: config.WebhookConfig{Secret: "test-secret"},
		Security: config.SecurityConfig{
			FilterAgentEvents: false,
		},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "http://localhost:11434/v1", Model: "test-model", MaxTokens: 1024},
		},
	}
}

func testRouter(t *testing.T, cfg *config.Config, bus *event.Bus) *webhook.Router {
	logger := slog.Default()
	return webhook.NewRouter(cfg, bus, logger)
}

func computeHMAC(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookToEvent(t *testing.T) {
	cfg := testE2EConfig(t)
	bus := event.NewBus()
	router := testRouter(t, cfg, bus)

	payload := map[string]interface{}{
		"action":     "opened",
		"repository": map[string]interface{}{"full_name": "duke/test-repo"},
		"issue":      map[string]interface{}{"number": float64(1), "title": "Test issue", "body": "Test body"},
		"sender":     map[string]interface{}{"login": "duke"},
	}
	payloadBytes, _ := json.Marshal(payload)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", strings.NewReader(string(payloadBytes)))
	req.Header.Set("X-Forgejo-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(cfg.Webhook.Secret, payloadBytes))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		body, _ := io.ReadAll(w.Body)
		t.Fatalf("expected 200, got %d: %s", w.Code, string(body))
	}

	select {
	case evt := <-sub:
		if evt.Repository != "duke/test-repo" {
			t.Errorf("expected repo duke/test-repo, got %s", evt.Repository)
		}
		if evt.IssueNumber != 1 {
			t.Errorf("expected issue #1, got %d", evt.IssueNumber)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event on bus")
	}
}

func TestHealthEndpoint(t *testing.T) {
	cfg := testE2EConfig(t)
	bus := event.NewBus()
	router := testRouter(t, cfg, bus)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if string(body) != "ok\n" {
		t.Errorf("expected 'ok', got %q", string(body))
	}
}

func TestMetricsEndpoint(t *testing.T) {
	cfg := testE2EConfig(t)
	bus := event.NewBus()
	router := testRouter(t, cfg, bus)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
