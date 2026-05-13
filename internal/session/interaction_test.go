package session

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

type interactionForgejo struct {
	srv           *httptest.Server
	issueTitle    string
	issueLabels   []string
	issueState    string
	isPR          bool
	prHeadRef     string
	prMerged      bool
	prState       string
	comments      []string
	addedLabels   []string
	removedLabels []string
	closedIssues  []int
	createdLabels []string
	addedLabelIDs []int64
	repoFiles     []string
	openIssues    []map[string]interface{}
	createdIssues []string
}

func newInteractionForgejo(t *testing.T) *interactionForgejo {
	f := &interactionForgejo{issueState: "open", prState: "open"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	return f
}

func (f *interactionForgejo) URL() string { return f.srv.URL }

func (f *interactionForgejo) Close() { f.srv.Close() }

func (f *interactionForgejo) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/git/trees/"):
		f.handleGitTrees(w, r)
	case r.Method == http.MethodGet && strings.Contains(path, "/pulls/") && !strings.Contains(path, "/files"):
		f.handleGetPR(w, r)
	case r.Method == http.MethodGet && strings.Contains(path, "/issues/") &&
		!strings.Contains(path, "/comments") && !strings.Contains(path, "/labels"):
		f.handleGetIssue(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/issues") && !strings.Contains(path, "/issues/"):
		f.handleListIssues(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/issues") && !strings.Contains(path, "/issues/"):
		f.handleCreateIssue(w, r)
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
	case r.Method == http.MethodPatch && strings.Contains(path, "/issues/"):
		f.handlePatchIssue(w, r)
	default:
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func (f *interactionForgejo) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	issueLabels := mergeLabels(f.issueLabels, f.addedLabels, nil)
	roleLabels := buildLabelObjects(issueLabels)
	resp := map[string]interface{}{
		"number": 42,
		"title":  f.issueTitle,
		"body":   "Test body",
		"state":  f.issueState,
		"labels": roleLabels,
	}
	if f.isPR {
		resp["is_pull_request"] = true
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *interactionForgejo) handleGetPR(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"number": 7,
		"title":  f.issueTitle,
		"state":  f.prState,
		"head":   map[string]interface{}{"ref": f.prHeadRef, "label": f.prHeadRef},
		"base":   map[string]interface{}{"ref": "main", "label": "main"},
		"merged": f.prMerged,
	})
}

func (f *interactionForgejo) handlePostComment(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.comments = append(f.comments, body["body"])
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 1})
}

func (f *interactionForgejo) handleListLabels(w http.ResponseWriter, r *http.Request) {
	allLbls := mergeLabels(f.issueLabels, f.addedLabels, f.createdLabels)
	labels := []map[string]interface{}{}
	id := int64(1)
	for _, l := range allLbls {
		labels = append(labels, map[string]interface{}{"id": id, "name": l})
		id++
	}
	_ = json.NewEncoder(w).Encode(labels)
}

func (f *interactionForgejo) handleCreateLabel(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.createdLabels = append(f.createdLabels, body["name"])
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": int64(len(f.createdLabels)), "name": body["name"]})
}

