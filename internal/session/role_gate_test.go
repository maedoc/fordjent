package session

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

func testConfig(t *testing.T, forgejoURL string, requireRoleTag bool) *config.Config {
	return &config.Config{
		Forgejo: config.ForgejoConfig{
			URL:   forgejoURL,
			Token: "test-token",
		},
		Agent: config.AgentConfig{
			MaxSessions:             10,
			WorkDir:                 t.TempDir(),
			IdleTimeout:             1 * time.Hour,
			RequireRoleTag:          requireRoleTag,
			EnableScaffoldDetection: false,
			SessionTimeout:          60 * time.Minute,
			MaxTurns:                5,
		},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "http://localhost:8080/v1", APIKey: "test", Model: "test", MaxTokens: 4096},
		},
		Webhook:            config.WebhookConfig{Secret: "test-secret"},
		Events:             []string{"issues"},
		SessionKeyTemplate: "{{.Repository}}/issues/{{.IssueNumber}}",
		Database:           config.DatabaseConfig{Path: ""},
		Memory:             config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security:           config.SecurityConfig{FilterAgentEvents: false},
	}
}

type fakeForgejo struct {
	issueTitle    string
	issueLabels   []string
	comments      []string
	addedLabels   []string
	removedLabels []string
	srv           *httptest.Server
}

func (f *fakeForgejo) URL() string { return f.srv.URL }

func newFakeForgejo(t *testing.T, title string, labels []string) *fakeForgejo {
	f := &fakeForgejo{issueTitle: title, issueLabels: labels}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	return f
}

func (f *fakeForgejo) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/issues/") &&
		!strings.Contains(path, "/comments") && !strings.Contains(path, "/labels"):
		f.handleGetIssue(w, r)
	case r.Method == http.MethodPost && strings.Contains(path, "/comments"):
		f.handlePostComment(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/labels"):
		f.handleListLabels(w, r)
	case r.Method == http.MethodPost && !strings.Contains(path, "/issues/") && strings.HasSuffix(path, "/labels"):
		f.handleCreateLabel(w, r)
	case r.Method == http.MethodPost && strings.Contains(path, "/issues/") && strings.Contains(path, "/labels"):
		f.handleAddLabels(w, r)
	case r.Method == http.MethodDelete && strings.Contains(path, "/labels/"):
		f.handleRemoveLabel(w, r)
	default:
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func (f *fakeForgejo) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	labels := make([]map[string]string, 0, len(f.issueLabels))
	for _, l := range f.issueLabels {
		labels = append(labels, map[string]string{"name": l})
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"number": 42,
		"title":  f.issueTitle,
		"body":   "Test body",
		"state":  "open",
		"labels": labels,
	})
}

func (f *fakeForgejo) handlePostComment(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.comments = append(f.comments, body["body"])
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 1})
}

func (f *fakeForgejo) handleListLabels(w http.ResponseWriter, r *http.Request) {
	labels := []map[string]interface{}{}
	for _, l := range append(f.issueLabels, "needs-role") {
		labels = append(labels, map[string]interface{}{"id": 1, "name": l})
	}
	_ = json.NewEncoder(w).Encode(labels)
}

func (f *fakeForgejo) handleCreateLabel(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 1, "name": body["name"]})
}

func (f *fakeForgejo) handleAddLabels(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(raw, &bodyMap); err == nil {
		if lbls, ok := bodyMap["labels"].([]interface{}); ok {
			for range lbls {
				f.addedLabels = append(f.addedLabels, "needs-role")
			}
		}
	} else {
		var names []string
		if json.Unmarshal(raw, &names) == nil && len(names) > 0 {
			f.addedLabels = append(f.addedLabels, names...)
		}
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "needs-role"}})
}

func (f *fakeForgejo) handleRemoveLabel(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	label := parts[len(parts)-1]
	label, _ = url.PathUnescape(label)
	f.removedLabels = append(f.removedLabels, label)
	w.WriteHeader(http.StatusNoContent)
}

