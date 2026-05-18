package session

import (
	"context"
	"errors"
	"fmt"
	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
	"github.com/fordjent/fordjent/internal/mergequeue"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/scaffold"
	"github.com/fordjent/fordjent/internal/scheduler"
	"github.com/fordjent/fordjent/internal/sentinel"
	"github.com/fordjent/fordjent/internal/tool"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Session represents an active agent session bound to a session key.
type Session struct {
	Key         string
	Repository  string
	IssueNumber int
	PRNumber    int
	IssueTitle  string
	WorkDir     string
	RepoDir     string
	LastActive  time.Time
	Cancel      context.CancelFunc
	Sender      string // original webhook sender (e.g. fordjent-bot)

	claimedReady bool // set when this session claimed a ready→in_progress transition

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
func (s *sessionInfoAdapter) RepoDir() string { return s.repoDir }

// agentConfigAdapter adapts Config to tool.AgentConfig
type agentConfigAdapter struct {
	cfg        *config.Config
	isScaffold bool
}

func (a *agentConfigAdapter) CommitPrefix() string        { return a.cfg.Agent.CommitPrefix }
func (a *agentConfigAdapter) ProtectedBranches() []string { return a.cfg.Security.ProtectedBranches }
func (a *agentConfigAdapter) RequirePRForWorkflows() bool {
	return a.cfg.Security.RequirePRForWorkflows
}
func (a *agentConfigAdapter) DryRun() bool { return a.cfg.Agent.DryRun }
func (a *agentConfigAdapter) AllowProtectedPush() bool {
	return a.cfg.Agent.AllowProtectedPush || a.isScaffold
}
func (a *agentConfigAdapter) IsScaffold() bool { return a.isScaffold }

// Manager manages agent session lifecycle.
type Manager struct {
	cfg           *config.Config
	bus           *event.Bus
	sessions      map[string]*Session
	mu            sync.RWMutex
	store         *Store
	forgejoClient *forgejo.Client
	adminClient   *forgejo.Client // repo-owner-level client for collab/label setup
	lc            *lifecycle.Lifecycle
	mqClient      *mergequeue.Client
	scheduler     *scheduler.Scheduler
	costTracker   *cost.Tracker
	labelBoot     sync.Map // repo → bool, tracks which repos have had labels ensured
	issueStates   sync.Map // "repo/issues/N" → lifecycle.IssueState, tracks previous FSM state
}

// Lifecycle returns the lifecycle tracker for external wiring (e.g., webhook delivery logging).
func (m *Manager) Lifecycle() *lifecycle.Lifecycle { return m.lc }
func (m *Manager) ForgejoClient() *forgejo.Client  { return m.forgejoClient }
func (m *Manager) AdminClient() *forgejo.Client    { return m.adminClient }

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

	// Admin client uses a separate token (if configured) for repo-owner-level
	// operations like adding collaborators. Falls back to the bot token.
	var adminClient *forgejo.Client
	if cfg.Forgejo.AdminToken != "" {
		adminClient = forgejo.NewClient(cfg.Forgejo.URL, cfg.Forgejo.AdminToken)
		slog.Info("admin client configured for repo setup operations")
	} else {
		adminClient = forgejoClient
	}

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
		adminClient:   adminClient,
		lc:            lc,
	}

	// Wire merge queue and scheduler (both need Forgejo API access)
	forgejoAdapter := tool.NewForgejoAdapter(cfg.Forgejo.URL, cfg.Forgejo.Token)
	m.mqClient = mergequeue.NewClient(forgejoAdapter)
	m.scheduler = scheduler.New(forgejoAdapter)
	m.scheduler.SetForgejoClient(adminClient)

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
		var sessCtx context.Context
		var cancel context.CancelFunc
		if m.cfg.Agent.SessionTimeout > 0 {
			sessCtx, cancel = context.WithTimeout(context.Background(), m.cfg.Agent.SessionTimeout)
		} else {
			sessCtx, cancel = context.WithCancel(context.Background())
		}
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

		recoveryWindow := time.Duration(m.cfg.Agent.RecoveryWindowHours) * time.Hour
		if recoveryWindow <= 0 {
			recoveryWindow = 24 * time.Hour
		}
		// Auto-resume recently-active implementer sessions by posting a synthetic comment
		if m.cfg.Agent.EnableSessionRecovery && time.Since(rec.LastActive) < recoveryWindow && rec.IssueNumber > 0 {
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

	stuckTicker := time.NewTicker(30 * time.Minute)
	defer stuckTicker.Stop()

	recoveryTicker := time.NewTicker(1 * time.Hour)
	defer recoveryTicker.Stop()

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

		case <-stuckTicker.C:
			m.detectStuckSessions(ctx)

		case <-recoveryTicker.C:
			if m.cfg.Agent.EnableSessionRecovery {
				m.runPeriodicRecovery(ctx)
			}
		}
	}
}

