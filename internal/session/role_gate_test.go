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
	addedLabelIDs []int64
	removedLabels []string
	createdLabels []string
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
	roleLabels := make([]map[string]string, 0, len(f.issueLabels)+len(f.addedLabels))
	seen := make(map[string]bool)
	for _, l := range f.issueLabels {
		roleLabels = append(roleLabels, map[string]string{"name": l})
		seen[l] = true
	}
	for _, l := range f.addedLabels {
		if !seen[l] {
			roleLabels = append(roleLabels, map[string]string{"name": l})
			seen[l] = true
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"number": 42,
		"title":  f.issueTitle,
		"body":   "Test body",
		"state":  "open",
		"labels": roleLabels,
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
	// Combine all label sources: initial, added via API, created via API
	allLbls := append([]string{}, f.issueLabels...)
	allLbls = append(allLbls, f.addedLabels...)
	allLbls = append(allLbls, f.createdLabels...)
	labels := []map[string]interface{}{}
	id := int64(1)
	for _, l := range allLbls {
		labels = append(labels, map[string]interface{}{"id": id, "name": l})
		id++
	}
	_ = json.NewEncoder(w).Encode(labels)
}

func (f *fakeForgejo) handleCreateLabel(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.createdLabels = append(f.createdLabels, body["name"])
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": int64(len(f.createdLabels)), "name": body["name"]})
}

func (f *fakeForgejo) handleAddLabels(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	// Forgejo sends {"labels": [id1, id2, ...]} — label IDs resolved from prior ListLabels call.
	var body struct {
		Labels []int64 `json:"labels"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.Labels) > 0 {
		f.addedLabelIDs = append(f.addedLabelIDs, body.Labels...)
		// For test compatibility, also record as named labels.
		// In real Forgejo, these IDs map to the labels returned by ListLabels.
		for _, id := range body.Labels {
			name := f.labelNameForID(id)
			if name != "" {
				f.addedLabels = append(f.addedLabels, name)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode([]map[string]interface{}{{"id": 1, "name": "needs-role"}})
}

func (f *fakeForgejo) handleRemoveLabel(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	label := parts[len(parts)-1]
	label, _ = url.PathUnescape(label)
	f.removedLabels = append(f.removedLabels, label)
	w.WriteHeader(http.StatusNoContent)
}


// labelNamesFromIDs maps label IDs back to names using the registered labels.
// This simulates Forgejo's label resolution: AddIssueLabels first calls
// ListLabels to get name→id, then sends IDs. We reverse-lookup here.
func (f *fakeForgejo) labelNamesFromIDs(ids []int64) []string {
	var names []string
	for _, id := range ids {
		if name := f.labelNameForID(id); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// labelNameForID returns the label name for a given ID.
// The fake assigns sequential IDs starting at 1 for all labels.
// Labels are stored in this order: issueLabels first, then addedLabels.
func (f *fakeForgejo) labelNameForID(id int64) string {
	allLabels := append([]string{}, f.issueLabels...)
	allLabels = append(allLabels, f.addedLabels...)
	allLabels = append(allLabels, f.createdLabels...)
	idx := int(id) - 1
	if idx >= 0 && idx < len(allLabels) {
		return allLabels[idx]
	}
	return "needs-role" // fallback for simple test cases
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
