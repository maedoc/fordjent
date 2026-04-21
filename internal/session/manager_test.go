package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

func TestManagerCreatesSession(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			MaxSessions: 10,
			IdleTimeout: 4 * time.Hour,
			WorkDir:     t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Forgejo:  config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr := NewManager(cfg, bus)

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
			MaxSessions: 10,
			IdleTimeout: 4 * time.Hour,
			WorkDir:     t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Forgejo:  config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr := NewManager(cfg, bus)

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
			MaxSessions: 10,
			IdleTimeout: 4 * time.Hour,
			WorkDir:     t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Forgejo:  config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr := NewManager(cfg, bus)

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
			MaxSessions: 2,
			IdleTimeout: 4 * time.Hour,
			WorkDir:     t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Forgejo:  config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr := NewManager(cfg, bus)

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
			MaxSessions: 10,
			IdleTimeout: 4 * time.Hour,
			WorkDir:     t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Forgejo:  config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr := NewManager(cfg, bus)

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
			MaxSessions: 100,
			IdleTimeout: 4 * time.Hour,
			WorkDir:     t.TempDir(),
			CommitPrefix: "[agent-automation]",
		},
		Forgejo:  config.ForgejoConfig{URL: "https://example.com", Token: "fake"},
		Providers: []config.ProviderConfig{
			{Name: "test", APIBase: "https://example.com/v1", APIKey: "fake", Model: "test", MaxTokens: 100},
		},
		Memory:   config.MemoryConfig{Enabled: false, CompactionPath: "docs/issues"},
		Security: config.SecurityConfig{FilterAgentEvents: false},
	}
	bus := event.NewBus()
	mgr := NewManager(cfg, bus)

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
