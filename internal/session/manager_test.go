package session

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
)

func TestManagerCreatesSession(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			MaxSessions:  10,
			IdleTimeout:  4 * time.Hour,
			WorkDir:      t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Database:  config.DatabaseConfig{Path: ""},
		Forgejo:   config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/42"

	sess, err := mgr.getOrCreate(context.Background(), evt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Key != "org/repo/issues/42" {
		t.Errorf("expected session key org/repo/issues/42, got %s", sess.Key)
	}
	if sess.Repository != "org/repo" {
		t.Errorf("expected org/repo, got %s", sess.Repository)
	}
	if sess.IssueNumber != 42 {
		t.Errorf("expected issue 42, got %d", sess.IssueNumber)
	}
}

func TestManagerSessionAffinity(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			MaxSessions:  10,
			IdleTimeout:  4 * time.Hour,
			WorkDir:      t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Database:  config.DatabaseConfig{Path: ""},
		Forgejo:   config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	evt1 := event.NewEvent(event.IssueCommentCreated, "org/repo", 42, 0, "alice", "created")
	evt1.SessionKey = "org/repo/issues/42"

	evt2 := event.NewEvent(event.IssueCommentCreated, "org/repo", 42, 0, "bob", "created")
	evt2.SessionKey = "org/repo/issues/42"

	sess1, _ := mgr.getOrCreate(context.Background(), evt1)
	sess2, _ := mgr.getOrCreate(context.Background(), evt2)

	if sess1 != sess2 {
		t.Error("expected same session for same session key (affinity)")
	}
}

func TestManagerDifferentSessions(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			MaxSessions:  10,
			IdleTimeout:  4 * time.Hour,
			WorkDir:      t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Database:  config.DatabaseConfig{Path: ""},
		Forgejo:   config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	evt1 := event.NewEvent(event.IssueOpened, "org/repo", 1, 0, "alice", "opened")
	evt1.SessionKey = "org/repo/issues/1"

	evt2 := event.NewEvent(event.IssueOpened, "org/repo", 2, 0, "bob", "opened")
	evt2.SessionKey = "org/repo/issues/2"

	sess1, _ := mgr.getOrCreate(context.Background(), evt1)
	sess2, _ := mgr.getOrCreate(context.Background(), evt2)

	if sess1 == sess2 {
		t.Error("expected different sessions for different session keys")
	}
}

func TestManagerMaxSessionsEnforced(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			MaxSessions:  2,
			IdleTimeout:  4 * time.Hour,
			WorkDir:      t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Database:  config.DatabaseConfig{Path: ""},
		Forgejo:   config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	// Create 2 sessions (max)
	for i := 1; i <= 3; i++ {
		evt := event.NewEvent(event.IssueOpened, "org/repo", i, 0, "alice", "opened")
		evt.SessionKey = "org/repo/issues/" + string(rune('0'+i))
		sess, err := mgr.getOrCreate(context.Background(), evt)
		if i <= 2 {
			if err != nil {
				t.Errorf("session %d: unexpected error: %v", i, err)
			}
			// Mark as not busy so eviction can work
			sess.mu.Lock()
			sess.busy = false
			sess.mu.Unlock()
		}
	}

	// Third should evict oldest idle
	mgr.mu.RLock()
	count := len(mgr.sessions)
	mgr.mu.RUnlock()
	if count > 2 {
		t.Errorf("expected at most 2 sessions, got %d", count)
	}
}

func TestManagerShutdownAll(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			MaxSessions:  10,
			IdleTimeout:  4 * time.Hour,
			WorkDir:      t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Database:  config.DatabaseConfig{Path: ""},
		Forgejo:   config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	for i := 1; i <= 3; i++ {
		evt := event.NewEvent(event.IssueOpened, "org/repo", i, 0, "alice", "opened")
		evt.SessionKey = "org/repo/issues/" + string(rune('0'+i))
		mgr.getOrCreate(context.Background(), evt)
	}

	mgr.shutdownAll()

	mgr.mu.RLock()
	count := len(mgr.sessions)
	mgr.mu.RUnlock()
	if count != 0 {
		t.Errorf("expected 0 sessions after shutdown, got %d", count)
	}
}

func TestManagerConcurrentAccess(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			MaxSessions:  100,
			IdleTimeout:  4 * time.Hour,
			WorkDir:      t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Database:  config.DatabaseConfig{Path: ""},
		Forgejo:   config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			evt := event.NewEvent(event.IssueOpened, "org/repo", n, 0, "alice", "opened")
			evt.SessionKey = "org/repo/issues/" + string(rune('A'+n))
			mgr.getOrCreate(context.Background(), evt)
		}(i)
	}
	wg.Wait()

	mgr.mu.RLock()
	count := len(mgr.sessions)
	mgr.mu.RUnlock()
	if count != 20 {
		t.Errorf("expected 20 sessions, got %d", count)
	}
}