func (m *Manager) handleEvent(ctx context.Context, evt *event.Event) {
	// Bootstrap scheduler/lifecycle/scaffold labels once per repo (sync to avoid races)
	// Use admin client if available (bot may not have repo access yet).
	if _, loaded := m.labelBoot.LoadOrStore(evt.Repository, true); !loaded {
		lbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		labelClient := m.adminClient
		if labelClient == nil {
			labelClient = m.forgejoClient
		}
		if err := labelClient.EnsureLabels(lbCtx, evt.Repository); err != nil {
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
			} else {
				slog.Info("scheduler: processed merged PR", "pr", evt.PRNumber, "repo", evt.Repository)
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
				} else {
					slog.Info("scheduler: unblock check completed after push", "repo", evt.Repository)
				}
			}()
		}
	}

	// Scaffold detection: on new issues for empty repos, create/block
	// Skip PM/decompose issues — they don't need code on main to decompose work.
	if m.cfg.Agent.EnableScaffoldDetection && evt.Type == event.IssueOpened && evt.IssueNumber > 0 && evt.Action != "green_light" {
		title := extractIssueTitle(evt)
		lower := strings.ToLower(title)
		isPM := strings.Contains(lower, "[pm]") || strings.Contains(lower, "[project manager]") || strings.Contains(lower, "[decompose]")
		if !isPM {
			blocked, err := scaffold.CheckAndBlock(ctx, m.forgejoClient, evt.Repository, evt.IssueNumber, m.adminClient)
			if err != nil {
				slog.Warn("scaffold detection failed", "error", err, "repo", evt.Repository, "issue", evt.IssueNumber)
			}
			if blocked {
				slog.Info("scaffold: blocked issue on empty repo", "repo", evt.Repository, "issue", evt.IssueNumber)
				return
			}
		}
	}

	// Role gate: if require_role_tag is enabled and the issue has no role tag/label,
	// post a guidance comment and wait for the user to assign one.
	// Skip for green-light events (human already approved the issue).
	if m.cfg.Agent.RequireRoleTag && evt.Type == event.IssueOpened && evt.IssueNumber > 0 && evt.Action != "green_light" {
		issue, err := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.IssueNumber)
		if err != nil {
			slog.Warn("role gate: failed to get issue", "error", err, "issue", evt.IssueNumber)
		} else if detectRoleFromIssue(issue) == "" {
			slog.Info("role gate: blocking untagged issue", "issue", evt.IssueNumber, "repo", evt.Repository)
			m.postRoleGuidance(ctx, evt.Repository, evt.IssueNumber)
			if err := m.forgejoClient.AddIssueLabels(ctx, evt.Repository, evt.IssueNumber, []string{"needs-role"}); err != nil {
				slog.Warn("role gate: failed to add needs-role label", "error", err, "issue", evt.IssueNumber)
			}
			return
		}
	}

	// FSM state detection: derive issue state from labels and react.
	// This MUST run before the role-assignment return below so that
	// done→auto-close works regardless of RequireRoleTag.
	if evt.Type == event.IssueLabelUpdated && evt.IssueNumber > 0 {
		issue, err := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.IssueNumber)
		if err == nil && issue != nil {
			labelNames := make([]string, len(issue.Labels))
			for i, l := range issue.Labels {
				labelNames[i] = l.Name
			}
			newState := lifecycle.StateFromLabels(labelNames)
			stateKey := fmt.Sprintf("%s/issues/%d", evt.Repository, evt.IssueNumber)
			prevStateRaw, _ := m.issueStates.Load(stateKey)
			prevState, _ := prevStateRaw.(lifecycle.IssueState)
			if prevState == "" {
				prevState = lifecycle.StateOpened
			}
			if !lifecycle.IsTransitionValid(prevState, newState) {
				slog.Warn("FSM: invalid state transition, updating state anyway",
					"issue", evt.IssueNumber,
					"repo", evt.Repository,
					"from", prevState,
					"to", newState,
				)
			}
			m.issueStates.Store(stateKey, newState)
			slog.Info("issue state from labels",
				"issue", evt.IssueNumber,
				"repo", evt.Repository,
				"new_state", newState,
				"prev_state", prevState,
				"labels", labelNames,
			)
			switch newState {
			case lifecycle.StateDone:
				if issue.State != "closed" {
					if err := m.forgejoClient.CloseIssue(ctx, evt.Repository, evt.IssueNumber); err != nil {
						slog.Warn("failed to close done issue", "error", err, "issue", evt.IssueNumber)
					}
				}
			}
		}
	}

	// Role assignment detection: when a needs-role issue gets a label or title edit,
	// check if a role is now present and create a session.
	if m.cfg.Agent.RequireRoleTag && (evt.Type == event.IssueLabelUpdated || evt.Type == event.IssueEdited) && evt.IssueNumber > 0 {
		if m.handleRoleAssignment(ctx, evt) {
			return // role was assigned, event handled
		}
		// Role not assigned yet — fall through to green-light check
	}

	// Green-light label detection: when a human adds plan-approved, ready, or implementing
	// label, that's a signal to start working. Create a session.
	if evt.Type == event.IssueLabelUpdated && evt.IssueNumber > 0 && evt.Sender != "fordjent-bot" {
		issue, err := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.IssueNumber)
		if err == nil && issue != nil {
			labelNames := make([]string, len(issue.Labels))
			for i, l := range issue.Labels {
				labelNames[i] = l.Name
			}
			state := lifecycle.StateFromLabels(labelNames)
			switch state {
			case lifecycle.StatePlanApproved, lifecycle.StateImplementing, lifecycle.StateReady:
				role := detectRoleFromIssue(issue)
				if role == "" {
					role = "implementer" // default role for green-light labels
				}
				slog.Info("green-light label detected, creating session", "issue", evt.IssueNumber, "state", state, "role", role, "sender", evt.Sender)
				// Build a synthetic IssueOpened event
				openedEvt := event.NewEvent(event.IssueOpened, evt.Repository, evt.IssueNumber, 0, evt.Sender, "green_light")
				openedEvt.Payload = evt.Payload
				openedEvt.SessionKey = fmt.Sprintf("%s/issues/%d", evt.Repository, evt.IssueNumber)
				m.handleEvent(ctx, openedEvt)
				return
			}
		}
	}

	// Non-role label_updated events should NOT create sessions.
	// FSM state tracking above (lines 379-419) already updates the state machine.
	// Creating sessions for label additions (e.g. "blocked") causes feedback loops:
	// agent adds "blocked" → label_updated → session → agent adds "blocked" again.
	// Only role-assignment and green-light label updates (handled above) should create sessions.
	if evt.Type == event.IssueLabelUpdated {
		slog.Debug("dropping non-role label_updated event", "event_id", evt.ID, "issue", evt.IssueNumber, "repo", evt.Repository)
		return
	}

	// Automerge label detection on PRs
	if evt.Type == event.PullRequestLabelUpdated && evt.PRNumber > 0 {
		pr, err := m.forgejoClient.GetPR(ctx, evt.Repository, evt.PRNumber)
		if err == nil && pr.State != "closed" {
			issue, issueErr := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.PRNumber)
			if issueErr == nil && issue != nil {
				hasAutomerge := false
				for _, l := range issue.Labels {
					if l.Name == "automerge" {
						hasAutomerge = true
						break
					}
				}
				if hasAutomerge {
					slog.Info("automerge label detected on PR, spawning reviewer", "pr", evt.PRNumber, "repo", evt.Repository)
					synthEvt := event.NewEvent(
						event.IssueCommentCreated,
						evt.Repository,
						evt.IssueNumber,
						evt.PRNumber,
						"automerge-trigger",
						"created",
					)
					synthEvt.SessionKey = fmt.Sprintf("%s/pulls/%d", evt.Repository, evt.PRNumber)
					synthEvt.Payload = map[string]interface{}{
						"comment": map[string]interface{}{
							"body": "[System] This PR has the 'automerge' label. Review the code and merge if it passes all checks.",
						},
					}
					m.handleEvent(ctx, synthEvt)
				}
			}
		}
		return
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

	// Role gate: require a role tag or label before creating a session
	// Skip for green-light events (human already approved the issue via plan-approved/ready/implementing label).
	if m.cfg.Agent.RequireRoleTag && evt.Type == event.IssueOpened && evt.IssueNumber > 0 && evt.Action != "green_light" {
		issue, err := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.IssueNumber)
		if err != nil || issue == nil {
			_ = m.forgejoClient.PostIssueComment(ctx, evt.Repository, evt.IssueNumber, buildRoleGuidance())
			_ = m.forgejoClient.AddIssueLabels(ctx, evt.Repository, evt.IssueNumber, []string{"needs-role"})
			return
		}
		role := detectRoleFromIssue(issue)
		if role == "" {
			_ = m.forgejoClient.PostIssueComment(ctx, evt.Repository, evt.IssueNumber, buildRoleGuidance())
			_ = m.forgejoClient.AddIssueLabels(ctx, evt.Repository, evt.IssueNumber, []string{"needs-role"})
			return
		}
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

	var sessCtx context.Context
	var cancel context.CancelFunc
	if m.cfg.Agent.SessionTimeout > 0 {
		sessCtx, cancel = context.WithTimeout(context.Background(), m.cfg.Agent.SessionTimeout)
	} else {
		sessCtx, cancel = context.WithCancel(context.Background())
	}
	sess = &Session{
		Key:         evt.SessionKey,
		Repository:  evt.Repository,
		IssueNumber: evt.IssueNumber,
		PRNumber:    evt.PRNumber,
		IssueTitle:  extractIssueTitle(evt),
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
	// All PRs get a reviewer session to inspect and merge code.
	// Bot PRs retain auto-bypass for merge approval (handled in forgejo_merge_pr tool).
	if sess.PRNumber > 0 && (role == "" || role == "implementer") {
		role = "reviewer"
	}

	// Claim protocol: if implementer starting on a ready issue, atomically swap labels
	// ready→in_progress so other implementers see it's claimed and skip.
	if role == "implementer" && sess.IssueNumber > 0 {
		issue, err := m.forgejoClient.GetIssue(ctx, sess.Repository, sess.IssueNumber)
		if err == nil && issue != nil {
			labelNames := make([]string, len(issue.Labels))
			for i, l := range issue.Labels {
				labelNames[i] = l.Name
			}
			hasReady := false
			for _, ln := range labelNames {
				if ln == "ready" {
					hasReady = true
					break
				}
			}
			if hasReady {
				newLabels := make([]string, 0, len(labelNames))
				for _, ln := range labelNames {
					if ln != "ready" {
						newLabels = append(newLabels, ln)
					}
				}
				newLabels = append(newLabels, "in_progress")
				if err := m.forgejoClient.ReplaceIssueLabels(ctx, sess.Repository, sess.IssueNumber, newLabels); err != nil {
					slog.Warn("claim: failed to transition ready→in_progress", "error", err, "issue", sess.IssueNumber)
				} else {
					slog.Info("claim: transitioned ready→in_progress", "issue", sess.IssueNumber, "repo", sess.Repository)
					sess.claimedReady = true
				}
			}
		}
	}

	agt := NewAgent(m.cfg, sess, m.mqClient, m.costTracker, m.lc, role)

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				slog.Warn("session timed out: hard wall-clock limit reached", "session_key", sess.Key, "limit", m.cfg.Agent.SessionTimeout)
				lcCtx, lcCancel := context.WithTimeout(context.Background(), 10*time.Second)
				m.lc.OnSessionFailedError(lcCtx, sess.Repository, sess.IssueNumber, sess.Key, fmt.Errorf("session timed out after %v", m.cfg.Agent.SessionTimeout))
				lcCancel()
			}
			// Revert claim on timeout
			if sess.claimedReady && sess.IssueNumber > 0 {
				m.revertClaim(ctx, sess)
			}
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
				} else if errors.Is(err, sentinel.ErrMaxTurnsReached) {
					m.lc.OnSessionFailedMaxTurns(ctx, evt.Repository, evt.IssueNumber, sess.Key)
				} else {
					slog.Error("agent processing failed",
						"error", err,
						"event_id", evt.ID,
						"session_key", sess.Key,
					)
					m.lc.OnSessionFailedError(ctx, evt.Repository, evt.IssueNumber, sess.Key, err)
				}
				// Revert claim: if this session claimed a ready issue, release it back to ready
				if sess.claimedReady && sess.IssueNumber > 0 {
					m.revertClaim(ctx, sess)
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
		sess.mu.Lock()
		lastActive := sess.LastActive
		sess.mu.Unlock()
		if time.Since(lastActive) > m.cfg.Agent.IdleTimeout {
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

// revertClaim reverts an in_progress → ready transition when a session fails
// or times out without completing work. Releases the issue for other agents.
func (m *Manager) revertClaim(ctx context.Context, sess *Session) {
	revertCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issue, err := m.forgejoClient.GetIssue(revertCtx, sess.Repository, sess.IssueNumber)
	if err != nil || issue == nil {
		slog.Warn("revertClaim: failed to get issue", "error", err, "issue", sess.IssueNumber)
		return
	}
	labelNames := make([]string, len(issue.Labels))
	for i, l := range issue.Labels {
		labelNames[i] = l.Name
	}
	newLabels := make([]string, 0, len(labelNames))
	hadInProgress := false
	for _, ln := range labelNames {
		if ln == "in_progress" {
			hadInProgress = true
			continue
		}
		newLabels = append(newLabels, ln)
	}
	if !hadInProgress {
		return // nothing to revert
	}
	newLabels = append(newLabels, "ready")
	if err := m.forgejoClient.ReplaceIssueLabels(revertCtx, sess.Repository, sess.IssueNumber, newLabels); err != nil {
		slog.Warn("revertClaim: failed to revert in_progress→ready", "error", err, "issue", sess.IssueNumber)
	} else {
		slog.Info("revertClaim: reverted in_progress→ready", "issue", sess.IssueNumber, "repo", sess.Repository)
		sess.claimedReady = false
	}
}

// runPeriodicRecovery re-scans stored sessions for any that have been restored
// since startup and posts nudge comments to re-activate idle implementer sessions.
func (m *Manager) runPeriodicRecovery(ctx context.Context) {
	records, err := m.store.ListAll()
	if err != nil {
		slog.Warn("periodic recovery: failed to list stored sessions", "error", err)
		return
	}
	recoveryWindow := time.Duration(m.cfg.Agent.RecoveryWindowHours) * time.Hour
	if recoveryWindow <= 0 {
		recoveryWindow = 24 * time.Hour
	}

	for _, rec := range records {
		// Skip sessions that are already active
		m.mu.RLock()
		_, active := m.sessions[rec.SessionKey]
		m.mu.RUnlock()
		if active {
			continue
		}

		// Skip completed/failed sessions
		state, _ := m.lc.GetState(ctx, rec.SessionKey)
		if state == lifecycle.StateCompleted || strings.HasPrefix(state, "failed") {
			continue
		}

		// Only nudge if within recovery window
		if time.Since(rec.LastActive) >= recoveryWindow || rec.IssueNumber <= 0 {
			continue
		}

		issue, err := m.forgejoClient.GetIssue(ctx, rec.Repository, rec.IssueNumber)
		if err == nil && issue != nil && detectRoleFromIssue(issue) == "pm" {
			continue // Do not nudge PM sessions
		}

		slog.Info("periodic recovery: nudging inactive session",
			"session_key", rec.SessionKey,
			"last_active", rec.LastActive,
			"issue", rec.IssueNumber,
		)
		body := "Resuming work after agent restart..."
		_ = m.forgejoClient.PostIssueComment(ctx, rec.Repository, rec.IssueNumber, body)
	}
}

// detectStuckSessions checks for sessions stuck in in_progress or blocked
// FSM states and either nudges or transitions them to failed:timeout.
func (m *Manager) detectStuckSessions(ctx context.Context) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for key, sess := range m.sessions {
		stateRaw, ok := m.issueStates.Load(key)
		if !ok {
			continue
		}
		state, ok := stateRaw.(lifecycle.IssueState)
		if !ok {
			continue
		}

		sess.mu.Lock()
		lastActive := sess.LastActive
		sess.mu.Unlock()

		switch state {
		case lifecycle.StateInProgress:
			if time.Since(lastActive) > 2*time.Hour {
				slog.Warn("stuck session detected: in_progress > 2hrs, posting nudge",
					"session_key", key,
					"last_active", lastActive,
					"issue", sess.IssueNumber,
				)
				go func(repo string, issueNum int) {
					nudgeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					body := "This issue has been in progress for over 2 hours with no activity. Are you still working on it?"
					_ = m.forgejoClient.PostIssueComment(nudgeCtx, repo, issueNum, body)
				}(sess.Repository, sess.IssueNumber)
			}
		case lifecycle.StateFSMBlocked:
			if time.Since(lastActive) > 6*time.Hour {
				slog.Warn("stuck session detected: blocked > 6hrs, transitioning to failed:timeout",
					"session_key", key,
					"last_active", lastActive,
					"issue", sess.IssueNumber,
				)
				go func(sessionKey string) {
					lcCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = m.lc.RecordTransition(lcCtx, sessionKey, lifecycle.StateBlocked, lifecycle.StateFailedError, "stuck in blocked state > 6hrs")
				}(key)
			}
		}
	}
}

// cleanupOldWorkDirs removes work directories for sessions that have been
// completed or failed for more than 7 days, freeing disk space.
func (m *Manager) cleanupOldWorkDirs(ctx context.Context) {
	if m.cfg.Agent.WorkDir == "" {
		return
	}
	archiveBase := filepath.Join(m.cfg.Agent.WorkDir, "..", "archive")
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
		// Archive audit trail before cleanup
		safeKey := strings.ReplaceAll(rec.SessionKey, "/", "_")
		archiveDir := filepath.Join(archiveBase, safeKey)
		if err := os.MkdirAll(archiveDir, 0755); err == nil {
			memFile := filepath.Join(rec.WorkDir, "memory.jsonl")
			if data, err := os.ReadFile(memFile); err == nil {
				_ = os.WriteFile(filepath.Join(archiveDir, "memory.jsonl"), data, 0644)
			}
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
		sess.mu.Lock()
		sessLastActive := sess.LastActive
		sess.mu.Unlock()
		if oldestKey == "" || sessLastActive.Before(oldestTime) {
			oldestKey = key
			oldestTime = sessLastActive
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
	if strings.Contains(lower, "[implementer]") || strings.Contains(lower, "[implement]") || strings.Contains(lower, "[dev]") || strings.Contains(lower, "[developer]") {
		return "implementer"
	}
	return ""
}

func buildRoleGuidance() string {
	return "Please assign a role to this issue by adding a label like `role:pm`, `role:implementer`, `role:reviewer`, `role:devops`, or `role:tester`, or by editing the title to include a tag like `[pm]`, `[implementer]`, `[reviewer]`, `[devops]`, or `[tester]`."
}

// extractIssueTitle pulls the issue title from the webhook payload.
func extractIssueTitle(evt *event.Event) string {
	if issue, ok := evt.Payload["issue"].(map[string]interface{}); ok {
		if title, ok := issue["title"].(string); ok {
			return title
		}
	}
	if pr, ok := evt.Payload["pull_request"].(map[string]interface{}); ok {
		if title, ok := pr["title"].(string); ok {
			return title
		}
	}
	return ""
}

func (m *Manager) postRoleGuidance(ctx context.Context, repo string, issueNumber int) {
	body := `Thanks for creating this issue! Before I start implementing, please tag it with a role so I know how to approach it.

**Available roles:**

| Tag in title | Label | What I'll do |
|---|---|---|
| [pm] or [decompose] | role:pm | Analyze and create sub-issues |
| [review] or [code review] | role:reviewer | Review code and approve/merge PRs |
| [test] or [testing] | role:tester | Write comprehensive tests |
| [devops] or [docker] | role:devops | Docker, CI/CD, deployment changes |
| (no tag, any label) | role:implementer | Write production code and open a PR |

**To assign a role, either:**
- Edit the issue title to include one of the tags above (e.g., "[pm] Add auth system")
- Add one of the matching labels

Once a role is assigned, I'll start working on this automatically.

<!-- ford -->`
	if err := m.forgejoClient.PostIssueComment(ctx, repo, issueNumber, body); err != nil {
		slog.Warn("role gate: failed to post guidance comment", "error", err, "issue", issueNumber)
	}
}

func (m *Manager) handleRoleAssignment(ctx context.Context, evt *event.Event) bool {
	// Check if this issue has the needs-role label
	issue, err := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.IssueNumber)
	if err != nil {
		slog.Warn("role assignment: failed to get issue", "error", err, "issue", evt.IssueNumber)
		return false
	}

	hasNeedsRole := false
	for _, label := range issue.Labels {
		if label.Name == "needs-role" {
			hasNeedsRole = true
			break
		}
	}
	if !hasNeedsRole {
		return false
	}

	role := detectRoleFromIssue(issue)
	if role == "" {
		return false
	}

	slog.Info("role assignment: role detected, creating session", "issue", evt.IssueNumber, "role", role)

	// Remove the needs-role label
	_ = m.forgejoClient.RemoveIssueLabel(ctx, evt.Repository, evt.IssueNumber, "needs-role")

	// Build a synthetic IssueOpened event from the label/edit payload
	openedEvt := event.NewEvent(event.IssueOpened, evt.Repository, evt.IssueNumber, 0, evt.Sender, "synthetic_opened")
	openedEvt.Payload = evt.Payload
	openedEvt.SessionKey = fmt.Sprintf("%s/issues/%d", evt.Repository, evt.IssueNumber)

	// Post confirmation comment
	body := fmt.Sprintf("Role assigned: **%s**. Starting work now.\n\n<!-- ford -->", role)
	_ = m.forgejoClient.PostIssueComment(ctx, evt.Repository, evt.IssueNumber, body)

	// Process the synthetic event (will create session via getOrCreate)
	m.handleEvent(ctx, openedEvt)

	return true
}
