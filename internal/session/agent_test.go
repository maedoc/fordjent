package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/lifecycle"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/tool"
)

func TestBuildTurnSignature(t *testing.T) {
	calls := []provider.ToolCall{
		{Function: provider.FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}},
	}
	sig := buildTurnSignature(calls)
	if sig.tools == "" {
		t.Fatal("expected non-empty signature")
	}
}

func TestBuildTurnSignatureIdentical(t *testing.T) {
	calls := []provider.ToolCall{
		{Function: provider.FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}},
	}
	sig1 := buildTurnSignature(calls)
	sig2 := buildTurnSignature(calls)
	if sig1.tools != sig2.tools {
		t.Fatalf("identical calls should produce identical signatures: %q vs %q", sig1.tools, sig2.tools)
	}
}

func TestBuildTurnSignatureDifferent(t *testing.T) {
	sig1 := buildTurnSignature([]provider.ToolCall{
		{Function: provider.FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}},
	})
	sig2 := buildTurnSignature([]provider.ToolCall{
		{Function: provider.FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}},
	})
	if sig1.tools == sig2.tools {
		t.Fatal("different calls should produce different signatures")
	}
}

func TestAllSameSignature(t *testing.T) {
	sig := turnSignature{tools: "bash(abcd)"}
	if !allSameSignature([]turnSignature{sig, sig, sig}) {
		t.Fatal("expected same signatures to return true")
	}
	sig2 := turnSignature{tools: "read(efgh)"}
	if allSameSignature([]turnSignature{sig, sig, sig2}) {
		t.Fatal("expected mixed signatures to return false")
	}
}

func TestIsImplementationTool(t *testing.T) {
	impl := []string{"write_file", "git", "forgejo_create_pr", "forgejo_merge_pr"}
	for _, name := range impl {
		if !isImplementationTool(name) {
			t.Errorf("expected %s to be an implementation tool", name)
		}
	}
	nonImpl := []string{"bash", "read_file", "forgejo_comment", "forgejo_get_issue", "forgejo_list_issues"}
	for _, name := range nonImpl {
		if isImplementationTool(name) {
			t.Errorf("expected %s to NOT be an implementation tool", name)
		}
	}
}

