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
	"github.com/fordjent/fordjent/internal/agent"
	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
	"github.com/fordjent/fordjent/internal/scaffold"
	"github.com/fordjent/fordjent/internal/memory"
	"github.com/fordjent/fordjent/internal/mergequeue"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/scheduler"
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
type Manager struct {
	cfg        *config.Config
	bus        *event.Bus
	sessions   map[string]*Session
	mu         sync.RWMutex
	store      *Store
	forgejoClient *forgejo.Client
	lc            *lifecycle.Lifecycle
	mqClient   *mergequeue.Client
	scheduler  *scheduler.Scheduler
	costTracker *cost.Tracker
}

func resolveDBPath(cfgPath, workDir string) string {
	if cfgPath != "" {
		return cfgPath
	}
	return filepath.Join(filepath.Dir(workDir), "sessions.db")
}

func NewManager(cfg *config.Config, bus *event.Bus) (*Manager, error) {
	dbPath := resolveDBPath(cfg.Database.Path, cfg.Agent.WorkDir)
	store, err := NewStore(dbPath)
	if err != nil {
		return nil, err
	}

	// Initialize cost tracker (persisted in work dir)
	costDBPath := ""
	if cfg.Agent.WorkDir != "" {
		costDBPath = filepath.Join(cfg.Agent.WorkDir, "costs.db")
		_ = os.MkdirAll(cfg.Agent.WorkDir, 0755)
	}
	costTracker, err := cost.NewTracker(costDBPath)
	if err != nil {
		return nil, fmt.Errorf("init cost tracker: %w", err)
	}

	forgejoClient := forgejo.NewClient(cfg.Forgejo.URL, cfg.Forgejo.Token)

	// Initialize lifecycle tracker
	lifecycleDBPath := ""
	if cfg.Agent.WorkDir != "" {
		lifecycleDBPath = filepath.Join(cfg.Agent.WorkDir, "lifecycle.db")
	}
	lc, err := lifecycle.New(lifecycleDBPath, forgejoClient)
	if err != nil {
		return nil, fmt.Errorf("init lifecycle tracker: %w", err)
	}

	m := &Manager{
		cfg:           cfg,
		bus:           bus,
		sessions:      make(map[string]*Session),
		store:         store,
		costTracker:   costTracker,
		forgejoClient: forgejoClient,
		lc:            lc,
	}

	// Wire merge queue and scheduler (both need Forgejo API access)
	forgejoAdapter := tool.NewForgejoAdapter(cfg.Forgejo.URL, cfg.Forgejo.Token)
	m.mqClient = mergequeue.NewClient(forgejoAdapter)
	m.scheduler = scheduler.New(forgejoAdapter)

	if err := m.restoreSessions(); err != nil {
		slog.Warn("failed to restore sessions from database", "error", err)
	}

	return m, nil
}