func TestRoleGateBlocked(t *testing.T) {
	f := newFakeForgejo(t, "Fix login bug", nil)
	defer f.srv.Close()

	cfg := testConfig(t, f.URL(), true)
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/42"

	mgr.handleEvent(context.Background(), evt)

	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()

	if exists {
		t.Error("expected session to NOT be created for untagged issue")
	}
	if len(f.comments) == 0 {
		t.Error("expected guidance comment to be posted")
	}
	if len(f.addedLabels) == 0 {
		t.Error("expected needs-role label to be added")
	}
}

func TestRoleGateAllowed(t *testing.T) {
	f := newFakeForgejo(t, "[pm] Plan auth refactor", nil)
	defer f.srv.Close()

	cfg := testConfig(t, f.URL(), true)
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/42"

	mgr.handleEvent(context.Background(), evt)

	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()

	if !exists {
		t.Error("expected session to be created for [pm] tagged issue")
	}
}

func TestRoleGateLabelAllowed(t *testing.T) {
	f := newFakeForgejo(t, "Review auth refactor", []string{"role:reviewer"})
	defer f.srv.Close()

	cfg := testConfig(t, f.URL(), true)
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/42"

	mgr.handleEvent(context.Background(), evt)

	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()

	if !exists {
		t.Error("expected session to be created for issue with role label")
	}
}

func TestRoleAssignmentViaLabel(t *testing.T) {
	f := newFakeForgejo(t, "Fix login bug", nil)
	defer f.srv.Close()

	cfg := testConfig(t, f.URL(), true)
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	// First: open plain issue → blocked
	evtOpen := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evtOpen.SessionKey = "org/repo/issues/42"
	mgr.handleEvent(context.Background(), evtOpen)

	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()
	if exists {
		t.Fatal("expected session to be blocked initially")
	}

	// Now simulate label update adding role:implementer and needs-role
	f.issueLabels = []string{"needs-role", "role:implementer"}

	evtLabel := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evtLabel.SessionKey = "org/repo/issues/42"
	mgr.handleEvent(context.Background(), evtLabel)

	mgr.mu.RLock()
	_, exists = mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()
	if !exists {
		t.Error("expected session to be created after role label was added")
	}

	if len(f.removedLabels) == 0 {
		t.Error("expected needs-role label to be removed after role assignment")
	}
}

func TestRoleAssignmentViaTitle(t *testing.T) {
	f := newFakeForgejo(t, "Fix login bug", nil)
	defer f.srv.Close()

	cfg := testConfig(t, f.URL(), true)
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	// First: open plain issue → blocked
	evtOpen := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evtOpen.SessionKey = "org/repo/issues/42"
	mgr.handleEvent(context.Background(), evtOpen)

	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()
	if exists {
		t.Fatal("expected session to be blocked initially")
	}

	// Now simulate title edit to include [devops]
	f.issueTitle = "[devops] Set up CI"

	evtEdit := event.NewEvent(event.IssueEdited, "org/repo", 42, 0, "alice", "edited")
	evtEdit.SessionKey = "org/repo/issues/42"
	mgr.handleEvent(context.Background(), evtEdit)

	mgr.mu.RLock()
	_, exists = mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()
	if !exists {
		t.Error("expected session to be created after title was edited with role tag")
	}

	if len(f.removedLabels) == 0 {
		t.Error("expected needs-role label to be removed after role assignment")
	}
}

func TestRoleGateDisabled(t *testing.T) {
	f := newFakeForgejo(t, "Fix login bug", nil)
	defer f.srv.Close()

	cfg := testConfig(t, f.URL(), false)
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/42"

	mgr.handleEvent(context.Background(), evt)

	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()

	if !exists {
		t.Error("expected session to be created when RequireRoleTag is false")
	}
}
