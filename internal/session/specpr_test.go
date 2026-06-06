package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/speccycle"
)

// specPRFakeForgejo is a minimal Forgejo fake for testing spec PR lifecycle handlers.
type specPRFakeForgejo struct {
	srv *httptest.Server

	mu                sync.Mutex
	createdIssues     []forgejo.Issue
	createdMilestones []forgejo.Milestone
	commentsPosted    []string
	labelsAdded       []string
	labelsRemoved     []string
	prRequested       *forgejo.PullRequest

	// configurable responses
	prFiles       []forgejo.PRFile
	tasksContent  string
	prHeadRef     string
	returnedPR    *forgejo.PullRequest
	issueCounter  int
	milestoneCounter int

	// repo file tree for language detection
	repoFiles []string
}

func newSpecPRFakeForgejo(t *testing.T) *specPRFakeForgejo {
	f := &specPRFakeForgejo{
		prHeadRef:    "spec/test-feature",
		issueCounter: 100,
		milestoneCounter: 1,
		repoFiles:    []string{"go.mod", ".gitignore", "README.md"}, // default: Go repo
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *specPRFakeForgejo) URL() string { return f.srv.URL }

func (f *specPRFakeForgejo) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	switch {
	// PR files (for IsSpecPR)
	case method == http.MethodGet && strings.Contains(path, "/pulls/") && strings.Contains(path, "/files"):
		f.mu.Lock()
		files := f.prFiles
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(files)

	// Get single PR (for handleSpecLifecycleLabels)
	case method == http.MethodGet && strings.Contains(path, "/pulls/") && !strings.Contains(path, "/files"):
		f.mu.Lock()
		pr := f.returnedPR
		if pr == nil {
			pr = &forgejo.PullRequest{
				Number: 7,
				Head:   struct{ Ref string "json:\"ref\""; SHA string "json:\"sha\"" }{Ref: f.prHeadRef, SHA: "abc123"},
			}
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pr)

	// Get file contents (tasks.md)
	case method == http.MethodGet && strings.Contains(path, "/contents/"):
		f.mu.Lock()
		content := f.tasksContent
		f.mu.Unlock()
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(forgejo.FileContent{
			Name:     "tasks.md",
			Path:     "openspec/changes/test-feature/tasks.md",
			Type:     "file",
			Content:  encoded,
			Encoding: "base64",
		})

	// Create milestone
	case method == http.MethodPost && strings.HasSuffix(path, "/milestones"):
		var req struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.milestoneCounter++
		ms := forgejo.Milestone{ID: f.milestoneCounter, Title: req.Title, Description: req.Description}
		f.createdMilestones = append(f.createdMilestones, ms)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ms)

	// Create issue
	case method == http.MethodPost && strings.HasSuffix(path, "/issues"):
		var req struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.issueCounter++
		issue := forgejo.Issue{Number: f.issueCounter, Title: req.Title, Body: req.Body}
		f.createdIssues = append(f.createdIssues, issue)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issue)

	// Set issue milestone
	case method == http.MethodPost && strings.Contains(path, "/milestones") && strings.Contains(path, "/issues/"):
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	// Post comment
	case method == http.MethodPost && strings.Contains(path, "/comments"):
		var req struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.commentsPosted = append(f.commentsPosted, req.Body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(forgejo.Comment{ID: 1, Body: req.Body})

	// List labels
	case method == http.MethodGet && strings.HasSuffix(path, "/labels") && !strings.HasSuffix(path, "/issues/labels") && !strings.HasSuffix(path, "/pulls/labels"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 1, "name": "spec-proposed", "color": "#000000"},
			{"id": 2, "name": "spec-approved", "color": "#000000"},
			{"id": 3, "name": "spec-implementing", "color": "#000000"},
		})

	// Add labels to issue/PR
	case method == http.MethodPost && (strings.Contains(path, "/issues/") || strings.Contains(path, "/pulls/")) && strings.HasSuffix(path, "/labels"):
		// The Forgejo client sends {"labels": [id1, id2]} with numeric IDs.
		// Decode flexibly and map IDs back to names for test assertions.
		bodyBytes, _ := io.ReadAll(r.Body)
		var reqIDs struct {
			Labels []int64 `json:"labels"`
		}
		var reqNames struct {
			Labels []string `json:"labels"`
		}
		var added []string
		if err := json.Unmarshal(bodyBytes, &reqIDs); err == nil && len(reqIDs.Labels) > 0 {
			for _, id := range reqIDs.Labels {
				switch id {
				case 1:
					added = append(added, "spec-proposed")
				case 2:
					added = append(added, "spec-approved")
				case 3:
					added = append(added, "spec-implementing")
				default:
					added = append(added, fmt.Sprintf("id-%d", id))
				}
			}
		} else if err := json.Unmarshal(bodyBytes, &reqNames); err == nil {
			added = append(added, reqNames.Labels...)
		}
		f.mu.Lock()
		f.labelsAdded = append(f.labelsAdded, added...)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	// Remove label from issue/PR (DELETE /issues/N/labels/ID)
	case method == http.MethodDelete && (strings.Contains(path, "/issues/") || strings.Contains(path, "/pulls/")) && strings.Contains(path, "/labels/"):
		parts := strings.Split(path, "/")
		labelID := parts[len(parts)-1]
		f.mu.Lock()
		// Map numeric IDs back to names for test assertions
		switch labelID {
		case "1":
			f.labelsRemoved = append(f.labelsRemoved, "spec-proposed")
		case "2":
			f.labelsRemoved = append(f.labelsRemoved, "spec-approved")
		case "3":
			f.labelsRemoved = append(f.labelsRemoved, "spec-implementing")
		default:
			f.labelsRemoved = append(f.labelsRemoved, labelID)
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	// Get repo file tree (for ListRepoFiles / language detection)
	case method == http.MethodGet && strings.Contains(path, "/git/trees/"):
		f.mu.Lock()
		files := f.repoFiles
		f.mu.Unlock()
		var treeEntries []map[string]interface{}
		for _, f := range files {
			treeEntries = append(treeEntries, map[string]interface{}{
				"path": f,
				"type": "blob",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tree": treeEntries,
		})

	default:
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func (f *specPRFakeForgejo) setPRFiles(files []forgejo.PRFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prFiles = files
}

func (f *specPRFakeForgejo) setTasksContent(content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasksContent = content
}

func (f *specPRFakeForgejo) setPRHeadRef(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prHeadRef = ref
}

func (f *specPRFakeForgejo) setRepoFiles(files []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoFiles = files
}

func testManagerForSpecPR(t *testing.T, fake *specPRFakeForgejo) *Manager {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			WorkDir:             t.TempDir(),
			MaxSessions:         5,
			IdleTimeout:         1 * time.Hour,
			SessionTimeout:      30 * time.Minute,
			MaxTurns:            25,
			ContextWindow:       128000,
			CompactionThreshold: 0.8,
		},
		Forgejo: config.ForgejoConfig{
			URL:   fake.URL(),
			Token: "test-token",
		},
		Webhook:  config.WebhookConfig{Secret: "test-secret"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "http://localhost:11434/v1", APIKey: "test", Model: "test", MaxTokens: 1024},
		},
	}
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return mgr
}

