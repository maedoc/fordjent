package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/memory"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/tool"
)

// Session represents an active agent session bound to a session key.
type Session struct {
	Key         string
	Repository  string
	IssueNumber int
	PRNumber    int
	WorkDir     string
	RepoDir     string
	LastActive  time.Time
	Cancel      context.CancelFunc

	mu     sync.Mutex
	busy   bool
	events chan *event.Event
}

// sessionInfoAdapter adapts Session to tool.SessionInfo
type sessionInfoAdapter struct {
	workDir string
	repoDir string
}

func (s *sessionInfoAdapter) WorkDir() string { return s.workDir }
func (s *sessionInfoAdapter) RepoDir() string  { return s.repoDir }

// agentConfigAdapter adapts Config to tool.AgentConfig
type agentConfigAdapter struct {
	cfg *config.Config
}

func (a *agentConfigAdapter) CommitPrefix() string       { return a.cfg.Agent.CommitPrefix }
func (a *agentConfigAdapter) ProtectedBranches() []string { return a.cfg.Security.ProtectedBranches }
func (a *agentConfigAdapter) RequirePRForWorkflows() bool { return a.cfg.Security.RequirePRForWorkflows }

// Manager manages agent session lifecycle.
// Events with the same session key are routed to the same agent session.
// Concurrent events queue and are processed serially.
type Manager struct {
	cfg      *config.Config
	bus      *event.Bus
	sessions map[string]*Session
	mu       sync.RWMutex
}

func NewManager(cfg *config.Config, bus *event.Bus) *Manager {
	return &Manager{
		cfg:      cfg,
		bus:      bus,
		sessions: make(map[string]*Session),
	}
}

// Run starts the session manager event loop.
func (m *Manager) Run(ctx context.Context) {
	sub := m.bus.Subscribe()
	defer m.bus.Unsubscribe(sub)

	slog.Info("session manager started", "max_sessions", m.cfg.Agent.MaxSessions)

	reaperTicker := time.NewTicker(1 * time.Minute)
	defer reaperTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.shutdownAll()
			return

		case evt, ok := <-sub:
			if !ok {
				return
			}
			m.handleEvent(ctx, evt)

		case <-reaperTicker.C:
			m.reapIdle(ctx)
		}
	}
}

func (m *Manager) handleEvent(ctx context.Context, evt *event.Event) {
	if evt.SessionKey == "" {
		slog.Warn("event with empty session key, dropping", "event_id", evt.ID)
		return
	}

	sess, err := m.getOrCreate(ctx, evt)
	if err != nil {
		slog.Error("failed to create session", "error", err, "session_key", evt.SessionKey)
		return
	}

	select {
	case sess.events <- evt:
		slog.Info("queued event for session",
			"event_id", evt.ID,
			"session_key", sess.Key,
		)
	default:
		slog.Warn("session event queue full, dropping event",
			"event_id", evt.ID,
			"session_key", sess.Key,
		)
	}
}

func buildCloneURL(baseURL, token, repo string) string {
	if token == "" {
		return fmt.Sprintf("%s/%s.git", baseURL, repo)
	}
	if strings.HasPrefix(baseURL, "https://") {
		return fmt.Sprintf("https://%s@%s/%s.git", token, strings.TrimPrefix(baseURL, "https://"), repo)
	}
	if strings.HasPrefix(baseURL, "http://") {
		return fmt.Sprintf("http://%s@%s/%s.git", token, strings.TrimPrefix(baseURL, "http://"), repo)
	}
	return fmt.Sprintf("%s/%s.git", baseURL, repo)
}