func TestBuildCloneURL(t *testing.T) {
	tests := []struct {
		base, token, repo string
		want              string
	}{
		{"https://git.example.com", "tok", "org/repo", "https://tok@git.example.com/org/repo.git"},
		{"http://localhost:3000", "", "org/repo", "http://localhost:3000/org/repo.git"},
		{"https://git.example.com", "tok", "user/repo", "https://tok@git.example.com/user/repo.git"},
	}
	for _, tt := range tests {
		got := buildCloneURL(tt.base, tt.token, tt.repo)
		if got != tt.want {
			t.Errorf("buildCloneURL(%q,%q,%q) = %q, want %q", tt.base, tt.token, tt.repo, got, tt.want)
		}
	}
}

func TestManager_RestoreSessions(t *testing.T) {
	workDir := t.TempDir()
	dbPath := filepath.Join(workDir, "sessions.db")

	cfg1 := &config.Config{
		Agent: config.AgentConfig{
			MaxSessions:  10,
			IdleTimeout:  4 * time.Hour,
			WorkDir:      filepath.Join(workDir, "work"),
			CommitPrefix: "[agent-automation]",
		},
		Database:  config.DatabaseConfig{Path: dbPath},
		Forgejo:   config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100}},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus1 := event.NewBus()
	mgr1, err := NewManager(cfg1, bus1)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/42"
	sess1, err := mgr1.getOrCreate(context.Background(), evt)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	mgr1.shutdownAll()

	cfg2 := *cfg1
	bus2 := event.NewBus()
	mgr2, err := NewManager(&cfg2, bus2)
	if err != nil {
		t.Fatalf("new manager 2: %v", err)
	}
	defer mgr2.store.Close()

	mgr2.mu.RLock()
	restored, ok := mgr2.sessions["org/repo/issues/42"]
	mgr2.mu.RUnlock()
	if !ok {
		t.Fatal("expected session to be restored from SQLite")
	}
	if restored.Key != sess1.Key {
		t.Errorf("key mismatch: got %q, want %q", restored.Key, sess1.Key)
	}
	if restored.Repository != sess1.Repository {
		t.Errorf("repo mismatch")
	}
}

func TestDetectRoleFromTitle(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"[pm] Plan the sprint", "pm"},
		{"[project manager] Organize backlog", "pm"},
		{"[decompose] Break down feature", "pm"},
		{"[review] Check the code", "reviewer"},
		{"[code review] Review PR #5", "reviewer"},
		{"[reviewer] Audit codebase", "reviewer"},
		{"[devops] Set up CI pipeline", "devops"},
		{"[infra] Provision servers", "devops"},
		{"[ci/cd] Configure actions", "devops"},
		{"[docker] Build container image", "devops"},
		{"[test] Write unit tests", "tester"},
		{"[tester] QA the release", "tester"},
		{"[testing] Integration tests", "tester"},
		{"[qa] Quality check", "tester"},
		{"[implementer] Add login feature", "implementer"},
		{"[implement] Build auth module", "implementer"},
		{"[dev] Fix the bug", "implementer"},
		{"[developer] Refactor API", "implementer"},
		{"No tag here", ""},
		{"Random issue title", ""},
	}
	for _, tt := range tests {
		got := detectRoleFromTitle(tt.title)
		if got != tt.want {
			t.Errorf("detectRoleFromTitle(%q) = %q, want %q", tt.title, got, tt.want)
		}
	}
}

func TestDetectRoleFromBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty body", "", ""},
		{"no role keywords", "Fix the login bug by updating the auth handler", ""},
		{"ambiguous build", "Build a simple API endpoint", ""},
		{"ambiguous test word", "Add a test for the build process", ""},
		{"implementer: role prefix", "role: implementer\n\nAdd auth module", "implementer"},
		{"implementer: as implementer", "As implementer, I should add the login feature", "implementer"},
		{"implementer: write code", "Write code to implement the payment flow", "implementer"},
		{"implementer: as developer", "As developer, please implement this", "implementer"},
		{"implementer: this is a implementer task", "This is a implementer task for the auth module", "implementer"},
		{"pm: role pm", "role: pm\n\nCoordinate the release", "pm"},
		{"pm: as pm", "As pm, break down the feature into sub-issues", "pm"},
		{"pm: decompose this", "Please decompose this into smaller tasks", "pm"},
		{"pm: break down the work", "Break down the work into sub-issues", "pm"},
		{"pm: this is a pm task", "This is a PM task to plan the sprint", "pm"},
		{"reviewer: role reviewer", "role: reviewer\n\nReview PR #5", "reviewer"},
		{"reviewer: as reviewer", "As reviewer, check the code changes", "reviewer"},
		{"reviewer: this is a review task", "This is a review task for the API", "reviewer"},
		{"tester: role tester", "role: tester\n\nWrite integration tests", "tester"},
		{"tester: as qa", "As QA, verify the release works", "tester"},
		{"tester: integration test", "This is an integration test task for the API", "tester"},
		{"devops: role devops", "role: devops\n\nSet up CI pipeline", "devops"},
		{"devops: as devops", "As devops, configure the deployment", "devops"},
		{"devops: ci/cd pipeline", "This is a CI/CD pipeline task", "devops"},
		{"priority: implementer over pm", "role: implementer\n\nAlso some pm-like text", "implementer"},
		{"case insensitive", "Role: PM\n\nPlan the sprint", "pm"},
		{"no false positive on build", "We need to build the docker image", ""},
		{"no false positive on plan", "Plan the implementation of feature X", ""},
		{"no false positive on test in sentence", "This is a test of the build system", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectRoleFromBody(tt.body)
			if got != tt.want {
				t.Errorf("detectRoleFromBody(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestDetectRoleFromIssueBodyFallback(t *testing.T) {
	tests := []struct {
		name   string
		title  string
		labels []string
		body   string
		want   string
	}{
		{"title takes priority", "[pm] Plan sprint", nil, "role: implementer", "pm"},
		{"label takes priority over body", "Fix bug", []string{"role:reviewer"}, "role: implementer", "reviewer"},
		{"body fallback when no title/label", "Fix bug", nil, "role: implementer", "implementer"},
		{"body fallback for pm", "Plan the work", nil, "This is a PM task to decompose", "pm"},
		{"no role anywhere", "Fix bug", nil, "Update the login handler", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := &forgejo.Issue{
				Title: tt.title,
				Body:  tt.body,
			}
			if tt.labels != nil {
				issue.Labels = make([]forgejo.Label, len(tt.labels))
				for i, l := range tt.labels {
					issue.Labels[i] = forgejo.Label{Name: l}
				}
			}
			got := detectRoleFromIssue(issue)
			if got != tt.want {
				t.Errorf("detectRoleFromIssue() = %q, want %q", got, tt.want)
			}
		})
	}
}