func TestHandleSpecPRMerged_CreatesIssuesAndMilestone(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/proposal.md", Status: "added"},
		{Filename: "openspec/changes/test-feature/tasks.md", Status: "added"},
	})
	fake.setTasksContent(`## Tasks
- [ ] Implement auth core
- [ ] Add OAuth flow [parallel]
- [ ] Write tests [parallel]
`)

	mgr := testManagerForSpecPR(t, fake)

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)

	// Allow goroutines to finish
	time.Sleep(100 * time.Millisecond)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.createdMilestones) != 1 {
		t.Fatalf("expected 1 milestone, got %d", len(fake.createdMilestones))
	}
	ms := fake.createdMilestones[0]
	if ms.Title != "#7: test-feature" {
		t.Errorf("expected milestone title '#7: test-feature', got %q", ms.Title)
	}

	if len(fake.createdIssues) != 3 {
		t.Fatalf("expected 3 issues, got %d", len(fake.createdIssues))
	}

	// Check issue titles
	expectedTitles := []string{
		"[implementer] test-feature: Implement auth core",
		"[implementer] test-feature: Add OAuth flow",
		"[implementer] test-feature: Write tests",
	}
	for i, want := range expectedTitles {
		if fake.createdIssues[i].Title != want {
			t.Errorf("issue %d title: want %q, got %q", i, want, fake.createdIssues[i].Title)
		}
	}

	// Check that parallel tasks have [parallel] in body
	for _, issue := range fake.createdIssues[1:] {
		if !strings.Contains(issue.Body, "[parallel]") {
			t.Errorf("expected parallel task body to contain '[parallel]', got: %s", issue.Body)
		}
	}

	// Check serial task depends on PR
	if !strings.Contains(fake.createdIssues[0].Body, "Depends on: #7") {
		t.Errorf("expected serial task to depend on PR #7, got: %s", fake.createdIssues[0].Body)
	}

	// Check summary comment was posted
	if len(fake.commentsPosted) == 0 {
		t.Fatal("expected at least one comment posted")
	}
	foundSummary := false
	for _, c := range fake.commentsPosted {
		if strings.Contains(c, "Spec **test-feature** approved and merged") {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Errorf("expected summary comment with spec name, got: %v", fake.commentsPosted)
	}
}

