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
	agt := NewAgent(m.cfg, sess, m.mqClient, m.costTracker)

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
				if errors.Is(err, agent.ErrMaxTurnsReached) {
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