func newTestAgentServer(t *testing.T, labels []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues/") && !strings.Contains(r.URL.Path, "/comments") && !strings.Contains(r.URL.Path, "/labels") {
			labelObjs := make([]map[string]string, len(labels))
			for i, l := range labels {
				labelObjs[i] = map[string]string{"name": l}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 42,
				"title":  "Test issue",
				"body":   "Test body",
				"state":  "open",
				"labels": labelObjs,
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/labels") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
}

func TestDetectIssueState_Planning(t *testing.T) {
	srv := newTestAgentServer(t, []string{"planning", "role:implementer"})
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "implementer", nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	state := agent.detectIssueState(context.Background(), evt)
	if state != lifecycle.StatePlanning {
		t.Errorf("expected StatePlanning, got %s", state)
	}
}

func TestDetectIssueState_Blocked(t *testing.T) {
	srv := newTestAgentServer(t, []string{"blocked", "role:implementer"})
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "implementer", nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	state := agent.detectIssueState(context.Background(), evt)
	if state != lifecycle.StateFSMBlocked {
		t.Errorf("expected StateFSMBlocked, got %s", state)
	}
}

func TestDetectIssueState_Opened(t *testing.T) {
	srv := newTestAgentServer(t, []string{"role:implementer"})
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "implementer", nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	state := agent.detectIssueState(context.Background(), evt)
	if state != lifecycle.StateOpened {
		t.Errorf("expected StateOpened, got %s", state)
	}
}

func TestDetectIssueState_NoIssueNumber(t *testing.T) {
	srv := newTestAgentServer(t, nil)
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/push/1", Repository: "org/repo", WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "implementer", nil)

	evt := event.NewEvent(event.Push, "org/repo", 0, 0, "alice", "push")
	state := agent.detectIssueState(context.Background(), evt)
	if state != lifecycle.StateOpened {
		t.Errorf("expected StateOpened for event with no issue number, got %s", state)
	}
}

func TestIssueStateInstructions_Planning(t *testing.T) {
	instructions := issueStateInstructions(lifecycle.StatePlanning)
	if !strings.Contains(instructions, "Planning") {
		t.Errorf("expected planning instructions, got: %s", instructions)
	}
	if !strings.Contains(instructions, "do not write code") && !strings.Contains(instructions, "STOP") {
		t.Errorf("expected planning instructions to prohibit code writing, got: %s", instructions)
	}
	if !strings.Contains(instructions, "BLOCKED") {
		t.Errorf("expected planning instructions to mention BLOCKED tools, got: %s", instructions)
	}
}

func TestIssueStateInstructions_Blocked(t *testing.T) {
	instructions := issueStateInstructions(lifecycle.StateFSMBlocked)
	if !strings.Contains(instructions, "Blocked") {
		t.Errorf("expected blocked instructions, got: %s", instructions)
	}
	if !strings.Contains(instructions, "Depends on") {
		t.Errorf("expected blocked instructions to mention dependencies, got: %s", instructions)
	}
	if !strings.Contains(instructions, "BLOCKED") {
		t.Errorf("expected blocked instructions to mention BLOCKED tools, got: %s", instructions)
	}
}

func TestIssueStateInstructions_Opened(t *testing.T) {
	instructions := issueStateInstructions(lifecycle.StateOpened)
	if instructions != "" {
		t.Errorf("expected empty instructions for opened state, got: %s", instructions)
	}
}

func TestBuildRoleRegistry_Reviewer(t *testing.T) {
	cfg := &config.Config{
		Forgejo: config.ForgejoConfig{URL: "http://localhost:3000", Token: "test"},
		Agent:   config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Security: config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/pulls/7", Repository: "org/repo", PRNumber: 7, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	adapter := tool.NewForgejoAdapter("http://localhost:3000", "test")
	sessionInfo := &sessionInfoAdapter{workDir: t.TempDir(), repoDir: t.TempDir()}
	agentCfg := &agentConfigAdapter{cfg: cfg}

	registry := buildRoleRegistry(adapter, nil, sess, sessionInfo, agentCfg, "reviewer", cfg, nil)

	reviewerOnly := []string{"forgejo_merge_pr"}
	implementerOnly := []string{"write_file", "git", "forgejo_create_pr", "forgejo_delete_branch", "forgejo_create_hook", "forgejo_delete_hook", "forgejo_create_token"}

	for _, name := range reviewerOnly {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("reviewer should have %s tool", name)
		}
	}
	for _, name := range implementerOnly {
		if _, ok := registry.Get(name); ok {
			t.Errorf("reviewer should NOT have %s tool", name)
		}
	}
}

func TestBuildRoleRegistry_PM(t *testing.T) {
	cfg := &config.Config{
		Forgejo: config.ForgejoConfig{URL: "http://localhost:3000", Token: "test"},
		Agent:   config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Security: config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	adapter := tool.NewForgejoAdapter("http://localhost:3000", "test")
	sessionInfo := &sessionInfoAdapter{workDir: t.TempDir(), repoDir: t.TempDir()}
	agentCfg := &agentConfigAdapter{cfg: cfg}

	registry := buildRoleRegistry(adapter, nil, sess, sessionInfo, agentCfg, "pm", cfg, nil)

	pmAllowed := []string{"forgejo_comment", "forgejo_create_issue", "bash", "read_file"}
	pmForbidden := []string{"write_file", "git", "forgejo_create_pr", "forgejo_merge_pr", "forgejo_delete_branch"}

	for _, name := range pmAllowed {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("PM should have %s tool", name)
		}
	}
	for _, name := range pmForbidden {
		if _, ok := registry.Get(name); ok {
			t.Errorf("PM should NOT have %s tool", name)
		}
	}
}

func TestBuildRoleRegistry_Implementer(t *testing.T) {
	cfg := &config.Config{
		Forgejo: config.ForgejoConfig{URL: "http://localhost:3000", Token: "test"},
		Agent:   config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Security: config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	adapter := tool.NewForgejoAdapter("http://localhost:3000", "test")
	sessionInfo := &sessionInfoAdapter{workDir: t.TempDir(), repoDir: t.TempDir()}
	agentCfg := &agentConfigAdapter{cfg: cfg}

	registry := buildRoleRegistry(adapter, nil, sess, sessionInfo, agentCfg, "implementer", cfg, nil)

	required := []string{"write_file", "git", "forgejo_create_pr", "forgejo_merge_pr", "forgejo_comment", "forgejo_create_issue", "bash", "read_file"}
	for _, name := range required {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("implementer should have %s tool", name)
		}
	}
}

func TestBuildSystemPrompt_IncludesStateInstructions(t *testing.T) {
	srv := newTestAgentServer(t, []string{"planning", "role:implementer"})
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers:  []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "implementer", nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	prompt := agent.buildSystemPrompt(context.Background(), evt, false, "implementer", lifecycle.StatePlanning)
	if !strings.Contains(prompt, "Planning") {
		t.Error("system prompt should include planning state instructions")
	}
}

func TestBuildSystemPrompt_PRReviewMode(t *testing.T) {
	srv := newTestAgentServer(t, []string{"role:reviewer"})
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers:  []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/pulls/7", Repository: "org/repo", PRNumber: 7, IssueNumber: 7, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "reviewer", nil)

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 7, 7, "alice", "created")
	prompt := agent.buildSystemPrompt(context.Background(), evt, false, "reviewer", lifecycle.StateOpened)
	if !strings.Contains(prompt, "PR Review Mode") {
		t.Error("system prompt should include PR Review Mode for PR comment events")
	}
	if !strings.Contains(prompt, "Do NOT create a new PR") {
		t.Error("PR Review Mode should instruct not to create a new PR")
	}
}