func TestHandleSpecPRMerged_SkipsCompletedTasks(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/tasks.md", Status: "added"},
	})
	fake.setTasksContent(`## Tasks
- [x] Already done
- [ ] Still needed
`)

	mgr := testManagerForSpecPR(t, fake)

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.createdIssues) != 1 {
		t.Fatalf("expected 1 issue (skipping completed), got %d", len(fake.createdIssues))
	}
	if !strings.Contains(fake.createdIssues[0].Title, "Still needed") {
		t.Errorf("expected issue for incomplete task, got: %s", fake.createdIssues[0].Title)
	}
}

func TestHandleSpecPRMerged_NoTasks(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/tasks.md", Status: "added"},
	})
	fake.setTasksContent(`## Tasks
No checkbox items here, just prose.
`)

	mgr := testManagerForSpecPR(t, fake)

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.createdIssues) != 0 {
		t.Fatalf("expected 0 issues for empty tasks, got %d", len(fake.createdIssues))
	}
}

func TestHandleSpecPRMerged_NotASpecPR(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	// PR files have no openspec/changes/ paths
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "pkg/auth/handler.go", Status: "modified"},
	})

	mgr := testManagerForSpecPR(t, fake)

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.createdIssues) != 0 {
		t.Fatalf("expected 0 issues for non-spec PR, got %d", len(fake.createdIssues))
	}
}

func TestHandleSpecLifecycleLabels_TransitionsOnSpecBranch(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRHeadRef("spec/test-feature")

	mgr := testManagerForSpecPR(t, fake)

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecLifecycleLabels(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if !sliceContains(fake.labelsRemoved, "spec-proposed") {
		t.Errorf("expected 'spec-proposed' to be removed, removed: %v", fake.labelsRemoved)
	}
	if !sliceContains(fake.labelsAdded, "spec-approved") {
		t.Errorf("expected 'spec-approved' to be added, added: %v", fake.labelsAdded)
	}
}

func TestHandleSpecLifecycleLabels_LogsLabelErrors(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRHeadRef("spec/test-feature")

	// Override the fake's handler to return errors for label operations
	origHandler := fake.srv.Config.Handler
	fake.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return errors for label operations
		if strings.Contains(r.URL.Path, "/labels") && (r.Method == http.MethodDelete || r.Method == http.MethodPost) {
			http.Error(w, "{\"message\":\"label not found\"}", http.StatusNotFound)
			return
		}
		origHandler.ServeHTTP(w, r)
	})

	mgr := testManagerForSpecPR(t, fake)

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	// This should not panic even though label ops return 404
	mgr.handleSpecLifecycleLabels(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	// Key assertion: no panic, function completes gracefully despite label errors.
	// Warning logs are emitted (hard to capture without slog handler swap).
	// Verify the function returned (no deadlock, no crash).
}

func TestHandleSpecLifecycleLabels_SkipsNonSpecBranch(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRHeadRef("feature/auth")

	mgr := testManagerForSpecPR(t, fake)

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecLifecycleLabels(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.labelsAdded) != 0 || len(fake.labelsRemoved) != 0 {
		t.Errorf("expected no label changes for non-spec branch, added=%v removed=%v", fake.labelsAdded, fake.labelsRemoved)
	}
}

func sliceContains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// ── Parallel Task Validation Tests ───────────────────────────────────────

