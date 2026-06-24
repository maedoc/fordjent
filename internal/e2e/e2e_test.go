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
	router := webhook.NewRouter(cfg, bus, logger)
	// Wire a route table backed by the router's own (possibly nil) forgejo
	// client so ApplyRoute actually runs and Role/SessionKey get populated.
	// Without this, e2e tests that assert on evt.Role are vacuously true.
	router.SetRouteTable(webhook.NewRouteTable(nil))
	return router
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

func TestPullRequestMergedWebhook(t *testing.T) {
	cfg := testE2EConfig(t)
	bus := event.NewBus()
	router := testRouter(t, cfg, bus)

	payload := map[string]interface{}{
		"action": "closed",
		"repository": map[string]interface{}{"full_name": "duke/test-repo"},
		"pull_request": map[string]interface{}{
			"number":  float64(42),
			"title":   "Spec: user-auth",
			"merged":  true,
			"state":   "closed",
			"head":    map[string]interface{}{"ref": "spec/user-auth", "sha": "abc123"},
			"user":    map[string]interface{}{"login": "duke"},
		},
		"sender": map[string]interface{}{"login": "duke"},
	}
	payloadBytes, _ := json.Marshal(payload)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", strings.NewReader(string(payloadBytes)))
	req.Header.Set("X-Forgejo-Event", "pull_request")
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
		if evt.Type != "pull_request.merged" {
			t.Errorf("expected event type pull_request.merged, got %s", evt.Type)
		}
		if evt.PRNumber != 42 {
			t.Errorf("expected PR #42, got %d", evt.PRNumber)
		}
		if evt.Repository != "duke/test-repo" {
			t.Errorf("expected repo duke/test-repo, got %s", evt.Repository)
		}
		if evt.SessionKey != "duke/test-repo/pulls/42" {
			t.Errorf("expected session key duke/test-repo/pulls/42, got %s", evt.SessionKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event on bus")
	}
}

// TestCheckRunFailed_RoutesToImplementerFix verifies the e2e webhook flow:
// a Forgejo check_run.completed webhook with conclusion=failure produces a
// CheckRunCompleted event on the bus with PRNumber, CheckName, CheckURL,
// and HeadSHA populated from the `check_run.pull_requests[0]` field.
func TestCheckRunFailed_RoutesToImplementerFix(t *testing.T) {
	cfg := testE2EConfig(t)
	bus := event.NewBus()
	router := testRouter(t, cfg, bus)

	payload := map[string]interface{}{
		"action": "completed",
		"repository": map[string]interface{}{"full_name": "duke/test-repo"},
		"check_run": map[string]interface{}{
			"name":       "CI",
			"conclusion": "failure",
			"head_sha":   "abc123",
			"html_url":   "https://forgejo.local/duke/test-repo/actions/runs/42",
			"pull_requests": []interface{}{
				map[string]interface{}{"number": float64(7)},
			},
		},
		"sender": map[string]interface{}{"login": "forgejo-runner"},
	}
	payloadBytes, _ := json.Marshal(payload)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", strings.NewReader(string(payloadBytes)))
	req.Header.Set("X-Forgejo-Event", "check_run")
	req.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(cfg.Webhook.Secret, payloadBytes))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case evt := <-sub:
		if evt.Type != event.CheckRunCompleted {
			t.Fatalf("expected event.Type event.CheckRunCompleted, got %v", evt.Type)
		}
		if evt.PRNumber != 7 {
			t.Errorf("expected PRNumber=7, got %d", evt.PRNumber)
		}
		if evt.CheckName != "CI" {
			t.Errorf("expected CheckName=CI, got %q", evt.CheckName)
		}
		if evt.CheckConclusion != "failure" {
			t.Errorf("expected CheckConclusion=failure, got %q", evt.CheckConclusion)
		}
		if !strings.Contains(evt.CheckURL, "actions/runs/42") {
			t.Errorf("expected CheckURL to contain actions/runs/42, got %q", evt.CheckURL)
		}
		if evt.HeadSHA != "abc123" {
			t.Errorf("expected HeadSHA=abc123, got %q", evt.HeadSHA)
		}
		if evt.SessionKey != "duke/test-repo/pulls/7-fix" {
			t.Errorf("expected session key duke/test-repo/pulls/7-fix, got %s", evt.SessionKey)
		}
		// Critical: a failed check_run MUST be routed to the implementer role
		// at pulls/N-fix. This assertion was previously vacuous (routeTable was
		// nil so Role was always ""); the wired route table makes it meaningful.
		if evt.Role != "implementer" {
			t.Errorf("expected Role=implementer for failed check_run, got %q", evt.Role)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for check_run event on bus")
	}
}

