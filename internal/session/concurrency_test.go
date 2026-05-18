package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

type concurrencyForgejo struct {
	srv        *httptest.Server
	issueTitles map[int]string
	mu         sync.Mutex
}

func newConcurrencyForgejo(t *testing.T) *concurrencyForgejo {
	f := &concurrencyForgejo{
		issueTitles: make(map[int]string),
	}
	for i := 1; i <= 10; i++ {
		f.issueTitles[i] = fmt.Sprintf("[implementer] Issue %d", i)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	return f
}

func (f *concurrencyForgejo) URL() string { return f.srv.URL }

func (f *concurrencyForgejo) Close() { f.srv.Close() }

func (f *concurrencyForgejo) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/git/trees/"):
		tree := []map[string]interface{}{
			{"path": "go.mod", "type": "blob"},
			{"path": "README.md", "type": "blob"},
			{"path": "main.go", "type": "blob"},
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"tree": tree})

	case r.Method == http.MethodGet && strings.Contains(path, "/issues/") &&
		!strings.Contains(path, "/comments") && !strings.Contains(path, "/labels"):
		parts := strings.Split(strings.TrimRight(path, "/"), "/")
		numStr := parts[len(parts)-1]
		var num int
		fmt.Sscanf(numStr, "%d", &num)

		f.mu.Lock()
		title := f.issueTitles[num]
		f.mu.Unlock()

		labels := []map[string]string{
			{"name": "role:implementer"},
		}
		resp := map[string]interface{}{
			"number": num,
			"title":  title,
			"body":   "Test body",
			"state":  "open",
			"labels": labels,
		}
		_ = json.NewEncoder(w).Encode(resp)

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/labels"):
		labels := []map[string]interface{}{
			{"id": int64(1), "name": "role:implementer"},
			{"id": int64(2), "name": "ready"},
			{"id": int64(3), "name": "implementing"},
			{"id": int64(4), "name": "done"},
			{"id": int64(5), "name": "blocked"},
			{"id": int64(6), "name": "planning"},
			{"id": int64(7), "name": "needs-role"},
		}
		_ = json.NewEncoder(w).Encode(labels)

	case r.Method == http.MethodPost && strings.Contains(path, "/comments"):
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 1})

	case r.Method == http.MethodPost && strings.Contains(path, "/issues/") && strings.Contains(path, "/labels"):
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{{"id": 1, "name": "role:implementer"}})

	default:
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func concurrencyTestConfig(t *testing.T, forgejoURL string) *config.Config {
	return &config.Config{
		Forgejo: config.ForgejoConfig{
			URL:   forgejoURL,
			Token: "test-token",
		},
		Agent: config.AgentConfig{
			MaxSessions:             20,
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

func TestConcurrentIssueOpenedSessions(t *testing.T) {
	f := newConcurrencyForgejo(t)
	defer f.Close()

	cfg := concurrencyTestConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.shutdownAll()

	const numIssues = 10
	var wg sync.WaitGroup
	var eventsPublished atomic.Int64

	wg.Add(numIssues)
	for i := 1; i <= numIssues; i++ {
		go func(issueNum int) {
			defer wg.Done()
			evt := event.NewEvent(event.IssueOpened, "fjadmin/testbed", issueNum, 0, "alice", "opened")
			evt.SessionKey = fmt.Sprintf("fjadmin/testbed/issues/%d", issueNum)
			evt.Payload = map[string]interface{}{
				"issue": map[string]interface{}{
					"number": float64(issueNum),
					"title":  fmt.Sprintf("[implementer] Issue %d", issueNum),
					"body":   "Do the work",
				},
			}
			mgr.handleEvent(context.Background(), evt)
			eventsPublished.Add(1)
		}(i)
	}
	wg.Wait()

	if eventsPublished.Load() != numIssues {
		t.Fatalf("expected %d events published, got %d", numIssues, eventsPublished.Load())
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		mgr.mu.RLock()
		count := len(mgr.sessions)
		mgr.mu.RUnlock()
		if count >= numIssues {
			break
		}
		if time.Now().After(deadline) {
			mgr.mu.RLock()
			existing := make([]string, 0, len(mgr.sessions))
			for k := range mgr.sessions {
				existing = append(existing, k)
			}
			mgr.mu.RUnlock()
			t.Fatalf("timed out waiting for sessions: got %d, want %d; existing: %v", count, numIssues, existing)
		}
		time.Sleep(50 * time.Millisecond)
	}

	mgr.mu.RLock()
	sessions := mgr.sessions
	count := len(sessions)
	mgr.mu.RUnlock()

	if count != numIssues {
		t.Errorf("expected exactly %d sessions, got %d", numIssues, count)
	}

	seen := make(map[string]int)
	mgr.mu.RLock()
	for key, sess := range mgr.sessions {
		seen[key]++
		if sess.Key != key {
			t.Errorf("session key mismatch: map key=%q, sess.Key=%q", key, sess.Key)
		}
	}
	mgr.mu.RUnlock()

	for key, n := range seen {
		if n > 1 {
			t.Errorf("duplicate session key in map: %q appears %d times (should be impossible for map, but checking)", key, n)
		}
	}

	for i := 1; i <= numIssues; i++ {
		expectedKey := fmt.Sprintf("fjadmin/testbed/issues/%d", i)
		mgr.mu.RLock()
		sess, exists := mgr.sessions[expectedKey]
		mgr.mu.RUnlock()
		if !exists {
			t.Errorf("missing session for issue %d (key=%q)", i, expectedKey)
		} else {
			if sess.IssueNumber != i {
				t.Errorf("session %q: expected IssueNumber=%d, got %d", expectedKey, i, sess.IssueNumber)
			}
		}
	}

	duplicateKeys := make(map[string]int)
	mgr.mu.RLock()
	for key := range mgr.sessions {
		duplicateKeys[key]++
	}
	mgr.mu.RUnlock()
	for key, n := range duplicateKeys {
		if n > 1 {
			t.Errorf("duplicate session key: %q appears %d times", key, n)
		}
	}
}