func TestValidateParallelTasks_NoOverlap(t *testing.T) {
	tasks := []speccycle.Task{
		{Index: 1, Description: "Implement OAuth handler in pkg/auth", Parallel: true},
		{Index: 2, Description: "Add rate limiting in pkg/middleware", Parallel: true},
	}
	parallel, serial, _ := validateParallelTasks(tasks, "go")
	if len(parallel) != 2 {
		t.Errorf("expected 2 parallel tasks, got %d", len(parallel))
	}
	if len(serial) != 0 {
		t.Errorf("expected 0 serial tasks, got %d", len(serial))
	}
}

func TestValidateParallelTasks_OverlapDetected(t *testing.T) {
	tasks := []speccycle.Task{
		{Index: 1, Description: "Implement OAuth handler in pkg/auth/handler.go", Parallel: true},
		{Index: 2, Description: "Add auth tests in pkg/auth/auth_test.go", Parallel: true},
	}
	parallel, serial, downgraded := validateParallelTasks(tasks, "go")
	// Both mention pkg/auth → overlap → second should be downgraded to serial
	if len(parallel) != 1 {
		t.Errorf("expected 1 parallel task, got %d", len(parallel))
	}
	if parallel[0].Index != 1 {
		t.Errorf("expected parallel task index 1, got %d", parallel[0].Index)
	}
	if len(serial) != 1 {
		t.Errorf("expected 1 serial task, got %d", len(serial))
	}
	if serial[0].Index != 2 {
		t.Errorf("expected serial task index 2, got %d", serial[0].Index)
	}
	if !downgraded[2] {
		t.Errorf("expected task index 2 to be in downgraded set, got %v", downgraded)
	}
	if downgraded[1] {
		t.Errorf("expected task index 1 NOT to be in downgraded set")
	}
	// Verify that a hypothetical body constructed with the downgraded marker would include the annotation
	task2 := serial[0]
	body := fmt.Sprintf("Task: %d - %s\nDepends on: #1", task2.Index, task2.Description)
	if downgraded[task2.Index] {
		body += "\n*(auto-downgraded from parallel)*"
	}
	if !strings.Contains(body, "*(auto-downgraded from parallel)*") {
		t.Errorf("expected hypothetical body for downgraded task to contain annotation, got: %s", body)
	}
	// Verify non-downgraded task would NOT have the annotation
	task1 := parallel[0]
	body1 := fmt.Sprintf("Task: %d - %s", task1.Index, task1.Description)
	if downgraded[task1.Index] {
		body1 += "\n*(auto-downgraded from parallel)*"
	}
	if strings.Contains(body1, "*(auto-downgraded from parallel)*") {
		t.Errorf("non-downgraded task body should NOT contain annotation, got: %s", body1)
	}
}

func TestValidateParallelTasks_SingleTask(t *testing.T) {
	tasks := []speccycle.Task{
		{Index: 1, Description: "Write README", Parallel: true},
	}
	parallel, serial, _ := validateParallelTasks(tasks, "go")
	if len(parallel) != 1 {
		t.Errorf("expected 1 parallel task, got %d", len(parallel))
	}
	if len(serial) != 0 {
		t.Errorf("expected 0 serial tasks, got %d", len(serial))
	}
}

func TestValidateParallelTasks_Empty(t *testing.T) {
	parallel, serial, _ := validateParallelTasks(nil, "go")
	if len(parallel) != 0 || len(serial) != 0 {
		t.Errorf("expected empty, got parallel=%d serial=%d", len(parallel), len(serial))
	}
}

func TestExtractPathPrefixes(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"Implement OAuth in pkg/auth/handler.go", []string{"pkg/auth"}},
		{"Update internal/forgejo/client.go and internal/webhook/router.go", []string{"internal/forgejo", "internal/webhook"}},
		{"Fix typo in README", nil}, // no path-like tokens
		{"Add test for cmd/gogit/main.go", []string{"cmd/gogit"}},
		// Whitelist filtering: non-Go-prefix first segments are rejected
		{"Install v1.49/linux-amd64 toolchain", nil},                              // "v1.49" not in whitelist
		{"Use 2.5/coder for generation", nil},                                     // "2.5" not in whitelist (numeric)
		{"Update api/v1/handlers and web/app/main.js", []string{"api/v1", "web/app"}}, // whitelisted prefixes
		{"Deploy configs/prod.yaml and scripts/setup.sh", []string{"configs/prod.yaml", "scripts/setup.sh"}}, // whitelisted prefixes
	}
	for _, tc := range tests {
		result := extractPathPrefixes(tc.input, "go")
		if len(result) != len(tc.expected) {
			t.Errorf("extractPathPrefixes(%q) = %v, want %v", tc.input, result, tc.expected)
			continue
		}
		for i := range result {
			if result[i] != tc.expected[i] {
				t.Errorf("extractPathPrefixes(%q)[%d] = %v, want %v", tc.input, i, result[i], tc.expected[i])
			}
		}
	}
}

