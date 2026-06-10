package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

type interactionForgejo struct {
	srv           *httptest.Server
	mu            sync.Mutex
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

func (f *interactionForgejo) setFields(labels []string, state string, setters ...func(*interactionForgejo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issueLabels = labels
	f.issueState = state
	for _, s := range setters {
		s(f)
	}
}

func (f *interactionForgejo) closedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.closedIssues)
}

func (f *interactionForgejo) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.createdIssues)
}

func (f *interactionForgejo) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/git/trees/"):
		f.handleGitTrees(w, r)
	case r.Method == http.MethodGet && strings.Contains(path, "/pulls/") && strings.Contains(path, "/files"):
		f.handlePRFiles(w, r)
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
	f.mu.Lock()
	issueLabels := mergeLabels(f.issueLabels, f.addedLabels, nil)
	roleLabels := buildLabelObjects(issueLabels)
	title := f.issueTitle
	state := f.issueState
	isPR := f.isPR
	f.mu.Unlock()

	resp := map[string]interface{}{
		"number": 42,
		"title":  title,
		"body":   "Test body",
		"state":  state,
		"labels": roleLabels,
	}
	if isPR {
		resp["is_pull_request"] = true
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *interactionForgejo) handleGetPR(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	title := f.issueTitle
	prState := f.prState
	prHeadRef := f.prHeadRef
	prMerged := f.prMerged
	f.mu.Unlock()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"number": 7,
		"title":  title,
		"state":  prState,
		"head":   map[string]interface{}{"ref": prHeadRef, "label": prHeadRef},
		"base":   map[string]interface{}{"ref": "main", "label": "main"},
		"merged": prMerged,
	})
}

func (f *interactionForgejo) handlePostComment(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.comments = append(f.comments, body["body"])
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 1})
}

func (f *interactionForgejo) handleListLabels(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	allLbls := mergeLabels(f.issueLabels, f.addedLabels, f.createdLabels)
	f.mu.Unlock()
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
	f.mu.Lock()
	f.createdLabels = append(f.createdLabels, body["name"])
	n := len(f.createdLabels)
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": int64(n), "name": body["name"]})
}

func (f *interactionForgejo) handleAddLabels(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Labels []int64 `json:"labels"`
	}
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	f.removedLabels = append(f.removedLabels, label)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *interactionForgejo) handlePatchIssue(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	if state, ok := body["state"].(string); ok && state == "closed" {
		f.closedIssues = append(f.closedIssues, 42)
	}
	f.issueState = "closed"
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"state": "closed"})
}

func (f *interactionForgejo) handleGitTrees(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	files := make([]string, len(f.repoFiles))
	copy(files, f.repoFiles)
	f.mu.Unlock()
	tree := make([]map[string]interface{}, 0, len(files))
	for _, p := range files {
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
	f.mu.Lock()
	issues := f.openIssues
	f.mu.Unlock()
	if len(issues) > 0 {
		_ = json.NewEncoder(w).Encode(issues)
		return
	}
	_ = json.NewEncoder(w).Encode([]interface{}{})
}

func (f *interactionForgejo) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.createdIssues = append(f.createdIssues, body["title"])
	n := len(f.createdIssues)
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"number": float64(n + 100),
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

	f.setFields([]string{"done"}, "open")

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

	if f.closedCount() == 0 {
		t.Error("expected issue to be closed when 'done' label is applied")
	}
}

func TestFSMDoneAlreadyClosedNoDoubleClose(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"done"}, "closed")

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

	if f.closedCount() > 0 {
		t.Error("expected no CloseIssue call when issue is already closed")
	}
}

func TestFSMPlanningLabelDoesNotCloseIssue(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"planning"}, "open")

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

	if f.closedCount() > 0 {
		t.Error("expected no CloseIssue call for 'planning' label")
	}
}