func (m *Manager) getOrCreate(ctx context.Context, evt *event.Event) (*Session, error) {
	m.mu.RLock()
	sess, exists := m.sessions[evt.SessionKey]
	m.mu.RUnlock()

	if exists {
		sess.LastActive = time.Now()
		return sess, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if sess, exists = m.sessions[evt.SessionKey]; exists {
		sess.LastActive = time.Now()
		return sess, nil
	}

	if len(m.sessions) >= m.cfg.Agent.MaxSessions {
		m.evictOldest()
		if len(m.sessions) >= m.cfg.Agent.MaxSessions {
			return nil, fmt.Errorf("max sessions (%d) reached", m.cfg.Agent.MaxSessions)
		}
	}

	// Create work directory
	workDir := filepath.Join(m.cfg.Agent.WorkDir, evt.SessionKey)
	repoDir := filepath.Join(workDir, "repo")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}

	// Clone the repository if not already cloned
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		cloneURL := buildCloneURL(m.cfg.Forgejo.URL, m.cfg.Forgejo.Token, evt.Repository)
		slog.Info("cloning repository", "url", cloneURL, "dir", repoDir)
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "50", cloneURL, repoDir)
		cmd.Env = append(os.Environ(),
			fmt.Sprintf("GIT_TERMINAL_PROMPT=0"),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("git clone failed (will use API-only mode)",
				"error", err, "output", string(out))
		}
	}

	sessCtx, cancel := context.WithCancel(context.Background())
	sess = &Session{
		Key:         evt.SessionKey,
		Repository:  evt.Repository,
		IssueNumber: evt.IssueNumber,
		PRNumber:    evt.PRNumber,
		WorkDir:     workDir,
		RepoDir:     repoDir,
		LastActive:  time.Now(),
		Cancel:      cancel,
		events:      make(chan *event.Event, 64),
	}

	m.sessions[evt.SessionKey] = sess

	metrics.IncSessions()
	metrics.SetActiveSessions(int64(len(m.sessions)))

	go m.runSession(sessCtx, sess)

	slog.Info("created new session",
		"session_key", sess.Key,
		"repository", sess.Repository,
		"work_dir", sess.WorkDir,
	)

	return sess, nil
}

// runSession is the per-session event loop. It processes events serially.
func (m *Manager) runSession(ctx context.Context, sess *Session) {
	agent := NewAgent(m.cfg, sess)

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-sess.events:
			if !ok {
				return
			}

			sess.mu.Lock()
			sess.busy = true
			sess.mu.Unlock()

			slog.Info("processing event in session",
				"event_id", evt.ID,
				"session_key", sess.Key,
				"type", evt.Type,
			)

			if err := agent.ProcessEvent(ctx, evt); err != nil {
				slog.Error("agent processing failed",
					"error", err,
					"event_id", evt.ID,
					"session_key", sess.Key,
				)
			}

			sess.mu.Lock()
			sess.busy = false
			sess.LastActive = time.Now()
			sess.mu.Unlock()
		}
	}
}

func (m *Manager) reapIdle(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, sess := range m.sessions {
		if time.Since(sess.LastActive) > m.cfg.Agent.IdleTimeout {
			sess.mu.Lock()
			busy := sess.busy
			sess.mu.Unlock()
			if busy {
				continue
			}
			slog.Info("reaping idle session", "session_key", key)
			sess.Cancel()
			delete(m.sessions, key)
			metrics.SetActiveSessions(int64(len(m.sessions)))
		}
	}
}

func (m *Manager) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for key, sess := range m.sessions {
		sess.mu.Lock()
		busy := sess.busy
		sess.mu.Unlock()
		if busy {
			continue
		}
		if oldestKey == "" || sess.LastActive.Before(oldestTime) {
			oldestKey = key
			oldestTime = sess.LastActive
		}
	}

	if oldestKey != "" {
		slog.Info("evicting oldest session for capacity", "session_key", oldestKey)
		if sess, ok := m.sessions[oldestKey]; ok {
			sess.Cancel()
		}
		delete(m.sessions, oldestKey)
		metrics.SetActiveSessions(int64(len(m.sessions)))
	}
}

func (m *Manager) shutdownAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, sess := range m.sessions {
		slog.Info("shutting down session", "session_key", key)
		sess.Cancel()
		delete(m.sessions, key)
	}
}

// Agent is the per-session agent that processes events via LLM + tools.
type Agent struct {
	cfg     *config.Config
	sess    *Session
	forgejo *forgejo.Client
	llm     *provider.Client
	tools   *tool.Registry
	mem     *memory.Memory
}