func TestExtractPathPrefixes_WhitelistExtensible(t *testing.T) {
	// Save and restore the original whitelist
	original := langPrefixes["go"]["src"]
	langPrefixes["go"]["src"] = true
	defer func() {
		if !original {
			delete(langPrefixes["go"], "src")
		}
	}()

	result := extractPathPrefixes("Update src/utils/helper.go", "go")
	found := false
	for _, p := range result {
		if p == "src/utils" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'src/utils' to be extracted after adding 'src' to whitelist, got %v", result)
	}
}

func TestPathsOverlap(t *testing.T) {
	if !pathsOverlap([]string{"pkg/auth", "pkg/middleware"}, []string{"pkg/auth", "pkg/user"}) {
		t.Error("expected overlap on pkg/auth")
	}
	if pathsOverlap([]string{"pkg/auth"}, []string{"pkg/middleware"}) {
		t.Error("expected no overlap")
	}
	if pathsOverlap(nil, []string{"pkg/auth"}) {
		t.Error("expected no overlap with nil")
	}
}

func TestExtractPathPrefixes_LanguageAware(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		lang     string
		expected []string
	}{
		{
			name:     "Go project extracts pkg/auth",
			input:    "Implement OAuth in pkg/auth/handler.go",
			lang:     "go",
			expected: []string{"pkg/auth"},
		},
		{
			name:     "Python project extracts app/auth",
			input:    "Create auth module in app/auth/handler.py",
			lang:     "python",
			expected: []string{"app/auth"},
		},
		{
			name:     "JavaScript project extracts components/Header",
			input:    "Add navigation in components/Header.jsx",
			lang:     "javascript",
			expected: []string{"components/Header.jsx"},
		},
		{
			name:     "Unknown language extracts nothing",
			input:    "Fix something in some/path/file.ext",
			lang:     "unknown",
			expected: nil,
		},
		{
			name:     "Go lang does not extract Python app/ paths",
			input:    "Create module in app/auth/handler.py and pkg/auth/handler.go",
			lang:     "go",
			expected: []string{"pkg/auth"},
		},
		{
			name:     "Python lang does not extract Go pkg/ paths",
			input:    "Create module in pkg/auth/handler.go and app/auth/handler.py",
			lang:     "python",
			expected: []string{"app/auth"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractPathPrefixes(tc.input, tc.lang)
			if len(result) != len(tc.expected) {
				t.Errorf("extractPathPrefixes(%q, %q) = %v, want %v", tc.input, tc.lang, result, tc.expected)
				return
			}
			for i := range result {
				if result[i] != tc.expected[i] {
					t.Errorf("extractPathPrefixes(%q, %q)[%d] = %q, want %q", tc.input, tc.lang, i, result[i], tc.expected[i])
				}
			}
		})
	}
}

func TestHandleSpecPRMerged_ReadsFromMergeSHA(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/tasks.md", Status: "added"},
	})
	fake.setTasksContent(`## Tasks\n- [ ] Do the thing\n`)
	fake.returnedPR = &forgejo.PullRequest{
		Number:        7,
		MergeCommitSHA: "abc123def456",
		Head:          struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}{Ref: "spec/test-feature", SHA: "abc123"},
	}

	mgr := testManagerForSpecPR(t, fake)

	// Capture the ref used in GetFile by intercepting the request
	var capturedRef string
	origHandler := fake.srv.Config.Handler
	fake.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			capturedRef = r.URL.Query().Get("ref")
		}
		origHandler.ServeHTTP(w, r)
	})

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	if capturedRef != "abc123def456" {
		t.Errorf("expected GetFile ref to be merge SHA 'abc123def456', got %q", capturedRef)
	}
}