func (m *Manager) restoreSessions() error {
	records, err := m.store.ListAll()
	if err != nil {
		return err
	}
	for _, rec := range records {
		if _, err := os.Stat(rec.WorkDir); err != nil {
			slog.Warn("skipping restored session, workdir missing", "session_key", rec.SessionKey)
			m.store.Delete(rec.SessionKey)
			continue
		}
		sessCtx, cancel := context.WithCancel(context.Background())
		sess := &Session{
			Key:         rec.SessionKey,
			Repository:  rec.Repository,
			IssueNumber: rec.IssueNumber,
			PRNumber:    rec.PRNumber,
			WorkDir:     rec.WorkDir,
			RepoDir:     rec.RepoDir,
			LastActive:  rec.LastActive,
			Cancel:      cancel,
			events:      make(chan *event.Event, 64),
		}
		m.sessions[rec.SessionKey] = sess
		go m.runSession(sessCtx, sess)
		slog.Info("restored session from database", "session_key", rec.SessionKey, "last_active", rec.LastActive)
	}
	return nil
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
	// If a PR was merged, notify the scheduler to unblock dependent issues
	if evt.Type == event.PullRequestMerged && evt.PRNumber > 0 {
		go func() {
			schedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := m.scheduler.OnPRMerged(schedCtx, evt.Repository, evt.PRNumber); err != nil {
				slog.Warn("scheduler: failed to process merged PR", "error", err, "pr", evt.PRNumber, "repo", evt.Repository)
			}
		}()
	}

	// Scaffold detection: on new issues for empty repos, create/block
	if m.cfg.Agent.EnableScaffoldDetection && evt.Type == event.IssueOpened && evt.IssueNumber > 0 {
		blocked, err := scaffold.CheckAndBlock(ctx, m.forgejoClient, evt.Repository, evt.IssueNumber)
		if err != nil {
			slog.Warn("scaffold detection failed", "error", err, "repo", evt.Repository, "issue", evt.IssueNumber)
		}
		if blocked {
			slog.Info("scaffold: blocked issue on empty repo", "repo", evt.Repository, "issue", evt.IssueNumber)
			return
		}
	}

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

	rec := &SessionRecord{
		SessionKey:  sess.Key,
		Repository:  sess.Repository,
		IssueNumber: sess.IssueNumber,
		PRNumber:    sess.PRNumber,
		WorkDir:     sess.WorkDir,
		RepoDir:     sess.RepoDir,
		CreatedAt:   time.Now(),
		LastActive:  time.Now(),
	}
	if err := m.store.Create(rec); err != nil {
		slog.Warn("failed to persist session", "error", err, "session_key", sess.Key)
	}

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
	agent := NewAgent(m.cfg, sess, m.mqClient, m.costTracker)

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

			// Only record start if this is the first event in the session
			if state, _ := m.lc.GetState(ctx, sess.Key); state == "" {
				m.lc.OnSessionStart(ctx, sess.Key)
			}

			if err := agent.ProcessEvent(ctx, evt); err != nil {
				if strings.Contains(err.Error(), "max turns") {
					m.lc.OnSessionFailedMaxTurns(ctx, evt.Repository, evt.IssueNumber, sess.Key)
				} else {
					slog.Error("agent processing failed",
						"error", err,
						"event_id", evt.ID,
						"session_key", sess.Key,
					)
					m.lc.OnSessionFailedError(ctx, evt.Repository, evt.IssueNumber, sess.Key, err)
				}
			} else {
				m.lc.OnSessionComplete(ctx, sess.Key)
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
			m.store.Delete(key)
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
		m.store.Delete(oldestKey)
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
	cfg         *config.Config
	sess        *Session
	forgejo     *forgejo.Client
	llm         *provider.Client
	tools       *tool.Registry
	mem         *memory.Memory
	costTracker *cost.Tracker
	executor    *agent.TurnExecutor
}

func NewAgent(cfg *config.Config, sess *Session, mq *mergequeue.Client, ct *cost.Tracker) *Agent {
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
	registry.Register(tool.NewCreatePRTool(forgejoAdapter, mq, sess.RepoDir))
	registry.Register(tool.NewSearchCodeTool(forgejoAdapter))
	registry.Register(tool.NewAddReactionTool(forgejoAdapter))
	registry.Register(tool.NewMergePRTool(forgejoAdapter))
	registry.Register(tool.NewBashTool(sessionInfo))
	registry.Register(tool.NewReadFileTool(sessionInfo))
	registry.Register(tool.NewWriteFileTool(sessionInfo, agentCfg))
	registry.Register(tool.NewGitTool(sessionInfo))

	executor := agent.NewTurnExecutor(cfg, llmClient, registry, ct, sess.Key, sess.Repository)

	return &Agent{
		cfg:         cfg,
		sess:        sess,
		forgejo:     forgejoClient,
		llm:         llmClient,
		tools:       registry,
		mem:         mem,
		costTracker: ct,
		executor:    executor,
	}
}

// ProcessEvent handles a single event: builds context, runs LLM loop with compaction/retry/cost tracking, executes tools.
func (a *Agent) ProcessEvent(ctx context.Context, evt *event.Event) error {
	// Step 1: Acknowledge with 👀 reaction
	a.addReaction(ctx, evt, "eyes")

	// Step 2: Build context for the LLM
	systemPrompt := a.buildSystemPrompt(evt)
	contextMessages, err := a.buildContext(ctx, evt)
	if err != nil {
		slog.Warn("failed to build full context", "error", err)
	}

	// Step 3: If this is a PR review comment, fetch PR and checkout its branch
	if evt.PRNumber > 0 && (evt.Type == event.IssueCommentCreated || evt.Type == event.PullRequestReviewComment) {
		pr, err := a.forgejo.GetPR(ctx, evt.Repository, evt.PRNumber)
		if err == nil && pr.Head.Ref != "" {
			repoDir := a.sess.RepoDir
			fetchCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "origin", pr.Head.Ref)
			if _, err := fetchCmd.CombinedOutput(); err != nil {
				slog.Warn("failed to fetch PR branch", "branch", pr.Head.Ref, "error", err)
			}
			checkoutCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "checkout", "-B", pr.Head.Ref, "origin/"+pr.Head.Ref)
			if _, err := checkoutCmd.CombinedOutput(); err != nil {
				slog.Warn("failed to checkout PR branch", "branch", pr.Head.Ref, "error", err)
			}
			slog.Info("checked out PR branch for review", "branch", pr.Head.Ref, "session_key", a.sess.Key)
			contextMessages = append(contextMessages, provider.Message{
				Role: "user",
				Content: fmt.Sprintf("[Context] Responding to review on PR #%d '%s'. You are now on branch '%s'. Make changes on this branch, commit, and push to it. Do NOT create a new PR.",
					pr.Number, pr.Title, pr.Head.Ref),
			})
		} else if err != nil {
			slog.Warn("failed to get PR details", "pr", evt.PRNumber, "error", err)
		}
	}

	// Step 4: Build the user message from the event
	userMessage := a.eventToUserMessage(evt)

	// Step 5: LLM loop (max turns)
	messages := append(contextMessages, provider.Message{
		Role:    "user",
		Content: userMessage,
	})

	// Update reaction to ⏳
	a.addReaction(ctx, evt, "hourglass_flowing_sand")

	for turn := 0; turn < a.cfg.Agent.MaxTurns; turn++ {
		slog.Info("LLM turn begin",
			"session_key", a.sess.Key,
			"turn", turn,
			"messages", len(messages),
		)

		result, updatedMessages, err := a.executor.Run(ctx, systemPrompt, messages)
		messages = updatedMessages

		if err != nil {
			slog.Error("LLM turn failed", "session_key", a.sess.Key, "turn", turn, "error", err)
			a.addReaction(ctx, evt, "x")
			return fmt.Errorf("turn %d failed: %w", turn, err)
		}

		// Track metrics
		metrics.IncLLMCalls()
		if result.Usage != nil {
			metrics.AddTokens(int64(result.Usage.PromptTokens), int64(result.Usage.CompletionTokens))
		}
		if result.CostUSD > 0 {
			metrics.AddCost(result.CostUSD)
		}

		// If no tool calls, we're done
		if len(result.Response.ToolCalls) == 0 {
			messages = append(messages, provider.Message{
				Role:    "assistant",
				Content: result.Response.Content,
			})

			if a.cfg.Memory.Enabled {
				a.mem.Record(ctx, evt, result.Response.Content, turn)
			}

			a.addReaction(ctx, evt, "white_check_mark")
			return nil
		}

		// Add assistant message with tool calls
		messages = append(messages, provider.Message{
			Role:      "assistant",
			Content:   result.Response.Content,
			ToolCalls: result.Response.ToolCalls,
		})

		// Execute tool calls
		for _, tc := range result.Response.ToolCalls {
			slog.Info("executing tool",
				"tool", tc.Function.Name,
				"session_key", a.sess.Key,
			)

			metrics.IncToolCalls()

			res, terr := a.tools.Execute(ctx, tc.Function.Name, tc.Function.Arguments)
			if terr != nil {
				slog.Error("tool execution failed", "tool", tc.Function.Name, "error", terr)
				res = fmt.Sprintf("Error: %s", terr)
			}

			if a.cfg.Memory.Enabled {
				a.mem.RecordToolCall(ctx, evt, tc.Function.Name, tc.Function.Arguments, res)
			}

			messages = append(messages, provider.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    res,
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

	var modeInstructions string
	if evt.PRNumber > 0 && (evt.Type == event.IssueCommentCreated || evt.Type == event.PullRequestReviewComment) {
		modeInstructions = `
## PR Review Mode (IMPORTANT)
You are responding to a review comment on an existing pull request.
- You are already on the PR branch (check git status if unsure).
- Make your fixes directly on this branch.
- After fixing, commit and push to the SAME branch.
- Do NOT create a new PR — the PR already exists.
- Post a comment confirming which issues were fixed.
- If the PR is mergeable with no conflicts, you may call forgejo_merge_pr to merge it automatically.`
	} else if evt.PRNumber > 0 {
		modeInstructions = `
## PR Context
You are working on a pull request. Create a feature branch, implement the changes, push the branch, and then use forgejo_create_pr to open the PR.`
	}

	return fmt.Sprintf(`You are Fordjent, an autonomous coding agent that helps with software development tasks on a Forgejo instance.

## Current Context
- Repository: %s
- Event: %s (action: %s)
- Sender: @%s
- Target: %s
%s

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
8. **ALWAYS rebase before creating a PR.** Before calling forgejo_create_pr, first run 'git fetch origin' and then 'git rebase origin/main' on your feature branch using the git tool (two separate calls) or the bash tool (combined). This prevents merge conflicts.
9. **Do NOT create a new PR if one already exists** for the current branch. Push to the existing branch instead.

## Response Format
Respond in plain text. Use tools to interact with the repository and Forgejo API.`,
		evt.Repository,
		evt.Type,
		evt.Action,
		evt.Sender,
		a.targetDescription(evt),
		modeInstructions,
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