func (f *interactionForgejo) handleAddLabels(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Labels []int64 `json:"labels"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.Labels) > 0 {
		f.addedLabelIDs = append(f.addedLabelIDs, body.Labels...)
		allLabels := mergeLabels(f.issueLabels, f.addedLabels, f.createdLabels)
		for _, id := range body.Labels {
			idx := int(id) - 1
			if idx >= 0 && idx < len(allLabels) {
				f.addedLabels = append(f.addedLabels, allLabels[idx])
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode([]map[string]interface{}{{"id": 1, "name": "needs-role"}})
}

func (f *interactionForgejo) handleRemoveLabel(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	label := parts[len(parts)-1]
	f.removedLabels = append(f.removedLabels, label)
	w.WriteHeader(http.StatusNoContent)
}

func (f *interactionForgejo) handlePatchIssue(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if state, ok := body["state"].(string); ok && state == "closed" {
		f.closedIssues = append(f.closedIssues, 42)
	}
	f.issueState = "closed"
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"state": "closed"})
}

func (f *interactionForgejo) handleGitTrees(w http.ResponseWriter, r *http.Request) {
	tree := make([]map[string]interface{}, 0, len(f.repoFiles))
	for _, p := range f.repoFiles {
		tree = append(tree, map[string]interface{}{
			"path": p,
			"type": "blob",
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"tree": tree,
	})
}

func (f *interactionForgejo) handleListIssues(w http.ResponseWriter, r *http.Request) {
	if len(f.openIssues) > 0 {
		_ = json.NewEncoder(w).Encode(f.openIssues)
		return
	}
	_ = json.NewEncoder(w).Encode([]interface{}{})
}

func (f *interactionForgejo) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.createdIssues = append(f.createdIssues, body["title"])
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"number": float64(len(f.createdIssues) + 100),
		"title":  body["title"],
		"body":   body["body"],
		"state":  "open",
	})
}

func mergeLabels(base, added, created []string) []string {
	all := append([]string{}, base...)
	all = append(all, added...)
	all = append(all, created...)
	return all
}

func buildLabelObjects(names []string) []map[string]string {
	out := make([]map[string]string, 0, len(names))
	seen := make(map[string]bool)
	for _, n := range names {
		if !seen[n] {
			out = append(out, map[string]string{"name": n})
			seen[n] = true
		}
	}
	return out
}

func interactionTestConfig(t *testing.T, forgejoURL string) *config.Config {
	return &config.Config{
		Forgejo: config.ForgejoConfig{
			URL:   forgejoURL,
			Token: "test-token",
		},
		Agent: config.AgentConfig{
			MaxSessions:             10,
			WorkDir:                 t.TempDir(),
			IdleTimeout:             1 * time.Hour,
			RequireRoleTag:          false,
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

func TestFSMDoneAutoClosesIssue(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueLabels = []string{"done"}
	f.issueState = "open"

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}

	mgr.handleEvent(context.Background(), evt)

	if len(f.closedIssues) == 0 {
		t.Error("expected issue to be closed when 'done' label is applied")
	}
}

func TestFSMDoneAlreadyClosedNoDoubleClose(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueLabels = []string{"done"}
	f.issueState = "closed"

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}

	mgr.handleEvent(context.Background(), evt)

	if len(f.closedIssues) > 0 {
		t.Error("expected no CloseIssue call when issue is already closed")
	}
}

func TestFSMPlanningLabelDoesNotCloseIssue(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueLabels = []string{"planning"}
	f.issueState = "open"

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}

	mgr.handleEvent(context.Background(), evt)

	if len(f.closedIssues) > 0 {
		t.Error("expected no CloseIssue call for 'planning' label")
	}
}

func TestAutomergeLabelSpawnsReviewer(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueLabels = []string{"automerge"}
	f.issueState = "open"
	f.isPR = true
	f.prHeadRef = "feature/add-foo"
	f.prState = "open"

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.PullRequestLabelUpdated, "org/repo", 0, 7, "alice", "label_updated")
	evt.SessionKey = "org/repo/pulls/7"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{
			"number": float64(7),
			"labels": []interface{}{
				map[string]interface{}{"name": "automerge"},
			},
		},
	}

	mgr.handleEvent(context.Background(), evt)

	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/pulls/7"]
	mgr.mu.RUnlock()

	if !exists {
		t.Error("expected session to be created in pulls/7 when automerge label is applied")
	}
}

func TestAutomergeLabelNoSessionWithoutLabel(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueLabels = []string{"review"}
	f.issueState = "open"
	f.isPR = true
	f.prHeadRef = "feature/add-foo"
	f.prState = "open"

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.PullRequestLabelUpdated, "org/repo", 0, 7, "alice", "label_updated")
	evt.SessionKey = "org/repo/pulls/7"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{
			"number": float64(7),
			"labels": []interface{}{
				map[string]interface{}{"name": "review"},
			},
		},
	}

	mgr.handleEvent(context.Background(), evt)

	// PR label updates without automerge should NOT create sessions.
	// The automerge detection block returns after processing, preventing
	// fallthrough to getOrCreate.
	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/pulls/7"]
	mgr.mu.RUnlock()

	if exists {
		t.Error("PR label update without automerge should NOT create a session")
	}
}

func TestPRCommentRoutesToPullsSession(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueTitle = "Add new feature"
	f.issueState = "open"
	f.isPR = true

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 7, 7, "alice", "created")
	evt.SessionKey = "org/repo/pulls/7"
	evt.Payload = map[string]interface{}{
		"comment": map[string]interface{}{
			"body": "Please fix the error handling",
		},
		"issue": map[string]interface{}{
			"number":          float64(7),
			"is_pull_request": true,
		},
	}

	mgr.handleEvent(context.Background(), evt)

	mgr.mu.RLock()
	sess, exists := mgr.sessions["org/repo/pulls/7"]
	mgr.mu.RUnlock()

	if !exists {
		t.Fatal("expected session to be created at org/repo/pulls/7 for PR comment")
	}
	if sess.PRNumber != 7 {
		t.Errorf("expected PRNumber=7, got %d", sess.PRNumber)
	}
}

func TestIssueCommentRoutesToIssuesSession(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueTitle = "Fix login bug"
	f.issueState = "open"
	f.isPR = false

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 42, 0, "alice", "created")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"comment": map[string]interface{}{
			"body": "I think we should use a different approach",
		},
		"issue": map[string]interface{}{
			"number": float64(42),
		},
	}

	mgr.handleEvent(context.Background(), evt)

	mgr.mu.RLock()
	sess, exists := mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()

	if !exists {
		t.Fatal("expected session to be created at org/repo/issues/42 for issue comment")
	}
	if sess.IssueNumber != 42 {
		t.Errorf("expected IssueNumber=42, got %d", sess.IssueNumber)
	}
	if sess.PRNumber != 0 {
		t.Errorf("expected PRNumber=0 for issue comment, got %d", sess.PRNumber)
	}
}

func TestIssueLabelUpdatedFSMDetection(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueLabels = []string{"implementing", "role:implementer"}
	f.issueState = "open"

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}

	mgr.handleEvent(context.Background(), evt)

	if len(f.closedIssues) > 0 {
		t.Error("implementing label should not close the issue")
	}
}

func TestRoleGateThenFSMStateTransition(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	cfg := interactionTestConfig(t, f.URL())
	cfg.Agent.RequireRoleTag = true
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	// Step 1: Open issue without role → blocked by role gate
	evtOpen := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evtOpen.SessionKey = "org/repo/issues/42"
	mgr.handleEvent(context.Background(), evtOpen)

	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()
	if exists {
		t.Fatal("expected no session for untagged issue")
	}

	// Step 2: Add role:implementer + needs-role labels → session created
	f.issueLabels = []string{"needs-role", "role:implementer"}
	evtLabel := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evtLabel.SessionKey = "org/repo/issues/42"
	mgr.handleEvent(context.Background(), evtLabel)

	mgr.mu.RLock()
	_, exists = mgr.sessions["org/repo/issues/42"]
	mgr.mu.RUnlock()
	if !exists {
		t.Error("expected session after role label added")
	}

	// Step 3: Add "done" label → issue should be auto-closed
	// FSM detection now runs BEFORE handleRoleAssignment, so done→close
	// works regardless of RequireRoleTag.
	f.issueLabels = []string{"role:implementer", "done"}
	evtDone := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evtDone.SessionKey = "org/repo/issues/42"
	evtDone.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}
	mgr.handleEvent(context.Background(), evtDone)

	if len(f.closedIssues) == 0 {
		t.Error("expected issue to be auto-closed when 'done' label is applied even with RequireRoleTag=true")
	}
}

func TestFSMBlockedLabelDoesNotPreventSession(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueLabels = []string{"blocked"}
	f.issueState = "open"

	cfg := interactionTestConfig(t, f.URL())
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
		t.Error("blocked FSM label should NOT prevent session creation (only affects prompt)")
	}
}

func TestFSMDoneCloseWithoutRoleGate(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueLabels = []string{"done", "implementing"}
	f.issueState = "open"

	cfg := interactionTestConfig(t, f.URL())
	cfg.Agent.RequireRoleTag = false
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}

	mgr.handleEvent(context.Background(), evt)

	if len(f.closedIssues) == 0 {
		t.Error("expected issue to be auto-closed when 'done' label is applied and RequireRoleTag=false")
	}
}

func TestFSMInvalidTransitionBlocked(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	// First set issue to "done" state
	f.issueLabels = []string{"done"}
	f.issueState = "closed"

	cfg := interactionTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	// Record the "done" state
	evtDone := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evtDone.SessionKey = "org/repo/issues/42"
	evtDone.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}
	mgr.handleEvent(context.Background(), evtDone)

	// Now try invalid transition: done → planning (should be blocked)
	f.issueLabels = []string{"planning"}
	f.issueState = "closed"
	closedBefore := len(f.closedIssues)

	evtPlanning := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evtPlanning.SessionKey = "org/repo/issues/42"
	evtPlanning.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}
	mgr.handleEvent(context.Background(), evtPlanning)

	if len(f.closedIssues) != closedBefore {
		t.Error("invalid FSM transition (done→planning) should not trigger any actions like auto-close")
	}
}

func TestRoleAssignment_ForgejoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := interactionTestConfig(t, srv.URL)
	cfg.Agent.RequireRoleTag = true
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}

	mgr.handleEvent(context.Background(), evt)
}

func TestScaffoldDetection_BlocksOnEmptyRepo(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.repoFiles = []string{}
	f.issueLabels = []string{"role:implementer"}
	f.issueTitle = "Add feature X"

	cfg := interactionTestConfig(t, f.URL())
	cfg.Agent.EnableScaffoldDetection = true
	cfg.Agent.RequireRoleTag = true
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42), "title": "Add feature X", "body": "Do the thing"},
	}

	mgr.handleEvent(context.Background(), evt)

	if len(f.createdIssues) == 0 {
		t.Error("expected scaffold issue to be created on empty repo")
	}
}

func TestScaffoldDetection_PassesOnPopulatedRepo(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.repoFiles = []string{"go.mod", "README.md", "main.go"}
	f.issueLabels = []string{"role:implementer"}
	f.issueTitle = "Add feature X"

	cfg := interactionTestConfig(t, f.URL())
	cfg.Agent.EnableScaffoldDetection = true
	cfg.Agent.RequireRoleTag = true
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/42"
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42), "title": "Add feature X", "body": "Do the thing"},
	}

	mgr.handleEvent(context.Background(), evt)

	if len(f.createdIssues) > 0 {
		t.Error("scaffold issue should NOT be created on populated repo")
	}
}

func TestLabelUpdatedDoesNotCreateSession(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.issueTitle = "[implementer] Add a feature"
	f.issueLabels = []string{"blocked"}
	f.repoFiles = []string{"go.mod", "main.go"}

	cfg := testConfig(t, f.URL(), true)
	cfg.Agent.EnableScaffoldDetection = false

	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go mgr.Run(ctx)
	defer cancel()

	evt := event.NewEvent(event.IssueLabelUpdated, "fjadmin/testbed", 1, 0, "fjadmin", "label_updated")
	evt.SessionKey = "fjadmin/testbed/issues/1"

	mgr.handleEvent(ctx, evt)

	_, exists := mgr.sessions["fjadmin/testbed/issues/1"]
	if exists {
		t.Error("label_updated events should NOT create sessions (only FSM state tracking)")
	}
}