func TestBuildSystemPrompt_AutomergeReviewerPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues/") && !strings.Contains(r.URL.Path, "/comments") && !strings.Contains(r.URL.Path, "/labels") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 7,
				"title":  "Test PR",
				"body":   "Test body",
				"state":  "open",
				"labels": []map[string]string{
					{"name": "automerge"},
					{"name": "role:reviewer"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers:  []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/pulls/7", Repository: "org/repo", PRNumber: 7, IssueNumber: 7, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "reviewer", nil)

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 7, 7, "alice", "created")
	prompt := agent.buildSystemPrompt(context.Background(), evt, false, "reviewer", lifecycle.StateMerging)
	if !strings.Contains(prompt, "automerge") {
		t.Error("reviewer prompt should mention automerge when PR has automerge label")
	}
	if !strings.Contains(prompt, "forgejo_merge_pr") {
		t.Error("automerge reviewer prompt should instruct to call forgejo_merge_pr")
	}
}

func TestTargetDescription(t *testing.T) {
	srv := newTestAgentServer(t, nil)
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers:  []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "implementer", nil)

	if desc := agent.targetDescription(&event.Event{PRNumber: 7}); desc != "Pull Request #7" {
		t.Errorf("expected 'Pull Request #7', got %q", desc)
	}
	if desc := agent.targetDescription(&event.Event{IssueNumber: 42}); desc != "Issue #42" {
		t.Errorf("expected 'Issue #42', got %q", desc)
	}
	if desc := agent.targetDescription(&event.Event{}); desc != "Repository" {
		t.Errorf("expected 'Repository', got %q", desc)
	}
}

func TestIssueStateInstructions_Implementing(t *testing.T) {
	for _, state := range []lifecycle.IssueState{lifecycle.StateImplementing, lifecycle.StateReady, lifecycle.StateReview} {
		instructions := issueStateInstructions(state)
		if instructions != "" {
			t.Errorf("expected empty instructions for %s state, got: %s", state, instructions)
		}
	}
}

func TestIssueStateInstructions_PlanApproved(t *testing.T) {
	instructions := issueStateInstructions(lifecycle.StatePlanApproved)
	if instructions == "" {
		t.Error("expected non-empty instructions for plan-approved state")
	}
	if !strings.Contains(instructions, "Plan Approved") {
		t.Error("expected plan-approved instructions to mention Plan Approved")
	}
}

func TestBuildSystemPrompt_DevOpsRole(t *testing.T) {
	srv := newTestAgentServer(t, nil)
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers:  []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "devops", nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	prompt := agent.buildSystemPrompt(context.Background(), evt, false, "devops", lifecycle.StateOpened)
	if !strings.Contains(prompt, "DevOps") {
		t.Error("devops prompt should include DevOps role instructions")
	}
	if !strings.Contains(prompt, "infrastructure") {
		t.Error("devops prompt should mention infrastructure")
	}
}

