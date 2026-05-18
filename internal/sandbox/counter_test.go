package sandbox

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

type mockReporter struct {
	calls atomic.Int32
	last  SandboxError
}

func (m *mockReporter) ReportSandboxViolation(_ context.Context, _ string, _ int, err SandboxError) {
	m.calls.Add(1)
	m.last = err
}

func TestViolationCounter_OnViolation(t *testing.T) {
	reporter := &mockReporter{}
	counter := NewViolationCounter(3, reporter, "owner/repo", 5)

	err := SandboxError{
		Command:  "go test ./...",
		Backend:  "sandbox-exec",
		Violated: true,
	}

	counter.OnViolation(context.Background(), "session1", err)
	if reporter.calls.Load() != 0 {
		t.Error("reporter should not be called below threshold")
	}

	counter.OnViolation(context.Background(), "session1", err)
	if reporter.calls.Load() != 0 {
		t.Error("reporter should not be called below threshold (2)")
	}

	counter.OnViolation(context.Background(), "session1", err)
	if reporter.calls.Load() != 1 {
		t.Error("reporter should be called at threshold")
	}

	if reporter.last.Command != "go test ./..." {
		t.Errorf("reporter received wrong error: %+v", reporter.last)
	}
}

func TestViolationCounter_OnSuccess(t *testing.T) {
	reporter := &mockReporter{}
	counter := NewViolationCounter(3, reporter, "owner/repo", 5)

	err := SandboxError{Command: "cmd", Violated: true}

	counter.OnViolation(context.Background(), "s1", err)
	counter.OnViolation(context.Background(), "s1", err)
	counter.OnSuccess("s1")
	counter.OnViolation(context.Background(), "s1", err)

	if reporter.calls.Load() != 0 {
		t.Error("reporter should not be called after reset (only 1 violation since reset)")
	}
}

func TestViolationCounter_MultipleSessions(t *testing.T) {
	reporter := &mockReporter{}
	counter := NewViolationCounter(2, reporter, "owner/repo", 5)

	err := SandboxError{Command: "cmd", Violated: true}

	counter.OnViolation(context.Background(), "s1", err)
	counter.OnViolation(context.Background(), "s2", err)
	counter.OnViolation(context.Background(), "s1", err)
	counter.OnViolation(context.Background(), "s2", err)

	if reporter.calls.Load() != 2 {
		t.Errorf("expected 2 reports (one per session), got %d", reporter.calls.Load())
	}
}

func TestViolationCounter_NilReporter(t *testing.T) {
	counter := NewViolationCounter(1, nil, "owner/repo", 5)

	err := SandboxError{Command: "cmd", Violated: true}

	counter.OnViolation(context.Background(), "s1", err)
}

func TestBuildViolationComment(t *testing.T) {
	err := SandboxError{
		Command:       "go test ./...",
		ProfilePath:   "/tmp/fordjent-sandbox-abc.sb",
		SandboxStderr: "sandbox-exec[123]: deny file-write* /etc/hosts\nsome other line\n",
		Violated:      true,
		Backend:       "sandbox-exec",
	}

	comment := BuildViolationComment(err, 3)

	if !strings.Contains(comment, "**Sandbox Policy Violation** (3 consecutive)") {
		t.Error("comment missing violation header")
	}
	if !strings.Contains(comment, "go test ./...") {
		t.Error("comment missing command")
	}
	if !strings.Contains(comment, "deny file-write* /etc/hosts") {
		t.Error("comment missing deny line")
	}
	if !strings.Contains(comment, "/tmp/fordjent-sandbox-abc.sb") {
		t.Error("comment missing profile path")
	}
	if !strings.Contains(comment, "allowed_write_dirs") {
		t.Error("comment missing fix suggestion")
	}
	if !strings.Contains(comment, "<!-- ford -->") {
		t.Error("comment missing ford marker")
	}
}

func TestNewViolationCounter_DefaultThreshold(t *testing.T) {
	counter := NewViolationCounter(0, nil, "owner/repo", 5)
	if counter.threshold != 3 {
		t.Errorf("expected default threshold 3, got %d", counter.threshold)
	}
}
