package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
)

type mockChecker struct {
	mu      sync.Mutex
	active  map[string]bool
	checked []string
}

func newMockChecker() *mockChecker {
	return &mockChecker{active: make(map[string]bool)}
}

func (m *mockChecker) HasActiveSession(repo string, issueNumber int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s/issues/%d", repo, issueNumber)
	m.checked = append(m.checked, key)
	return m.active[key]
}

func (m *mockChecker) setActive(repo string, issueNumber int, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s/issues/%d", repo, issueNumber)
	if active {
		m.active[key] = true
	} else {
		delete(m.active, key)
	}
}

func fakeForgejoServer(issues []forgejo.Issue) *forgejo.Client {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/test/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(issues)
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	return forgejo.NewClient(srv.URL, "test-token")
}

func TestScan_ReadyNoSession(t *testing.T) {
	issues := []forgejo.Issue{
		{Number: 1, Title: "Do thing", Labels: []forgejo.Label{{Name: "ready"}}},
		{Number: 2, Title: "Other", Labels: []forgejo.Label{{Name: "planning"}}},
	}
	client := fakeForgejoServer(issues)
	bus := event.NewBus()
	checker := newMockChecker()

	received := make(chan *event.Event, 10)
	sub := bus.Subscribe()
	go func() {
		for evt := range sub {
			received <- evt
		}
	}()

	s := NewScanner(ScannerConfig{
		ForgejoClient: client,
		Checker:       checker,
		Bus:           bus,
		Repo:          "test",
		Interval:      100 * time.Millisecond,
		Logger:        slog.Default(),
	})

	s.scan()

	select {
	case evt := <-received:
		if evt.Type != event.IssueOpened {
			t.Errorf("expected IssueOpened, got %v", evt.Type)
		}
		if evt.IssueNumber != 1 {
			t.Errorf("expected issue 1, got %d", evt.IssueNumber)
		}
		if evt.Action != "green_light" {
			t.Errorf("expected green_light action, got %q", evt.Action)
		}
		if evt.Sender != "fordjent-scanner" {
			t.Errorf("expected sender fordjent-scanner, got %q", evt.Sender)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestScan_ReadyWithSession(t *testing.T) {
	issues := []forgejo.Issue{
		{Number: 1, Title: "Do thing", Labels: []forgejo.Label{{Name: "ready"}}},
	}
	client := fakeForgejoServer(issues)
	bus := event.NewBus()
	checker := newMockChecker()
	checker.setActive("test", 1, true)

	sub := bus.Subscribe()
	s := NewScanner(ScannerConfig{
		ForgejoClient: client,
		Checker:       checker,
		Bus:           bus,
		Repo:          "test",
		Interval:      100 * time.Millisecond,
		Logger:        slog.Default(),
	})

	s.scan()

	select {
	case <-sub:
		t.Error("should not have published event for issue with active session")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestScan_NoReadyLabel(t *testing.T) {
	issues := []forgejo.Issue{
		{Number: 1, Title: "Do thing", Labels: []forgejo.Label{{Name: "planning"}}},
		{Number: 2, Title: "Other", Labels: []forgejo.Label{}},
	}
	client := fakeForgejoServer(issues)
	bus := event.NewBus()
	checker := newMockChecker()

	sub := bus.Subscribe()
	s := NewScanner(ScannerConfig{
		ForgejoClient: client,
		Checker:       checker,
		Bus:           bus,
		Repo:          "test",
		Interval:      100 * time.Millisecond,
		Logger:        slog.Default(),
	})

	s.scan()

	select {
	case <-sub:
		t.Error("should not have published event for issue without ready label")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestStartStop(t *testing.T) {
	issues := []forgejo.Issue{
		{Number: 1, Title: "Do thing", Labels: []forgejo.Label{{Name: "ready"}}},
	}
	client := fakeForgejoServer(issues)
	bus := event.NewBus()
	checker := newMockChecker()

	s := NewScanner(ScannerConfig{
		ForgejoClient: client,
		Checker:       checker,
		Bus:           bus,
		Repo:          "test",
		Interval:      50 * time.Millisecond,
		Logger:        slog.Default(),
	})

	s.Start()
	time.Sleep(200 * time.Millisecond)
	s.Stop()

	checker.mu.Lock()
	checked := len(checker.checked)
	checker.mu.Unlock()
	if checked == 0 {
		t.Error("expected scanner to check at least one issue")
	}
}

func TestScan_MultipleReadyOrphans(t *testing.T) {
	issues := []forgejo.Issue{
		{Number: 1, Title: "A", Labels: []forgejo.Label{{Name: "ready"}}},
		{Number: 2, Title: "B", Labels: []forgejo.Label{{Name: "ready"}}},
		{Number: 3, Title: "C", Labels: []forgejo.Label{{Name: "ready"}}},
	}
	client := fakeForgejoServer(issues)
	bus := event.NewBus()
	checker := newMockChecker()
	checker.setActive("test", 3, true)

	received := make([]*event.Event, 0)
	mu := sync.Mutex{}
	sub := bus.Subscribe()
	go func() {
		for evt := range sub {
			mu.Lock()
			received = append(received, evt)
			mu.Unlock()
		}
	}()

	s := NewScanner(ScannerConfig{
		ForgejoClient: client,
		Checker:       checker,
		Bus:           bus,
		Repo:          "test",
		Interval:      100 * time.Millisecond,
		Logger:        slog.Default(),
	})

	s.scan()

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	count := len(received)
	mu.Unlock()
	if count != 2 {
		t.Errorf("expected 2 events for orphaned ready issues, got %d", count)
	}
}

func TestScan_EmptyRepo(t *testing.T) {
	client := fakeForgejoServer(nil)
	bus := event.NewBus()
	checker := newMockChecker()

	sub := bus.Subscribe()
	s := NewScanner(ScannerConfig{
		ForgejoClient: client,
		Checker:       checker,
		Bus:           bus,
		Repo:          "test",
		Interval:      100 * time.Millisecond,
		Logger:        slog.Default(),
	})

	s.scan()

	select {
	case <-sub:
		t.Error("should not have published event for empty repo")
	case <-time.After(200 * time.Millisecond):
	}
}

var _ context.Context
