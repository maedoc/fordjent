package autoreg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fordjent/fordjent/internal/forgejo"
)

// fakeForgejoServer creates a test HTTP server that simulates Forgejo API
// endpoints needed by AutoRegistrar: ListWebhooks, CreateWebhook, ListLabels, CreateLabel.
// It tracks state per-repo based on the URL path.
type fakeForgejoServer struct {
	server        *httptest.Server
	webhookCalls  int // count of CreateWebhook calls
	labelCalls    int // count of EnsureLabels-related CreateLabel calls
	registry      map[string]*repoState // repo → state
}

type repoState struct {
	webhooks []map[string]interface{}
	labels   map[string]bool
}

func newFakeForgejoServer() *fakeForgejoServer {
	f := &fakeForgejoServer{
		registry: make(map[string]*repoState),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		method := r.Method

		// Extract repo from path: /api/v1/repos/{owner}/{repo}/...
		// After stripping the prefix, the next two segments are the repo
		rest := strings.TrimPrefix(path, "/api/v1/repos/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		repo := parts[0] + "/" + parts[1]
		suffix := ""
		if len(parts) > 2 {
			suffix = parts[2]
		}

		state := f.registry[repo]
		if state == nil {
			state = &repoState{labels: make(map[string]bool)}
			f.registry[repo] = state
		}

		switch {
		case suffix == "hooks" && method == http.MethodGet:
			// ListWebhooks
			hooks := state.webhooks
			if hooks == nil {
				hooks = []map[string]interface{}{}
			}
			json.NewEncoder(w).Encode(hooks)

		case suffix == "hooks" && method == http.MethodPost:
			// CreateWebhook
			f.webhookCalls++
			var payload map[string]interface{}
			json.NewDecoder(r.Body).Decode(&payload)
			hook := map[string]interface{}{
				"id":     len(state.webhooks) + 1,
				"type":   payload["type"],
				"active": true,
				"config": payload["config"],
				"events": payload["events"],
			}
			state.webhooks = append(state.webhooks, hook)
			json.NewEncoder(w).Encode(hook)

		case suffix == "labels" && method == http.MethodGet:
			// ListLabels
			var labels []map[string]interface{}
			for name := range state.labels {
				labels = append(labels, map[string]interface{}{"id": len(labels) + 1, "name": name})
			}
			if labels == nil {
				labels = []map[string]interface{}{}
			}
			json.NewEncoder(w).Encode(labels)

		case suffix == "labels" && method == http.MethodPost:
			// CreateLabel
			f.labelCalls++
			var payload map[string]interface{}
			json.NewDecoder(r.Body).Decode(&payload)
			name, _ := payload["name"].(string)
			state.labels[name] = true
			json.NewEncoder(w).Encode(map[string]interface{}{"id": len(state.labels) + 1, "name": name})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeForgejoServer) Close() { f.server.Close() }

func (f *fakeForgejoServer) URL() string { return f.server.URL }

func (f *fakeForgejoServer) Client() *forgejo.Client {
	return forgejo.NewClient(f.URL(), "test-token")
}

func TestEnsureRegistered_CacheHit(t *testing.T) {
	f := newFakeForgejoServer()
	defer f.Close()

	ar := NewAutoRegistrar(AutoRegistrarConfig{
		ForgejoClient: f.Client(),
		WebhookURL:    "https://fordjent.example.com/acp/v1/events",
		WebhookSecret: "secret",
	})

	ctx := context.Background()

	// First call — should hit the API
	if err := ar.EnsureRegistered(ctx, "fjadmin/testrepo"); err != nil {
		t.Fatalf("first EnsureRegistered failed: %v", err)
	}

	firstWebhookCalls := f.webhookCalls
	firstLabelCalls := f.labelCalls

	// Second call — should hit the cache, no API calls
	if err := ar.EnsureRegistered(ctx, "fjadmin/testrepo"); err != nil {
		t.Fatalf("cached EnsureRegistered failed: %v", err)
	}

	if f.webhookCalls != firstWebhookCalls {
		t.Fatalf("expected no additional webhook calls on cache hit, got %d (was %d)", f.webhookCalls, firstWebhookCalls)
	}
	if f.labelCalls != firstLabelCalls {
		t.Fatalf("expected no additional label calls on cache hit, got %d (was %d)", f.labelCalls, firstLabelCalls)
	}
}

func TestEnsureRegistered_WebhookCreated(t *testing.T) {
	f := newFakeForgejoServer()
	defer f.Close()

	ar := NewAutoRegistrar(AutoRegistrarConfig{
		ForgejoClient: f.Client(),
		WebhookURL:    "https://fordjent.example.com/acp/v1/events",
		WebhookSecret: "my-secret",
	})

	ctx := context.Background()

	if err := ar.EnsureRegistered(ctx, "fjadmin/newrepo"); err != nil {
		t.Fatalf("EnsureRegistered failed: %v", err)
	}

	// Verify webhook was created
	if f.webhookCalls != 1 {
		t.Fatalf("expected 1 webhook creation, got %d", f.webhookCalls)
	}

	// Verify the webhook has the right URL
	state := f.registry["fjadmin/newrepo"]
	if state == nil || len(state.webhooks) != 1 {
		t.Fatalf("expected 1 webhook in repo state, got %d", len(state.webhooks))
	}
	config, _ := state.webhooks[0]["config"].(map[string]interface{})
	if config["url"] != "https://fordjent.example.com/acp/v1/events" {
		t.Fatalf("expected webhook URL to match, got config: %v", config)
	}
}

func TestEnsureRegistered_WebhookSkippedIfExists(t *testing.T) {
	f := newFakeForgejoServer()
	defer f.Close()

	// Pre-seed the repo with a webhook pointing to our URL
	state := &repoState{
		webhooks: []map[string]interface{}{
			{
				"id":     1,
				"type":   "forgejo",
				"active": true,
				"config": map[string]interface{}{"url": "https://fordjent.example.com/acp/v1/events"},
				"events": []string{"issues", "issue_comment"},
			},
		},
		labels: make(map[string]bool),
	}
	f.registry["fjadmin/existingrepo"] = state

	ar := NewAutoRegistrar(AutoRegistrarConfig{
		ForgejoClient: f.Client(),
		WebhookURL:    "https://fordjent.example.com/acp/v1/events",
		WebhookSecret: "my-secret",
	})

	ctx := context.Background()

	if err := ar.EnsureRegistered(ctx, "fjadmin/existingrepo"); err != nil {
		t.Fatalf("EnsureRegistered failed: %v", err)
	}

	// Webhook should NOT have been created
	if f.webhookCalls != 0 {
		t.Fatalf("expected 0 webhook creations (already exists), got %d", f.webhookCalls)
	}
}

func TestEnsureRegistered_LabelsCreated(t *testing.T) {
	f := newFakeForgejoServer()
	defer f.Close()

	ar := NewAutoRegistrar(AutoRegistrarConfig{
		ForgejoClient: f.Client(),
		WebhookURL:    "https://fordjent.example.com/acp/v1/events",
		WebhookSecret: "secret",
	})

	ctx := context.Background()

	if err := ar.EnsureRegistered(ctx, "fjadmin/testrepo"); err != nil {
		t.Fatalf("EnsureRegistered failed: %v", err)
	}

	// EnsureLabels should have created some labels (the fake starts empty)
	if f.labelCalls == 0 {
		t.Fatal("expected label creation calls, got 0")
	}
}

func TestEnsureRegistered_WebhooksOnlyIfURLConfigured(t *testing.T) {
	f := newFakeForgejoServer()
	defer f.Close()

	ar := NewAutoRegistrar(AutoRegistrarConfig{
		ForgejoClient: f.Client(),
		WebhookURL:    "", // empty — should skip webhook creation
		WebhookSecret: "secret",
	})

	ctx := context.Background()

	if err := ar.EnsureRegistered(ctx, "fjadmin/testrepo"); err != nil {
		t.Fatalf("EnsureRegistered failed: %v", err)
	}

	// No webhook should have been created
	if f.webhookCalls != 0 {
		t.Fatalf("expected 0 webhook calls when webhook_url is empty, got %d", f.webhookCalls)
	}

	// Labels should still be created
	if f.labelCalls == 0 {
		t.Fatal("expected label creation calls even without webhook, got 0")
	}
}

func TestEnsureRegistered_DifferentReposTrackedSeparately(t *testing.T) {
	f := newFakeForgejoServer()
	defer f.Close()

	ar := NewAutoRegistrar(AutoRegistrarConfig{
		ForgejoClient: f.Client(),
		WebhookURL:    "https://fordjent.example.com/acp/v1/events",
		WebhookSecret: "secret",
	})

	ctx := context.Background()

	// Register first repo
	if err := ar.EnsureRegistered(ctx, "fjadmin/repo1"); err != nil {
		t.Fatalf("EnsureRegistered repo1 failed: %v", err)
	}
	firstWebhookCalls := f.webhookCalls

	// Register second repo — should create new webhook
	if err := ar.EnsureRegistered(ctx, "fjadmin/repo2"); err != nil {
		t.Fatalf("EnsureRegistered repo2 failed: %v", err)
	}

	if f.webhookCalls != firstWebhookCalls+1 {
		t.Fatalf("expected webhook creation for second repo, total calls: %d (was %d)", f.webhookCalls, firstWebhookCalls)
	}
}

func TestEnsureRegistered_WebhookURLOnlyMatchesExactURL(t *testing.T) {
	f := newFakeForgejoServer()
	defer f.Close()

	// Pre-seed with a webhook that has a different URL
	state := &repoState{
		webhooks: []map[string]interface{}{
			{
				"id":     1,
				"type":   "forgejo",
				"active": true,
				"config": map[string]interface{}{"url": "https://other.example.com/acp/v1/events"},
				"events": []string{"issues"},
			},
		},
		labels: make(map[string]bool),
	}
	f.registry["fjadmin/testrepo"] = state

	ar := NewAutoRegistrar(AutoRegistrarConfig{
		ForgejoClient: f.Client(),
		WebhookURL:    "https://fordjent.example.com/acp/v1/events",
		WebhookSecret: "secret",
	})

	ctx := context.Background()

	if err := ar.EnsureRegistered(ctx, "fjadmin/testrepo"); err != nil {
		t.Fatalf("EnsureRegistered failed: %v", err)
	}

	// A new webhook should have been created since the URL doesn't match
	if f.webhookCalls != 1 {
		t.Fatalf("expected 1 webhook creation (existing URL doesn't match), got %d", f.webhookCalls)
	}
}