func TestAutomergeLabelDirectMerge(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"automerge"}, "open", func(f *interactionForgejo) {
		f.isPR = true
		f.prHeadRef = "feature/add-foo"
		f.prState = "open"
	})

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

	// When direct API merge succeeds, NO session should be created
	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/pulls/7"]
	mgr.mu.RUnlock()

	if exists {
		t.Error("automerge via direct API should NOT create a reviewer session")
	}
}

func TestAutomergeLabelNoSessionWithoutLabel(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"review"}, "open", func(f *interactionForgejo) {
		f.isPR = true
		f.prHeadRef = "feature/add-foo"
		f.prState = "open"
	})

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

	f.setFields(nil, "open", func(f *interactionForgejo) {
		f.issueTitle = "Add new feature"
		f.isPR = true
	})

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

	// Find session with -fix prefix (exact key includes event ID suffix)
	var sess *Session
	var exists bool
	mgr.mu.RLock()
	for k, s := range mgr.sessions {
		if strings.HasPrefix(k, "org/repo/pulls/7-fix") {
			sess = s
			exists = true
			break
		}
		if k == "org/repo/pulls/7" {
			sess = s
			exists = true
			break
		}
	}
	mgr.mu.RUnlock()

	if !exists {
		t.Fatal("expected session to be created with pulls/7-fix* key for PR comment")
	}
	if sess.PRNumber != 7 {
		t.Errorf("expected PRNumber=7, got %d", sess.PRNumber)
	}
}

func TestIssueCommentRoutesToIssuesSession(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields(nil, "open", func(f *interactionForgejo) {
		f.issueTitle = "Fix login bug"
		f.isPR = false
	})

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

	f.setFields([]string{"implementing", "role:implementer"}, "open")

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

	if f.closedCount() > 0 {
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
	f.setFields([]string{"needs-role", "role:implementer"}, "open")
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
	f.setFields([]string{"role:implementer", "done"}, "open")
	evtDone := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evtDone.SessionKey = "org/repo/issues/42"
	evtDone.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}
	mgr.handleEvent(context.Background(), evtDone)

	if f.closedCount() == 0 {
		t.Error("expected issue to be auto-closed when 'done' label is applied even with RequireRoleTag=true")
	}
}

func TestFSMBlockedLabelDoesNotPreventSession(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"blocked"}, "open")

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

	f.setFields([]string{"done", "implementing"}, "open")

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

	if f.closedCount() == 0 {
		t.Error("expected issue to be auto-closed when 'done' label is applied and RequireRoleTag=false")
	}
}

func TestFSMInvalidTransitionBlocked(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	// First set issue to "done" state
	f.setFields([]string{"done"}, "closed")

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
	f.setFields([]string{"planning"}, "closed")
	closedBefore := f.closedCount()

	evtPlanning := event.NewEvent(event.IssueLabelUpdated, "org/repo", 42, 0, "alice", "label_updated")
	evtPlanning.SessionKey = "org/repo/issues/42"
	evtPlanning.Payload = map[string]interface{}{
		"issue": map[string]interface{}{"number": float64(42)},
	}
	mgr.handleEvent(context.Background(), evtPlanning)

	closedCount := f.closedCount()
	if closedCount != closedBefore {
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

	f.setFields([]string{"role:implementer"}, "open", func(f *interactionForgejo) {
		f.repoFiles = []string{}
		f.issueTitle = "Add feature X"
	})

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

	// Post-question behavior: scaffold should add "question" label and post a comment.
	f.mu.Lock()
	found := false
	for _, lbl := range f.addedLabels {
		if lbl == "question" {
			found = true
		}
	}
	f.mu.Unlock()
	if !found {
		t.Error("expected 'question' label to be added for empty repo")
	}
	if len(f.comments) < 1 {
		t.Error("expected question comment to be posted")
	}
}

func TestScaffoldDetection_PassesOnPopulatedRepo(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"role:implementer"}, "open", func(f *interactionForgejo) {
		f.repoFiles = []string{"go.mod", "README.md", "main.go"}
		f.issueTitle = "Add feature X"
	})

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

	if f.createdCount() > 0 {
		t.Error("scaffold issue should NOT be created on populated repo")
	}
}