func TestBuildSystemPrompt_TesterRole(t *testing.T) {
	srv := newTestAgentServer(t, nil)
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers:  []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "tester", nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	prompt := agent.buildSystemPrompt(context.Background(), evt, false, "tester", lifecycle.StateOpened)
	if !strings.Contains(prompt, "Test Engineer") {
		t.Error("tester prompt should include Test Engineer role instructions")
	}
	if !strings.Contains(prompt, "test quality") {
		t.Error("tester prompt should mention test quality")
	}
}

func TestBuildSystemPrompt_PMRole(t *testing.T) {
	srv := newTestAgentServer(t, nil)
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers:  []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/issues/42", Repository: "org/repo", IssueNumber: 42, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "pm", nil)

	evt := event.NewEvent(event.IssueOpened, "org/repo", 42, 0, "alice", "opened")
	prompt := agent.buildSystemPrompt(context.Background(), evt, false, "pm", lifecycle.StateOpened)
	if !strings.Contains(prompt, "Project Manager") {
		t.Error("pm prompt should include Project Manager role instructions")
	}
	if !strings.Contains(prompt, "Depends on:") {
		t.Error("pm prompt should mention dependency tracking")
	}
}

func TestParseClosingRefs(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []int
	}{
		{"Closes #5", "Closes #5", []int{5}},
		{"closes #5", "closes #5", []int{5}},
		{"Fixes: #5", "Fixes: #5", []int{5}},
		{"fixes #5", "fixes #5", []int{5}},
		{"Resolves #5", "Resolves #5", []int{5}},
		{"Close #5", "Close #5", []int{5}},
		{"Fix #5", "Fix #5", []int{5}},
		{"Resolve #5", "Resolve #5", []int{5}},
		{"closes: #5", "closes: #5", []int{5}},
		{"multiple refs", "Closes #5, fixes #6, resolves #7", []int{5, 6, 7}},
		{"dedup", "Closes #5 and closes #5 again", []int{5}},
		{"no refs", "This is just a description", nil},
		{"in sentence", "This PR closes #42 by doing X", []int{42}},
		{"mixed case", "ClOsEs #10", []int{10}},
		{"with colon and space", "Closes:  #12", []int{12}},
		{"Resolves: #5", "Resolves: #5", []int{5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseClosingRefs(tt.input)
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %v, got %v", tt.expected, got)
			}
			for i, v := range got {
				if v != tt.expected[i] {
					t.Errorf("at index %d: expected %d, got %d", i, tt.expected[i], v)
				}
			}
		})
	}
}

func TestFetchParentContext_NoRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not make any API calls when no closing refs in PR body")
	}))
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/pulls/7", Repository: "org/repo", PRNumber: 7, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "reviewer", nil)

	result := agent.fetchParentContext(context.Background(), "org/repo", "This PR has no closing references")
	if result != "" {
		t.Errorf("expected empty string for no refs, got: %s", result)
	}
}

func TestFetchParentContext_WithRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.Contains(path, "/issues/5") && !strings.Contains(path, "/comments") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 5,
				"title":  "Add authentication",
				"body":   "We need OAuth2 support",
				"state":  "open",
				"labels": []map[string]string{},
			})
			return
		}
		if strings.Contains(path, "/issues/5/comments") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 100, "body": "Use Firebase", "user": map[string]string{"login": "alice"}},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/pulls/7", Repository: "org/repo", PRNumber: 7, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "reviewer", nil)

	result := agent.fetchParentContext(context.Background(), "org/repo", "Closes #5")
	if result == "" {
		t.Fatal("expected non-empty result for PR with closing ref")
	}
	if !strings.Contains(result, "Issue #5") {
		t.Error("result should mention issue #5")
	}
	if !strings.Contains(result, "Add authentication") {
		t.Error("result should include issue title")
	}
	if !strings.Contains(result, "OAuth2") {
		t.Error("result should include issue body")
	}
	if !strings.Contains(result, "Firebase") {
		t.Error("result should include comments")
	}
}