func NewAgent(cfg *config.Config, sess *Session) *Agent {
	forgejoClient := forgejo.NewClient(cfg.Forgejo.URL, cfg.Forgejo.Token)
	prov := cfg.DefaultProvider()
	llmClient := provider.NewClient(prov)
	mem := memory.New(cfg, sess.WorkDir, forgejoClient)

	registry := tool.NewRegistry()
	sessionInfo := &sessionInfoAdapter{workDir: sess.WorkDir, repoDir: sess.RepoDir}
	forgejoAdapter := tool.NewForgejoAdapter(cfg.Forgejo.URL, cfg.Forgejo.Token)
	agentCfg := &agentConfigAdapter{cfg: cfg}

	// Register tools
	registry.Register(tool.NewCommentTool(forgejoAdapter))
	registry.Register(tool.NewListIssuesTool(forgejoAdapter))
	registry.Register(tool.NewGetIssueTool(forgejoAdapter))
	registry.Register(tool.NewCreatePRTool(forgejoAdapter))
	registry.Register(tool.NewSearchCodeTool(forgejoAdapter))
	registry.Register(tool.NewAddReactionTool(forgejoAdapter))
	registry.Register(tool.NewBashTool(sessionInfo))
	registry.Register(tool.NewReadFileTool(sessionInfo))
	registry.Register(tool.NewWriteFileTool(sessionInfo, agentCfg))
	registry.Register(tool.NewGitTool(sessionInfo))

	return &Agent{
		cfg:     cfg,
		sess:    sess,
		forgejo: forgejoClient,
		llm:     llmClient,
		tools:   registry,
		mem:     mem,
	}
}

// ProcessEvent handles a single event: builds context, runs LLM loop, executes tools.
func (a *Agent) ProcessEvent(ctx context.Context, evt *event.Event) error {
	// Step 1: Acknowledge with 👀 reaction
	a.addReaction(ctx, evt, "eyes")

	// Step 2: Build context for the LLM
	systemPrompt := a.buildSystemPrompt(evt)
	contextMessages, err := a.buildContext(ctx, evt)
	if err != nil {
		slog.Warn("failed to build full context", "error", err)
	}

	// Step 3: Build the user message from the event
	userMessage := a.eventToUserMessage(evt)

	// Step 4: LLM loop (max turns)
	messages := append(contextMessages, provider.Message{
		Role:    "user",
		Content: userMessage,
	})

	// Update reaction to ⏳
	a.addReaction(ctx, evt, "hourglass_flowing_sand")

	metrics.IncLLMCalls()

	for turn := 0; turn < a.cfg.Agent.MaxTurns; turn++ {
		slog.Info("LLM turn",
			"session_key", a.sess.Key,
			"turn", turn,
			"messages", len(messages),
		)

		response, err := a.llm.Chat(ctx, systemPrompt, messages, a.tools.Tools())
		if err != nil {
			a.addReaction(ctx, evt, "x")
			return fmt.Errorf("LLM call failed: %w", err)
		}

		// If no tool calls, we're done
		if len(response.ToolCalls) == 0 {
			messages = append(messages, provider.Message{
				Role:    "assistant",
				Content: response.Content,
			})

			// Record to memory
			if a.cfg.Memory.Enabled {
				a.mem.Record(ctx, evt, response.Content, turn)
			}

			// Mark complete with ✅
			a.addReaction(ctx, evt, "white_check_mark")
			return nil
		}

		// Add assistant message with tool calls
		messages = append(messages, provider.Message{
			Role:      "assistant",
			Content:   response.Content,
			ToolCalls: response.ToolCalls,
		})

		// Execute tool calls
		for _, tc := range response.ToolCalls {
			slog.Info("executing tool",
				"tool", tc.Function.Name,
				"session_key", a.sess.Key,
			)

			metrics.IncToolCalls()

			result, err := a.tools.Execute(ctx, tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				slog.Error("tool execution failed", "tool", tc.Function.Name, "error", err)
				result = fmt.Sprintf("Error: %s", err)
			}

			if a.cfg.Memory.Enabled {
				a.mem.RecordToolCall(ctx, evt, tc.Function.Name, tc.Function.Arguments, result)
			}

			messages = append(messages, provider.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}

	slog.Warn("max turns reached", "session_key", a.sess.Key)
	a.addReaction(ctx, evt, "warning")
	return fmt.Errorf("max turns (%d) reached", a.cfg.Agent.MaxTurns)
}

func (a *Agent) addReaction(ctx context.Context, evt *event.Event, emoji string) {
	commentID := 0
	if raw, ok := evt.Payload["comment"].(map[string]interface{}); ok {
		if id, ok := raw["id"].(float64); ok {
			commentID = int(id)
		}
	}

	if err := a.forgejo.AddReaction(ctx, evt.Repository, evt.IssueNumber, commentID, emoji); err != nil {
		slog.Debug("failed to add reaction", "emoji", emoji, "error", err)
	}
}

func (a *Agent) buildSystemPrompt(evt *event.Event) string {
	toolsDesc := a.tools.Descriptions()

	return fmt.Sprintf(`You are Fordjent, an autonomous coding agent that helps with software development tasks on a Forgejo instance.

## Current Context
- Repository: %s
- Event: %s (action: %s)
- Sender: @%s
- Target: %s

## Your Capabilities
You have access to the following tools:
%s

## Rules
1. Always read existing code before making changes.
2. Make minimal, focused changes.
3. All commit messages must start with "%s".
4. NEVER push directly to protected branches (%s). Create a feature branch and PR instead.
5. Workflow file changes (.forgejo/workflows/) MUST go through PRs.
6. When done, post a summary comment on the issue/PR.
7. Be helpful, concise, and correct.

## Response Format
Respond in plain text. Use tools to interact with the repository and Forgejo API.`,
		evt.Repository,
		evt.Type,
		evt.Action,
		evt.Sender,
		a.targetDescription(evt),
		toolsDesc,
		a.cfg.Agent.CommitPrefix,
		strings.Join(a.cfg.Security.ProtectedBranches, ", "),
	)
}

func (a *Agent) targetDescription(evt *event.Event) string {
	if evt.PRNumber > 0 {
		return fmt.Sprintf("Pull Request #%d", evt.PRNumber)
	}
	if evt.IssueNumber > 0 {
		return fmt.Sprintf("Issue #%d", evt.IssueNumber)
	}
	return "Repository"
}

func (a *Agent) buildContext(ctx context.Context, evt *event.Event) ([]provider.Message, error) {
	var messages []provider.Message

	if evt.IssueNumber > 0 {
		issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
		if err == nil && issue != nil {
			messages = append(messages, provider.Message{
				Role:    "user",
				Content: fmt.Sprintf("[Context] Issue #%d: %s\n\n%s", evt.IssueNumber, issue.Title, issue.Body),
			})
		}

		comments, err := a.forgejo.ListComments(ctx, evt.Repository, evt.IssueNumber)
		if err == nil {
			for _, c := range comments {
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: fmt.Sprintf("[Comment by @%s] %s", c.User, c.Body),
				})
			}
		}
	}

	if a.cfg.Memory.Enabled {
		summary, err := a.mem.Query(ctx, evt)
		if err == nil && summary != "" {
			messages = append(messages, provider.Message{
				Role:    "user",
				Content: fmt.Sprintf("[Previous Agent Context]\n%s", summary),
			})
		}
	}

	return messages, nil
}