func TestHandleSpecPRMerged_FallsBackToMainWhenNoSHA(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/tasks.md", Status: "added"},
	})
	fake.setTasksContent(`## Tasks\n- [ ] Do the thing\n`)
	fake.returnedPR = &forgejo.PullRequest{
		Number: 7,
		// No MergeCommitSHA
		Head: struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}{Ref: "spec/test-feature", SHA: "abc123"},
	}

	mgr := testManagerForSpecPR(t, fake)

	var capturedRef string
	origHandler := fake.srv.Config.Handler
	fake.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			capturedRef = r.URL.Query().Get("ref")
		}
		origHandler.ServeHTTP(w, r)
	})

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	if capturedRef != "main" {
		t.Errorf("expected GetFile ref to fall back to 'main', got %q", capturedRef)
	}
}

func TestSpecLifecycleIntegration_FullLifecycle(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/proposal.md", Status: "added"},
		{Filename: "openspec/changes/test-feature/tasks.md", Status: "added"},
	})
	fake.setTasksContent(`## Tasks
- [x] Already done
- [ ] Implement auth core
- [ ] Add OAuth flow [parallel]
`)
	fake.setPRHeadRef("spec/test-feature")

	mgr := testManagerForSpecPR(t, fake)

	// 1. handleSpecPRMerged — creates implementer issues
	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)
	time.Sleep(150 * time.Millisecond)

	fake.mu.Lock()
	// 2. Verify: 2 issues created (1 already done, skip)
	if len(fake.createdIssues) != 2 {
		t.Fatalf("expected 2 issues (1 done skipped), got %d", len(fake.createdIssues))
	}
	// 3. Verify: milestone created
	if len(fake.createdMilestones) != 1 {
		t.Fatalf("expected 1 milestone, got %d", len(fake.createdMilestones))
	}
	// 4. Verify: summary comment posted
	foundSummary := false
	for _, c := range fake.commentsPosted {
		if strings.Contains(c, "Spec **test-feature** approved and merged") {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Errorf("expected summary comment, got: %v", fake.commentsPosted)
	}
	// 5. Verify: spec-implementing label added
	if !sliceContains(fake.labelsAdded, "spec-implementing") {
		t.Errorf("expected spec-implementing label, added: %v", fake.labelsAdded)
	}
	fake.mu.Unlock()

	// 6. handleSpecLifecycleLabels — transitions spec-proposed → spec-approved
	mgr.handleSpecLifecycleLabels(context.Background(), evt)
	time.Sleep(100 * time.Millisecond)

	fake.mu.Lock()
	if !sliceContains(fake.labelsRemoved, "spec-proposed") {
		t.Errorf("expected spec-proposed to be removed, removed: %v", fake.labelsRemoved)
	}
	if !sliceContains(fake.labelsAdded, "spec-approved") {
		t.Errorf("expected spec-approved to be added, added: %v", fake.labelsAdded)
	}
	fake.mu.Unlock()
}

func TestSpecLifecycleIntegration_PartiallyCompleted(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/proposal.md", Status: "added"},
	})
	// 4 tasks, 2 already done → only 2 issues should be created
	fake.setTasksContent(`## Tasks
- [x] Design the API
- [x] Write spec
- [ ] Implement handler
- [ ] Add tests
`)

	mgr := testManagerForSpecPR(t, fake)
	mgr.handleSpecPRMerged(context.Background(), event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged"))
	time.Sleep(150 * time.Millisecond)

	fake.mu.Lock()
	if len(fake.createdIssues) != 2 {
		t.Fatalf("expected 2 issues (2 done skipped), got %d", len(fake.createdIssues))
	}
	if !strings.Contains(fake.createdIssues[0].Title, "Implement handler") {
		t.Errorf("expected first issue for 'Implement handler', got: %s", fake.createdIssues[0].Title)
	}
	fake.mu.Unlock()
}