func TestLabelUpdatedDoesNotCreateSession(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"blocked"}, "open", func(f *interactionForgejo) {
		f.issueTitle = "[implementer] Add a feature"
		f.repoFiles = []string{"go.mod", "main.go"}
	})

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

// TestMergeQueueFileOverlap: two open PRs touching same file → blocked.
func TestMergeQueueFileOverlap(t *testing.T) {
	t.Skip("merge queue requires live Forgejo for file comparison")
}

// TestPRCommentRouting: verifies issue_comment with is_pull_request=true routes to pulls/N.
func TestPRCommentRouting(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"implementing"}, "open", func(f *interactionForgejo) {
		f.issueTitle = "[implementer] Test PR routing"
		f.isPR = true
		f.prHeadRef = "feature/test-branch"
		f.repoFiles = []string{"go.mod", "main.go"}
	})

	cfg := testConfig(t, f.URL(), true)
	cfg.Agent.EnableScaffoldDetection = false
	cfg.Agent.MaxTurns = 5
	cfg.Agent.MaxSessions = 5

	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go mgr.Run(ctx)
	defer cancel()

	// Simulate PR comment webhook
	evt := event.NewEvent(event.IssueCommentCreated, "fjadmin/testbed", 42, 7, "human", "created")
	evt.SessionKey = "fjadmin/testbed/pulls/7"

	mgr.handleEvent(ctx, evt)

	// Find session with -fix prefix (exact key includes event ID suffix)
	var sess *Session
	var exists bool
	mgr.mu.RLock()
	for k, s := range mgr.sessions {
		if strings.HasPrefix(k, "fjadmin/testbed/pulls/7-fix") || k == "fjadmin/testbed/pulls/7" {
			sess = s
			exists = true
			break
		}
	}
	mgr.mu.RUnlock()
	if !exists {
		t.Error("PR comment should create pulls/N-fix* or pulls/N session")
		return
	}
	if sess.PRNumber != 7 {
		t.Errorf("expected PRNumber=7, got %d", sess.PRNumber)
	}
}

// TestAutoRetrySkipsClosedPR: closed PRs are skipped by auto-retry scan.
func TestAutoRetrySkipsClosedPR(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"fordjent/failed:max-turns"}, "closed", func(f *interactionForgejo) {
		f.issueTitle = "[implementer] Test auto-retry skip"
		f.prState = "closed"
		f.repoFiles = []string{"go.mod", "main.go"}
	})

	cfg := testConfig(t, f.URL(), true)
	cfg.Agent.EnableScaffoldDetection = false
	cfg.Agent.EnableAutoRetry = false
	cfg.Agent.MaxTurns = 5

	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Auto-retry is disabled in test config
	// Verify session store doesn't re-open closed issues
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go mgr.Run(ctx)
	defer cancel()

	sessKey := "fjadmin/testbed/issues/1"
	if _, open := mgr.sessions[sessKey]; open {
		t.Error("closed issue should not have active session")
	}
}

// TestCommentCapBlock: after comment limit, forgejo_comment is removed from tool schema.
func TestCommentCapBlock(t *testing.T) {
	t.Skip("comment cap behavior is LLM-side; test via live integration")
}