// TestCheckRunSuccess_DoesNotFireRework verifies the negative path: a
// successful check_run.completed event still fires on the bus (manager uses
// it to re-evaluate gated automerge), but the router's Role is NOT implementer
// (no failed-conclusion match).
func TestCheckRunSuccess_DoesNotFireRework(t *testing.T) {
	cfg := testE2EConfig(t)
	bus := event.NewBus()
	router := testRouter(t, cfg, bus)

	payload := map[string]interface{}{
		"action": "completed",
		"repository": map[string]interface{}{"full_name": "duke/test-repo"},
		"check_run": map[string]interface{}{
			"name":       "CI",
			"conclusion": "success",
			"head_sha":   "abc123",
			"html_url":   "https://forgejo.local/duke/test-repo/actions/runs/43",
			"pull_requests": []interface{}{
				map[string]interface{}{"number": float64(7)},
			},
		},
		"sender": map[string]interface{}{"login": "forgejo-runner"},
	}
	payloadBytes, _ := json.Marshal(payload)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", strings.NewReader(string(payloadBytes)))
	req.Header.Set("X-Forgejo-Event", "check_run")
	req.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(cfg.Webhook.Secret, payloadBytes))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case evt := <-sub:
		if evt.Type != event.CheckRunCompleted {
			t.Fatalf("expected event.Type event.CheckRunCompleted, got %v", evt.Type)
		}
		if evt.CheckConclusion != "success" {
			t.Errorf("expected success conclusion, got %q", evt.CheckConclusion)
		}
		// Critical: a success event should NOT route to implementer Role.
		if evt.Role == "implementer" {
			t.Error("successful check_run should not be routed to implementer role")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for success check_run event on bus")
	}
}