func (a *Agent) eventToUserMessage(evt *event.Event) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("New event: %s (action: %s) from @%s\n", evt.Type, evt.Action, evt.Sender))

	switch evt.Type {
	case event.IssueCommentCreated, event.PullRequestReviewComment:
		if comment, ok := evt.Payload["comment"].(map[string]interface{}); ok {
			if body, ok := comment["body"].(string); ok {
				sb.WriteString(fmt.Sprintf("\nComment:\n%s\n", body))
			}
		}
	case event.IssueOpened:
		if issue, ok := evt.Payload["issue"].(map[string]interface{}); ok {
			if body, ok := issue["body"].(string); ok {
				sb.WriteString(fmt.Sprintf("\nIssue body:\n%s\n", body))
			}
		}
	case event.PullRequestOpened, event.PullRequestSync:
		if pr, ok := evt.Payload["pull_request"].(map[string]interface{}); ok {
			if body, ok := pr["body"].(string); ok {
				sb.WriteString(fmt.Sprintf("\nPR body:\n%s\n", body))
			}
		}
	}

	// Include full payload as JSON for detailed context
	payloadJSON, err := json.MarshalIndent(evt.Payload, "", "  ")
	if err == nil && len(payloadJSON) < 5000 {
		sb.WriteString(fmt.Sprintf("\n<details>\n<summary>Full payload</summary>\n\n```json\n%s\n```\n</details>", string(payloadJSON)))
	}

	return sb.String()
}