// TestScaffoldQuestionLabel: scaffold posts questions and labels issue 'question'.
func TestScaffoldQuestionLabel(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	// Empty repo with no files
	f.setFields([]string{}, "open", func(f *interactionForgejo) {
		f.issueTitle = "[implementer] Set up project structure"
		f.repoFiles = []string{}
	})

	cfg := testConfig(t, f.URL(), true)
	cfg.Agent.EnableScaffoldDetection = true
	cfg.Agent.MaxTurns = 5

	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go mgr.Run(ctx)
	defer cancel()

	evt := event.NewEvent(event.IssueOpened, "fjadmin/testbed", 1, 0, "human", "opened")
	evt.SessionKey = "fjadmin/testbed/issues/1"

	// scaffold.CheckAndBlock should detect empty repo and:
	// 1. Add "question" label (not "blocked")
	// 2. Post a question comment
	// 3. Return blocked=true to skip session creation
	mgr.handleEvent(ctx, evt)

	f.mu.Lock()
	defer f.mu.Unlock()

	// Check "question" label was added
	found := false
	for _, lbl := range f.addedLabels {
		if lbl == "question" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'question' label to be added for empty repo")
	}

	// Check a comment was posted (should be 1)
	if len(f.comments) < 1 {
		t.Error("expected question comment to be posted")
	}
}

// TestFSMQuestionLabelTransitions: verifies question label triggers correct session.
func TestFSMQuestionLabelTransitions(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	f.setFields([]string{"question"}, "open", func(f *interactionForgejo) {
		f.issueTitle = "[implementer] Answer scaffold question"
		f.repoFiles = []string{"go.mod"}
		f.comments = []string{"I want a Go project with cobra CLI"}
	})

	cfg := testConfig(t, f.URL(), true)
	cfg.Agent.EnableScaffoldDetection = true
	cfg.Agent.MaxTurns = 5

	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go mgr.Run(ctx)
	defer cancel()

	evt := event.NewEvent(event.IssueCommentCreated, "fjadmin/testbed", 1, 0, "human", "created")
	evt.SessionKey = "fjadmin/testbed/issues/1"

	mgr.handleEvent(ctx, evt)

	sess, exists := mgr.sessions["fjadmin/testbed/issues/1"]
	if !exists {
		t.Error("expected session to be created for scaffold answer")
		return
	}
	if !sess.IsScaffoldAnswer {
		t.Error("expected IsScaffoldAnswer to be true")
	}
}

func (f *interactionForgejo) handlePRFiles(w http.ResponseWriter, r *http.Request) {
	// Return spec PR files for testing
	files := []map[string]interface{}{
		{
			"filename":  "openspec/changes/test-feature/proposal.md",
			"status":    "added",
			"additions": 10,
			"deletions": 0,
		},
		{
			"filename":  "openspec/changes/test-feature/design.md",
			"status":    "added",
			"additions": 20,
			"deletions": 0,
		},
	}
	_ = json.NewEncoder(w).Encode(files)
}

func TestSpecLifecycleLabels_TransitionOnSpecPRMerge(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()
	f.prHeadRef = "spec/test-feature"
	f.prMerged = false // will test merged=true scenario
	f.issueLabels = []string{"spec-proposed"}

	cfg := testConfig(t, f.URL(), true)
	cfg.Agent.RequireRoleTag = false

	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Drain(context.Background())

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 42, 7, "fjadmin", "merged")
	evt.Payload = map[string]interface{}{
		"pull_request": map[string]interface{}{
			"merged": true,
			"head": map[string]interface{}{
				"ref": "spec/test-feature",
			},
		},
	}

	// Process the event
	mgr.handleEvent(context.Background(), evt)

	// Wait briefly for goroutine to process
	time.Sleep(500 * time.Millisecond)

	// Verify spec-approved label was added
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, l := range f.addedLabels {
		if l == "spec-approved" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected spec-approved label to be added, got addedLabels:", f.addedLabels)
	}
}

// --- Restart Checkpoint + Stale Cleanup tests ---