// TestPullRequestReview_ChangesRequested parses a Forgejo review webhook
// and verifies the resulting PullRequestReview event carries the verdict.
func TestPullRequestReview_ChangesRequested(t *testing.T) {
	cfg := testE2EConfig(t)
	bus := event.NewBus()
	router := testRouter(t, cfg, bus)

	payload := map[string]interface{}{
		"action": "submitted",
		"repository": map[string]interface{}{"full_name": "duke/test-repo"},
		"pull_request": map[string]interface{}{
			"number": float64(7),
			"head":   map[string]interface{}{"ref": "feature/x", "sha": "abc123"},
			"user":   map[string]interface{}{"login": "duke"},
		},
		"review": map[string]interface{}{
			"id":    float64(1),
			"state": "changes_requested",
			"body":  "Add a test for empty input.",
			"user":  map[string]interface{}{"login": "djent-qa"},
		},
		"sender": map[string]interface{}{"login": "djent-qa"},
	}
	payloadBytes, _ := json.Marshal(payload)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", strings.NewReader(string(payloadBytes)))
	req.Header.Set("X-Forgejo-Event", "pull_request_review")
	req.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(cfg.Webhook.Secret, payloadBytes))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case evt := <-sub:
		if evt.Type != event.PullRequestReview {
			t.Fatalf("expected event.Type event.PullRequestReview, got %v", evt.Type)
		}
		if evt.PRNumber != 7 {
			t.Errorf("expected PRNumber=7, got %d", evt.PRNumber)
		}
		if evt.ReviewState != "changes_requested" {
			t.Errorf("expected ReviewState changes_requested, got %q", evt.ReviewState)
		}
		// The review body should be carried in the payload comment slot so
		// existing comment-body-based routing sees it.
		if cm, ok := evt.Payload["comment"].(map[string]interface{}); !ok || cm["body"] != "Add a test for empty input." {
			t.Errorf("expected review body in payload.comment, got %v", evt.Payload["comment"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pull_request_review event on bus")
	}
}

// TestReworkLoop_FullChain_RoutingOnly verifies the routing layer of the
// full rework loop. Without spinning up sessions, we confirm each event in
// the loop is parsed and routed to the expected role + session key:
//
//   1. dev opens PR in a yolo repo → PullRequestOpened,
//      manager transforms to ReviewRequested → reviewer @ pulls/N
//   2. djent-qa submits changes_requested review → PullRequestReview w/
//      ReviewState="changes_requested" (manager dispatches _fix)
//   3. implementer pushes a fix → CI reruns, fires CheckRunCompleted success
//      → no implementer rework (gated automerge prods the gate)
//   4. djent-qa approves → PullRequestReview w/ ReviewState="approved" →
//      gated automerge fires
//
// This is an e2e test of the router layer, not of the manager's session loop.
// (The session-layer equivalent is in internal/session: TestEvaluateAutomerge_*).
func TestReworkLoop_FullChain_RoutingOnly(t *testing.T) {
	cfg := testE2EConfig(t)
	bus := event.NewBus()
	router := testRouter(t, cfg, bus)

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	send := func(t *testing.T, eventType string, payload map[string]interface{}) *event.Event {
		t.Helper()
		payloadBytes, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/acp/v1/events", strings.NewReader(string(payloadBytes)))
		req.Header.Set("X-Forgejo-Event", eventType)
		req.Header.Set("X-Hub-Signature-256", "sha256="+computeHMAC(cfg.Webhook.Secret, payloadBytes))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("event %s: expected 200, got %d: %s", eventType, w.Code, w.Body.String())
		}
		select {
		case evt := <-sub:
			return evt
		case <-time.After(2 * time.Second):
			t.Fatalf("event %s: timed out waiting on bus", eventType)
			return nil
		}
	}

	repo := "duke/test-repo"
	pr := float64(7)
	repoPayload := func() map[string]interface{} {
		return map[string]interface{}{"full_name": repo}
	}

	// 1. dev opens PR.
	evt1 := send(t, "pull_request", map[string]interface{}{
		"action":      "opened",
		"repository":  repoPayload(),
		"pull_request": map[string]interface{}{
			"number": pr,
			"head":   map[string]interface{}{"ref": "feature/x", "sha": "abc123"},
			"user":   map[string]interface{}{"login": "djent-dev"},
		},
		"sender": map[string]interface{}{"login": "djent-dev"},
	})
	if evt1.Type != event.PullRequestOpened {
		t.Errorf("step 1: expected PullRequestOpened, got %v", evt1.Type)
	}
	if evt1.PRNumber != 7 {
		t.Errorf("step 1: expected PR 7, got %d", evt1.PRNumber)
	}

	// 2. djent-qa submits changes_requested review.
	evt2 := send(t, "pull_request_review", map[string]interface{}{
		"action":     "submitted",
		"repository": repoPayload(),
		"pull_request": map[string]interface{}{
			"number": pr,
			"head":   map[string]interface{}{"ref": "feature/x", "sha": "abc123"},
			"user":   map[string]interface{}{"login": "djent-dev"},
		},
		"review": map[string]interface{}{
			"id":    float64(1),
			"state": "changes_requested",
			"body":  "Add a test for empty input.",
			"user":  map[string]interface{}{"login": "djent-qa"},
		},
		"sender": map[string]interface{}{"login": "djent-qa"},
	})
	if evt2.Type != event.PullRequestReview {
		t.Errorf("step 2: expected PullRequestReview, got %v", evt2.Type)
	}
	if evt2.ReviewState != "changes_requested" {
		t.Errorf("step 2: expected ReviewState changes_requested, got %q", evt2.ReviewState)
	}
	if evt2.PRNumber != 7 {
		t.Errorf("step 2: expected PR 7, got %d", evt2.PRNumber)
	}

	// 3. CI reruns and goes green after the implementer's fix.
	evt3 := send(t, "check_run", map[string]interface{}{
		"action":     "completed",
		"repository": repoPayload(),
		"check_run": map[string]interface{}{
			"name":       "CI",
			"conclusion": "success",
			"head_sha":   "abc123",
			"html_url":   "https://forgejo.local/duke/test-repo/actions/runs/44",
			"pull_requests": []interface{}{
				map[string]interface{}{"number": pr},
			},
		},
		"sender": map[string]interface{}{"login": "forgejo-runner"},
	})
	if evt3.Type != event.CheckRunCompleted {
		t.Errorf("step 3: expected CheckRunCompleted, got %v", evt3.Type)
	}
	if evt3.CheckConclusion != "success" {
		t.Errorf("step 3: expected success conclusion, got %q", evt3.CheckConclusion)
	}
	// Success should NOT be routed to implementer (rule 7 only matches failures).
	if evt3.Role == "implementer" {
		t.Error("step 3: successful check_run should not route to implementer")
	}

	// 4. djent-qa approves — the pull_request_review event with approved state.
	evt4 := send(t, "pull_request_review", map[string]interface{}{
		"action":     "submitted",
		"repository": repoPayload(),
		"pull_request": map[string]interface{}{
			"number": pr,
			"head":   map[string]interface{}{"ref": "feature/x", "sha": "abc123"},
			"user":   map[string]interface{}{"login": "djent-dev"},
		},
		"review": map[string]interface{}{
			"id":    float64(2),
			"state": "approved",
			"body":  "LGTM",
			"user":  map[string]interface{}{"login": "djent-qa"},
		},
		"sender": map[string]interface{}{"login": "djent-qa"},
	})
	if evt4.Type != event.PullRequestReview {
		t.Errorf("step 4: expected PullRequestReview, got %v", evt4.Type)
	}
	if evt4.ReviewState != "approved" {
		t.Errorf("step 4: expected ReviewState approved, got %q", evt4.ReviewState)
	}
}
