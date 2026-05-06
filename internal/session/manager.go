package session

import (
	"context"
	"errors"
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
	"github.com/fordjent/fordjent/internal/mergequeue"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/scheduler"
	"github.com/fordjent/fordjent/internal/sentinel"
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
	Sender      string // original webhook sender (e.g. fordjent-bot)

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
	cfg           *config.Config
	bus           *event.Bus
	sessions      map[string]*Session
	mu            sync.RWMutex
	store         *Store
	forgejoClient *forgejo.Client
	lc            *lifecycle.Lifecycle
	mqClient      *mergequeue.Client
	scheduler     *scheduler.Scheduler
	costTracker   *cost.Tracker
	labelBoot     sync.Map // repo → bool, tracks which repos have had labels ensured
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
	lc, err := lifecycle.New(lifecycleDBPath, forgejoClient, costTracker)
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
		// Skip completed/failed sessions — no need to restore idle goroutines
		state, _ := m.lc.GetState(context.Background(), rec.SessionKey)
		if state == lifecycle.StateCompleted || strings.HasPrefix(state, "failed") {
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

		// Auto-resume recently-active implementer sessions by posting a synthetic comment
		if m.cfg.Agent.EnableSessionRecovery && time.Since(rec.LastActive) < 2*time.Hour && rec.IssueNumber > 0 {
			go func(repo string, issueNum int) {
				resumeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				issue, err := m.forgejoClient.GetIssue(resumeCtx, repo, issueNum)
				if err == nil && issue != nil && detectRoleFromIssue(issue) == "pm" {
					return // Do not nudge PM sessions
				}
				body := "Resuming work after agent restart..."
				if err := m.forgejoClient.PostIssueComment(resumeCtx, repo, issueNum, body); err != nil {
					slog.Warn("failed to post resume comment", "error", err, "issue", issueNum)
				}
			}(rec.Repository, rec.IssueNumber)
		}
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

	cleanupTicker := time.NewTicker(1 * time.Hour)
	defer cleanupTicker.Stop()

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

		case <-cleanupTicker.C:
			m.cleanupOldWorkDirs(ctx)
		}
	}
}