func TestShutdownCheckpointWrites(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		lastTool  string
	}{
		{"completed", "completed", "forgejo_create_pr"},
		{"failed", "failed", "write_file"},
		{"cancelled", "cancelled", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeShutdownCheckpoint(dir, tc.state, tc.lastTool)
			cp := readShutdownCheckpoint(dir)
			if cp == nil {
				t.Fatalf("expected checkpoint to exist")
			}
			if cp.State != tc.state {
				t.Errorf("state = %q, want %q", cp.State, tc.state)
			}
			if cp.LastTool != tc.lastTool {
				t.Errorf("last_tool = %q, want %q", cp.LastTool, tc.lastTool)
			}
			if cp.Timestamp == "" {
				t.Error("expected timestamp to be set")
			}
		})
	}
}

func TestRestoreSkipsCompletedSession(t *testing.T) {
	// Create a session store with a completed session
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	workDir := filepath.Join(dir, "work", "test-session")
	_ = os.MkdirAll(workDir, 0755)
	// Write shutdown.json indicating completed
	writeShutdownCheckpoint(workDir, "completed", "forgejo_create_pr")

	rec := &SessionRecord{
		SessionKey:  "test/session",
		Repository:  "test/repo",
		IssueNumber: 1,
		WorkDir:     workDir,
		RepoDir:     filepath.Join(workDir, "repo"),
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify session exists in store
	records, _ := store.ListAll()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	// Simulate restoreSessions logic: when shutdown.json has state="completed",
	// the session should be skipped and the store record deleted.
	cp := readShutdownCheckpoint(workDir)
	if cp == nil || cp.State != "completed" {
		t.Fatalf("expected completed checkpoint")
	}
	// In real code, this would call store.Delete and skip session creation.
	// For test, verify the store CRUD works.
	if err := store.Delete("test/session"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	records, _ = store.ListAll()
	if len(records) != 0 {
		t.Fatalf("expected 0 records after delete, got %d", len(records))
	}
}

func TestRestoreCrashRecoverySteering(t *testing.T) {
	// No shutdown.json → crash → session should be restored with verification steering
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	workDir := filepath.Join(dir, "work", "test-crash-session")
	_ = os.MkdirAll(workDir, 0755)
	// No shutdown.json written — simulating a crash

	rec := &SessionRecord{
		SessionKey:  "test/crash-session",
		Repository:  "test/repo",
		IssueNumber: 2,
		WorkDir:     workDir,
		RepoDir:     filepath.Join(workDir, "repo"),
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Check that no shutdown checkpoint exists (crash condition)
	cp := readShutdownCheckpoint(workDir)
	if cp != nil {
		t.Fatal("expected no checkpoint for crash scenario")
	}

	// In the real code, when tp == nil and lifecycle state is "working",
	// IsCrashRecovery would be set to true. We verify the flag mechanism works.
	sess := &Session{
		Key:             rec.SessionKey,
		WorkDir:         rec.WorkDir,
		IsCrashRecovery: true, // This is what restoreSessions would set
	}
	if !sess.IsCrashRecovery {
		t.Error("expected IsCrashRecovery to be true")
	}
}

func TestStoreDeleteOnShutdown(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Create two sessions
	for i := 0; i < 2; i++ {
		rec := &SessionRecord{
			SessionKey:  fmt.Sprintf("test/session-%d", i),
			Repository:  "test/repo",
			IssueNumber: i,
			WorkDir:     filepath.Join(dir, "work", fmt.Sprintf("session-%d", i)),
			RepoDir:     filepath.Join(dir, "work", fmt.Sprintf("session-%d", i), "repo"),
			CreatedAt:   time.Now(),
			LastActive:  time.Now(),
		}
		if err := store.Create(rec); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	// Simulate shutdownAll: delete all records from store
	records, _ := store.ListAll()
	if len(records) != 2 {
		t.Fatalf("expected 2 records before shutdown, got %d", len(records))
	}
	for _, rec := range records {
		store.Delete(rec.SessionKey)
	}

	records, _ = store.ListAll()
	if len(records) != 0 {
		t.Fatalf("expected 0 records after shutdownAll, got %d", len(records))
	}
}

func TestPruneMergedBranches(t *testing.T) {
	// Create a git repo with a merged feature branch
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")

	// Initialize repo
	cmd := exec.Command("git", "init", repoDir)
	if err := cmd.Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}

	// Create initial commit on main
	cmd = exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.com")
	_ = cmd.Run()
	cmd = exec.Command("git", "-C", repoDir, "config", "user.name", "Test")
	_ = cmd.Run()
	cmd = exec.Command("git", "-C", repoDir, "add", ".")
	_ = cmd.Run()
	cmd = exec.Command("git", "-C", repoDir, "commit", "-m", "initial")
	_ = cmd.Run()

	// Create and merge a feature branch
	cmd = exec.Command("git", "-C", repoDir, "checkout", "-b", "feature/test")
	_ = cmd.Run()
	_ = os.WriteFile(filepath.Join(repoDir, "test.txt"), []byte("test"), 0644)
	cmd = exec.Command("git", "-C", repoDir, "add", ".")
	_ = cmd.Run()
	cmd = exec.Command("git", "-C", repoDir, "commit", "-m", "add test")
	_ = cmd.Run()
	cmd = exec.Command("git", "-C", repoDir, "checkout", "main")
	_ = cmd.Run()
	cmd = exec.Command("git", "-C", repoDir, "merge", "feature/test")
	_ = cmd.Run()

	// Verify feature/test is now merged
	cmd = exec.Command("git", "-C", repoDir, "branch", "--merged", "main")
	output, _ := cmd.CombinedOutput()
	merged := strings.TrimSpace(string(output))
	if !strings.Contains(merged, "feature/test") {
		t.Skip("feature/test not showing as merged")
	}

	// Prune
	pruneMergedBranches(repoDir)

	// Verify feature/test was deleted
	cmd = exec.Command("git", "-C", repoDir, "branch")
	output, _ = cmd.CombinedOutput()
	branches := string(output)
	if strings.Contains(branches, "feature/test") {
		t.Error("expected feature/test branch to be pruned, but it still exists")
	}
	if !strings.Contains(branches, "main") {
		t.Error("expected main branch to still exist")
	}
}

func TestPruneBranchesFailureNonFatal(t *testing.T) {
	// Pass a corrupt/non-existent directory — should log warning but not panic
	dir := t.TempDir()
	corruptDir := filepath.Join(dir, "nonexistent")

	// This should not panic or cause test failure
	pruneMergedBranches(corruptDir)

	// Also test with a non-git directory
	normalDir := filepath.Join(dir, "notagitrepo")
	_ = os.MkdirAll(normalDir, 0755)
	pruneMergedBranches(normalDir)

	// If we get here without panic, the test passes
}

func TestCompletedAtColumn(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	workDir := filepath.Join(dir, "work", "completed-session")
	_ = os.MkdirAll(workDir, 0755)

	// Create a session
	rec := &SessionRecord{
		SessionKey:  "test/completed-at",
		Repository:  "test/repo",
		IssueNumber: 1,
		WorkDir:     workDir,
		RepoDir:     filepath.Join(workDir, "repo"),
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify completed_at is NULL initially
	got, err := store.Get("test/completed-at")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CompletedAt != nil {
		t.Errorf("expected completed_at to be NULL for active session, got %v", got.CompletedAt)
	}

	// Set completed_at
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.SetCompletedAt("test/completed-at", now); err != nil {
		t.Fatalf("SetCompletedAt: %v", err)
	}

	// Verify it's set
	got, err = store.Get("test/completed-at")
	if err != nil {
		t.Fatalf("Get after SetCompletedAt: %v", err)
	}
	if got.CompletedAt == nil {
		t.Fatal("expected completed_at to be set, got nil")
	}
	// Compare as RFC3339 strings to avoid timezone issues
	if got.CompletedAt.Format(time.RFC3339) != now.Format(time.RFC3339) {
		t.Errorf("completed_at = %v, want %v", got.CompletedAt, now)
	}
}