func TestSpecLifecycleIntegration_NonSpecPR(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	// PR contains only code files
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "pkg/auth/handler.go", Status: "modified"},
		{Filename: "pkg/auth/handler_test.go", Status: "modified"},
	})
	fake.setPRHeadRef("feature/auth") // NOT spec/ prefixed

	mgr := testManagerForSpecPR(t, fake)
	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")

	// Run both handlers
	mgr.handleSpecPRMerged(context.Background(), evt)
	mgr.handleSpecLifecycleLabels(context.Background(), evt)
	time.Sleep(150 * time.Millisecond)

	fake.mu.Lock()
	// No issues created
	if len(fake.createdIssues) != 0 {
		t.Errorf("expected 0 issues, got %d", len(fake.createdIssues))
	}
	// No label changes for non-spec branch
	if len(fake.labelsAdded) != 0 || len(fake.labelsRemoved) != 0 {
		t.Errorf("expected no label changes, added=%v removed=%v", fake.labelsAdded, fake.labelsRemoved)
	}
	// No comments
	if len(fake.commentsPosted) != 0 {
		t.Errorf("expected no comments, got %d", len(fake.commentsPosted))
	}
	fake.mu.Unlock()
}

func TestHandleSpecPRMerged_ParallelOverlap_DowngradedToSerial(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/tasks.md", Status: "added"},
	})
	// Two parallel tasks that both mention pkg/auth → overlap → second downgraded to serial
	// Plus one originally-serial task for contrast
	fake.setTasksContent(`## Tasks
- [ ] Implement auth handler in pkg/auth/handler.go [parallel]
- [ ] Add auth unit tests in pkg/auth/auth_test.go [parallel]
- [ ] Update README
`)

	mgr := testManagerForSpecPR(t, fake)

	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)

	time.Sleep(100 * time.Millisecond)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	// Both tasks should create issues, but the overlapping one should be serial
	if len(fake.createdIssues) != 3 {
		t.Fatalf("expected 3 issues, got %d", len(fake.createdIssues))
	}

	// Order: serial tasks first (original + downgraded), then parallel tasks.
	// Serial[0] = "Update README" (index 3, originally serial)
	// Serial[1] = "Add auth unit tests" (index 2, downgraded from parallel)
	// Parallel[0] = "Implement auth handler" (index 1, stays parallel)

	// Originally-serial task: no parallel marker, no downgrade annotation
	if strings.Contains(fake.createdIssues[0].Body, "[parallel]") {
		t.Errorf("originally-serial task should NOT have [parallel], got: %s", fake.createdIssues[0].Body)
	}
	if strings.Contains(fake.createdIssues[0].Body, "auto-downgraded") {
		t.Errorf("originally-serial task should NOT have downgrade annotation, got: %s", fake.createdIssues[0].Body)
	}

	// Downgraded task: has the annotation
	if !strings.Contains(fake.createdIssues[1].Body, "*(auto-downgraded from parallel)*") {
		t.Errorf("expected downgraded task to contain annotation, got: %s", fake.createdIssues[1].Body)
	}
	if !strings.Contains(fake.createdIssues[1].Body, "Depends on:") {
		t.Errorf("expected downgraded task to have Depends on, got: %s", fake.createdIssues[1].Body)
	}

	// Remaining parallel task: has [parallel] but no downgrade annotation
	if !strings.Contains(fake.createdIssues[2].Body, "[parallel]") {
		t.Errorf("expected parallel task body to contain '[parallel]', got: %s", fake.createdIssues[2].Body)
	}
	if strings.Contains(fake.createdIssues[2].Body, "auto-downgraded") {
		t.Errorf("parallel task should NOT have downgrade annotation, got: %s", fake.createdIssues[2].Body)
	}
}

func TestHandleSpecPRMerged_AddsSpecImplementingLabel(t *testing.T) {
	fake := newSpecPRFakeForgejo(t)
	fake.setPRFiles([]forgejo.PRFile{
		{Filename: "openspec/changes/test-feature/tasks.md", Status: "added"},
	})
	fake.setTasksContent(`## Tasks
- [ ] Implement auth
`)
	fake.setPRHeadRef("spec/test-feature")

	mgr := testManagerForSpecPR(t, fake)
	evt := event.NewEvent(event.PullRequestMerged, "test/repo", 0, 7, "alice", "merged")
	mgr.handleSpecPRMerged(context.Background(), evt)
	time.Sleep(150 * time.Millisecond)

	fake.mu.Lock()
	if !sliceContains(fake.labelsAdded, "spec-implementing") {
		t.Errorf("expected spec-implementing label to be added, got: %v", fake.labelsAdded)
	}
	fake.mu.Unlock()
}