func TestFetchParentContext_APIErrorNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues/99") && !strings.Contains(r.URL.Path, "/comments") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/pulls/7", Repository: "org/repo", PRNumber: 7, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "reviewer", nil)

	result := agent.fetchParentContext(context.Background(), "org/repo", "Closes #99")
	if result != "" && !strings.Contains(result, "Parent Issue Context") {
		t.Error("if any ref exists, should include header even if fetch fails")
	}
}

func TestFetchParentContext_MultipleRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.Contains(path, "/issues/5") && !strings.Contains(path, "/comments") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 5,
				"title":  "Issue five",
				"body":   "Body five",
				"state":  "open",
				"labels": []map[string]string{},
			})
			return
		}
		if strings.Contains(path, "/issues/6") && !strings.Contains(path, "/comments") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 6,
				"title":  "Issue six",
				"body":   "Body six",
				"state":  "open",
				"labels": []map[string]string{},
			})
			return
		}
		if strings.Contains(path, "/comments") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/pulls/7", Repository: "org/repo", PRNumber: 7, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "reviewer", nil)

	result := agent.fetchParentContext(context.Background(), "org/repo", "Closes #5, fixes #6")
	if !strings.Contains(result, "Issue #5") {
		t.Error("should include issue #5")
	}
	if !strings.Contains(result, "Issue #6") {
		t.Error("should include issue #6")
	}
	if !strings.Contains(result, "Issue five") {
		t.Error("should include issue #5 title")
	}
	if !strings.Contains(result, "Issue six") {
		t.Error("should include issue #6 title")
	}
}

func TestBuildContext_IncludesParentContextForPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.Contains(path, "/issues/7") && !strings.Contains(path, "/comments") && !strings.Contains(path, "/labels") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 7,
				"title":  "Test PR issue",
				"body":   "PR description",
				"state":  "open",
				"labels": []map[string]string{},
			})
			return
		}
		if strings.Contains(path, "/issues/7/comments") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		if strings.Contains(path, "/issues/3") && !strings.Contains(path, "/comments") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 3,
				"title":  "Parent requirement",
				"body":   "We need feature X",
				"state":  "open",
				"labels": []map[string]string{},
			})
			return
		}
		if strings.Contains(path, "/issues/3/comments") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 50, "body": "Also consider Y", "user": map[string]string{"login": "bob"}},
			})
			return
		}
		if strings.Contains(path, "/pulls/7") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 7,
				"title":  "PR #7",
				"body":   "Closes #3",
				"state":  "open",
				"head":   map[string]string{"ref": "feature/x"},
				"base":   map[string]string{"ref": "main"},
			})
			return
		}
		if strings.Contains(path, "/labels") {
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	cfg := &config.Config{
		Forgejo:   config.ForgejoConfig{URL: srv.URL, Token: "test"},
		Agent:     config.AgentConfig{MaxSessions: 10, WorkDir: t.TempDir(), IdleTimeout: 1 * time.Hour, MaxTurns: 5, CommitPrefix: "[agent]"},
		Providers: []config.ProviderConfig{{Name: "test", APIBase: "http://localhost/v1", APIKey: "test", Model: "test", MaxTokens: 100}},
		Memory:    config.MemoryConfig{Enabled: false},
		Security:  config.SecurityConfig{},
	}

	sess := &Session{Key: "org/repo/pulls/7", Repository: "org/repo", PRNumber: 7, IssueNumber: 7, WorkDir: t.TempDir(), RepoDir: t.TempDir()}
	agent := NewAgent(cfg, sess, nil, nil, nil, "reviewer", nil)

	evt := event.NewEvent(event.IssueCommentCreated, "org/repo", 7, 7, "alice", "created")
	messages, err := agent.buildContext(context.Background(), evt)
	if err != nil {
		t.Fatalf("buildContext returned error: %v", err)
	}

	foundParentCtx := false
	for _, m := range messages {
		if strings.Contains(m.Content, "Parent Issue Context") && strings.Contains(m.Content, "Parent requirement") {
			foundParentCtx = true
			break
		}
	}
	if !foundParentCtx {
		t.Error("buildContext should include parent issue context for PR with closing ref")
	}
}
