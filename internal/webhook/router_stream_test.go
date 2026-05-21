package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/lifecycle"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

func TestSSEStreamHandler_GET_Content(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "lifecycle.db")
	lc, err := lifecycle.New(dbPath, nil, nil)
	if err != nil {
		t.Fatalf("failed to create lifecycle: %v", err)
	}

	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: "test"},
		Agent:   config.AgentConfig{WorkDir: dir},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())
	router.SetLifecycle(lc)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/acp/v1/stream", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %s", ct)
	}
	_ = os.Remove(dbPath)
}

func TestSSEStreamHandler_POST_MethodNotAllowed(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: "test"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/stream", nil)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", w.Code)
	}
}

func TestSSEStreamHandler_NoLifecycle(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{Secret: "test"},
	}
	bus := event.NewBus()
	router := NewRouter(cfg, bus, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/acp/v1/stream", nil)
	w := httptest.NewRecorder()
	router.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 without lifecycle, got %d", w.Code)
	}
}