func (m *Manager) handleEvent(ctx context.Context, evt *event.Event) {
	// Bootstrap scheduler/lifecycle/scaffold labels once per repo (sync to avoid races)
	if _, ok := m.labelBoot.Load(evt.Repository); !ok {
		m.labelBoot.Store(evt.Repository, true)
		lbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := m.forgejoClient.EnsureLabels(lbCtx, evt.Repository); err != nil {
			slog.Warn("failed to ensure repo labels", "repo", evt.Repository, "error", err)
		}
		cancel()
	}

	// If a PR was merged, notify the scheduler to unblock dependent issues
	if evt.Type == event.PullRequestMerged && evt.PRNumber > 0 {
		go func() {
			schedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := m.scheduler.OnPRMerged(schedCtx, evt.Repository, evt.PRNumber); err != nil {
				slog.Warn("scheduler: failed to process merged PR", "error", err, "pr", evt.PRNumber, "repo", evt.Repository)
			}
			// Auto-requeue any blocked branches whose merge-gate may now be clear.
			if m.mqClient != nil && m.lc != nil {
				time.Sleep(2 * time.Second) // let Forgejo update file indices
				blocked, err := m.lc.ListBlockedBranches(schedCtx, evt.Repository)
				if err != nil {
					slog.Warn("lifecycle: failed to list blocked branches", "error", err)
					return
				}
				for _, b := range blocked {
					cleared, msg, err := m.mqClient.CheckGate(schedCtx, evt.Repository, b.Branch, "main")
					if err != nil {
						slog.Warn("merge queue: re-check failed", "error", err, "branch", b.Branch)
						continue
					}
					if cleared {
						slog.Info("merge gate cleared for blocked branch, posting unblock nudge", "branch", b.Branch, "issue", b.IssueNumber)
						if m.forgejoClient != nil {
							body := "The merge gate is now clear after conflicting PRs were merged. You may retry creating the PR."
							_ = m.forgejoClient.PostIssueComment(schedCtx, evt.Repository, b.IssueNumber, body)
						}
						_ = m.lc.ResolveBlockedBranch(schedCtx, evt.Repository, b.Branch)
					} else {
						slog.Info("merge gate still blocked for branch", "branch", b.Branch, "reason", msg)
					}
				}
			}
		}()
	}

	// If code was pushed directly to main (e.g., scaffold), close scaffold issues and unblock dependents
	if evt.Type == event.Push {
		if ref, ok := evt.Payload["ref"].(string); ok && ref == "refs/heads/main" {
			go func() {
				schedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				// Close any open scaffold issues now that main has content
				issues, err := m.forgejoClient.ListOpenIssues(schedCtx, evt.Repository)
				if err != nil {
					slog.Warn("push handler: failed to list issues", "error", err, "repo", evt.Repository)
				} else {
					for _, issue := range issues {
						if strings.HasPrefix(issue.Title, "[scaffold]") {
							if err := m.forgejoClient.CloseIssue(schedCtx, evt.Repository, issue.Number); err != nil {
								slog.Warn("push handler: failed to close scaffold issue", "error", err, "issue", issue.Number)
							} else {
								slog.Info("push handler: closed scaffold issue", "issue", issue.Number, "repo", evt.Repository)
							}
						}
					}
				}

				// Now unblock dependent issues
				if err := m.scheduler.CheckAndUnblock(schedCtx, evt.Repository); err != nil {
					slog.Warn("scheduler: failed to unblock after push", "error", err, "repo", evt.Repository)
				}
			}()
		}
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

	// Session recovery: if a scheduler unblocks an issue whose session died, re-trigger.
	if m.cfg.Agent.EnableSessionRecovery && evt.Type == event.IssueCommentCreated && evt.IssueNumber > 0 {
		body := ""
		if comment, ok := evt.Payload["comment"].(map[string]interface{}); ok {
			if b, ok := comment["body"].(string); ok {
				body = b
			}
		}
		if strings.Contains(body, "is now merged") && strings.Contains(body, "unblocked") {
			slog.Info("scheduler unblock comment detected, recovery session will be created", "session_key", evt.SessionKey, "issue", evt.IssueNumber)
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

	// Ensure refs are current. Critical for PR review sessions and when
	// main was updated since the last session.
	fetchCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "origin")
	fetchCmd.Env = append(os.Environ(), fmt.Sprintf("GIT_TERMINAL_PROMPT=0"))
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		slog.Warn("git fetch failed", "error", err, "output", string(out), "repoDir", repoDir)
	} else {
		slog.Debug("git fetch completed", "repoDir", repoDir)
	}

	// Auto-elevate bot permissions so subsequent label/branch/PR operations succeed.
	// NOTE: The bot cannot add itself as a collaborator — this requires owner action.
	// We log a one-time warning with instructions instead.
	if m.cfg.Agent.EnableAutoCollaborator {
		_, loaded := m.labelBoot.LoadOrStore("collab-warn-"+evt.Repository, true)
		if !loaded {
			slog.Warn("bot may lack write access — add fordjent-bot as admin collaborator manually",
				"repo", evt.Repository,
				"instruction", "Forgejo Admin: Settings → Collaborators → Add fordjent-bot with Admin permission")
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
		Sender:      evt.Sender,
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
	// Detect role from issue or PR title/labels before agent construction
	role := detectRoleFromSession(ctx, m.forgejoClient, sess)
	// If a bot created this PR, auto-assign reviewer role to inspect and merge it
	if sess.PRNumber > 0 {
		isBotPR := sess.Sender == "fordjent-bot" || sess.Sender == "fordjent[bot]"
		if !isBotPR {
			// Check PR author for restored sessions where Sender is not set
			pr, err := m.forgejoClient.GetPR(ctx, sess.Repository, sess.PRNumber)
			if err == nil && pr != nil && pr.User != nil {
				login := strings.ToLower(pr.User.Login)
				if login == "fordjent-bot" || login == "fordjent[bot]" {
					isBotPR = true
				}
			}
		}
		if isBotPR && (role == "implementer" || role == "") {
			role = "reviewer"
		}
	}
	agt := NewAgent(m.cfg, sess, m.mqClient, m.costTracker, m.lc, role)

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

			if err := agt.ProcessEvent(ctx, evt); err != nil {
				if errors.Is(err, sentinel.ErrBlocked) {
					slog.Info("session blocked by merge queue", "session_key", sess.Key)
					branch := ""
					if sess.RepoDir != "" {
						branchCmd := exec.CommandContext(ctx, "git", "-C", sess.RepoDir, "rev-parse", "--abbrev-ref", "HEAD")
						if out, bErr := branchCmd.CombinedOutput(); bErr == nil {
							branch = strings.TrimSpace(string(out))
						}
					}
					m.lc.OnSessionBlocked(ctx, evt.Repository, evt.IssueNumber, sess.Key, branch)
				} else if errors.Is(err, agent.ErrMaxTurnsReached) {
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
				m.lc.OnSessionComplete(ctx, sess.Key, evt.Repository, evt.IssueNumber)
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

// cleanupOldWorkDirs removes work directories for sessions that have been
// completed or failed for more than 7 days, freeing disk space.
func (m *Manager) cleanupOldWorkDirs(ctx context.Context) {
	if m.cfg.Agent.WorkDir == "" {
		return
	}
	const maxAge = 7 * 24 * time.Hour

	records, err := m.store.ListAll()
	if err != nil {
		return
	}
	for _, rec := range records {
		// Only clean up old completed/failed sessions
		state, _ := m.lc.GetState(ctx, rec.SessionKey)
		if state != lifecycle.StateCompleted && !strings.HasPrefix(state, "failed") {
			continue
		}
		if time.Since(rec.LastActive) < maxAge {
			continue
		}
		if _, err := os.Stat(rec.WorkDir); err != nil {
			continue // already gone
		}
		if err := os.RemoveAll(rec.WorkDir); err != nil {
			slog.Warn("failed to clean up old workdir", "error", err, "dir", rec.WorkDir)
		} else {
			slog.Info("cleaned up old workdir", "session_key", rec.SessionKey, "dir", rec.WorkDir)
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

// Drain waits up to the context deadline for active sessions to finish
// their current turn before hard-cancelling. Call after Run() returns.
func (m *Manager) Drain(ctx context.Context) {
	for {
		m.mu.Lock()
		active := 0
		for _, s := range m.sessions {
			if s.busy {
				active++
			}
		}
		m.mu.Unlock()

		if active == 0 {
			slog.Info("all sessions drained")
			return
		}

		slog.Info("draining sessions", "active", active)
		select {
		case <-ctx.Done():
			slog.Warn("drain timeout exceeded, forcing shutdown", "remaining", active)
			m.shutdownAll()
			return
		case <-time.After(1 * time.Second):
		}
	}
}

// CleanSessions wipes all persistent session records from the database.
// Does not affect in-memory sessions — call before Run().
func (m *Manager) CleanSessions(_ context.Context) error {
	return m.store.DeleteAll()
}


// detectRoleFromSession inspects the issue or PR associated with this session
// and returns a role label (pm, reviewer, devops, tester, implementer).
func detectRoleFromSession(ctx context.Context, client *forgejo.Client, sess *Session) string {
	if sess.IssueNumber > 0 {
		issue, err := client.GetIssue(ctx, sess.Repository, sess.IssueNumber)
		if err == nil && issue != nil {
			role := detectRoleFromIssue(issue)
			if role != "" {
				return role
			}
		}
	}
	if sess.PRNumber > 0 {
		pr, err := client.GetPR(ctx, sess.Repository, sess.PRNumber)
		if err == nil && pr != nil {
			role := detectRoleFromTitle(pr.Title)
			if role != "" {
				return role
			}
		}
	}
	return "implementer"
}

func detectRoleFromIssue(issue *forgejo.Issue) string {
	if issue == nil {
		return ""
	}
	role := detectRoleFromTitle(issue.Title)
	if role != "" {
		return role
	}
	for _, label := range issue.Labels {
		name := strings.ToLower(label.Name)
		switch name {
		case "role:pm", "role:project-manager":
			return "pm"
		case "role:reviewer", "role:code-reviewer":
			return "reviewer"
		case "role:devops", "role:infra":
			return "devops"
		case "role:tester", "role:test-engineer":
			return "tester"
		case "role:implementer", "role:developer":
			return "implementer"
		}
	}
	return ""
}

func detectRoleFromTitle(title string) string {
	lower := strings.ToLower(title)
	if strings.Contains(lower, "[pm]") || strings.Contains(lower, "[project manager]") || strings.Contains(lower, "[decompose]") {
		return "pm"
	}
	if strings.Contains(lower, "[review]") || strings.Contains(lower, "[code review]") || strings.Contains(lower, "[reviewer]") {
		return "reviewer"
	}
	if strings.Contains(lower, "[devops]") || strings.Contains(lower, "[infra]") || strings.Contains(lower, "[ci/cd]") || strings.Contains(lower, "[docker]") {
		return "devops"
	}
	if strings.Contains(lower, "[test]") || strings.Contains(lower, "[tester]") || strings.Contains(lower, "[testing]") || strings.Contains(lower, "[qa]") {
		return "tester"
	}
	return ""
}
