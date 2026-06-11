package session

import (
	"context"
	"errors"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/autoreg"
	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
	"github.com/fordjent/fordjent/internal/mergequeue"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/policy"
	"github.com/fordjent/fordjent/internal/sandbox"
	"github.com/fordjent/fordjent/internal/scaffold"
	"github.com/fordjent/fordjent/internal/scheduler"
	"github.com/fordjent/fordjent/internal/sentinel"
	"github.com/fordjent/fordjent/internal/speccycle"
	"github.com/fordjent/fordjent/internal/tool"
)

// Session represents an active agent session bound to a session key.
type Session struct {
	Key              string
	Repository       string
	IssueNumber      int
	PRNumber         int
	IssueTitle       string
	WorkDir          string
	RepoDir          string
	LastActive       time.Time
	StartTime        time.Time // when session processing began (for wall-clock tracking)
	Cancel           context.CancelFunc
	Sender           string // original webhook sender (e.g. fordjent-bot)
	IsPMFollowUp     bool   // true if this is a PM re-activation follow-up session
	IsScaffoldAnswer bool   // true if this session should answer scaffold questions
	IsPRReviewFix    bool   // true if human commented on PR requesting fixes
	IsArchival       bool   // true if this is a PM archival session triggered by ArchiveChangeRequested
	ChangeName       string // for archival sessions: the change name to archive
	TriggeringIssue  int    // the sub-issue that triggered the PM re-activation
	IsCrashRecovery  bool   // true if this session was restored after a crash (no shutdown.json)

	claimedReady bool // set when this session claimed a ready→in_progress transition

	mu     sync.Mutex
	busy   bool
	events chan *event.Event
}

// isBusy returns whether the session is currently processing events.
func (s *Session) isBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
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
func (a *agentConfigAdapter) GitUser() string { return a.cfg.Agent.GitUser }

// Manager manages agent session lifecycle.
type Manager struct {
	cfg              *config.Config
	bus             *event.Bus
	sessions        map[string]*Session
	mu              sync.RWMutex
	store           *Store
	forgejoClient   *forgejo.Client
	adminClient     *forgejo.Client // repo-owner-level client for collab/label setup
	lc              *lifecycle.Lifecycle
	mqClient        *mergequeue.Client
	scheduler       *scheduler.Scheduler
	specPRManager   *speccycle.SpecPRManager
	costTracker     *cost.Tracker
	sandboxReporter *SandboxReporter
	labelBoot       sync.Map // repo → bool, tracks which repos have had labels ensured
	issueStates     sync.Map // "repo/issues/N" → lifecycle.IssueState, tracks previous FSM state
	autoReg         *autoreg.AutoRegistrar
	policyDetector *policy.CachedDetector
}

// SandboxReporter implements sandbox.ErrorReporter by posting violation comments to Forgejo.
type SandboxReporter struct {
	client *forgejo.Client
}

func NewSandboxReporter(client *forgejo.Client) *SandboxReporter {
	return &SandboxReporter{client: client}
}

func (r *SandboxReporter) ReportSandboxViolation(ctx context.Context, repo string, issueNumber int, err sandbox.SandboxError) {
	comment := sandbox.BuildViolationComment(err, 0)
	if r.client != nil {
		if postErr := r.client.PostIssueComment(ctx, repo, issueNumber, comment); postErr != nil {
			slog.Warn("failed to post sandbox violation comment", "error", postErr, "issue", issueNumber, "repo", repo)
		}
	}
}

// Lifecycle returns the lifecycle tracker for external wiring (e.g., webhook delivery logging).
func (m *Manager) Lifecycle() *lifecycle.Lifecycle { return m.lc }
func (m *Manager) ForgejoClient() *forgejo.Client  { return m.forgejoClient }
func (m *Manager) AdminClient() *forgejo.Client    { return m.adminClient }

func (m *Manager) HasActiveSession(repo string, issueNumber int) bool {
	key := fmt.Sprintf("%s/issues/%d", repo, issueNumber)
	m.mu.RLock()
	_, exists := m.sessions[key]
	m.mu.RUnlock()
	return exists
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
		cfg:              cfg,
		bus:              bus,
		sessions:         make(map[string]*Session),
		store:            store,
		costTracker:      costTracker,
		forgejoClient:    forgejoClient,
		adminClient:      adminClient,
		lc:               lc,
		sandboxReporter:  NewSandboxReporter(forgejoClient),
	}

	// Wire merge queue and scheduler (both need Forgejo API access)
	forgejoAdapter := tool.NewForgejoAdapter(cfg.Forgejo.URL, cfg.Forgejo.Token)
	m.mqClient = mergequeue.NewClient(forgejoAdapter)
	m.scheduler = scheduler.New(forgejoAdapter)
	m.scheduler.SetForgejoClient(adminClient)
	m.scheduler.SetBus(bus) // enable ArchiveChangeRequested dispatch
	if cfg.Agent.MaxCascadeRounds > 0 {
		m.scheduler.SetMaxCascadeRounds(cfg.Agent.MaxCascadeRounds)
	}

	// Wire spec PR manager for detecting spec PR merges
	m.specPRManager = speccycle.NewSpecPRManager(adminClient)


	// Wire auto-registrar if enabled
	if cfg.AutoRegister.Enabled {
		m.autoReg = autoreg.NewAutoRegistrar(autoreg.AutoRegistrarConfig{
			ForgejoClient: adminClient,
			WebhookURL:    cfg.AutoRegister.WebhookURL,
			WebhookSecret: cfg.Webhook.Secret,
		})
		slog.Info("auto-register enabled", "webhook_url", cfg.AutoRegister.WebhookURL)
	}

	// Wire policy detector (uses admin client to read repo topics)
	m.policyDetector = policy.NewCachedDetector(adminClient)
	slog.Info("policy detector initialized", "default_policy", policy.DefaultPolicy().String())

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

		// Check for shutdown checkpoint
		tp := readShutdownCheckpoint(rec.WorkDir)
		if tp != nil {
			switch tp.State {
			case "completed":
				slog.Info("skipping completed session (shutdown.json)", "session_key", rec.SessionKey)
				m.store.Delete(rec.SessionKey)
				deleteShutdownCheckpoint(rec.WorkDir)
				continue
			case "cancelled":
				slog.Info("skipping cancelled session (shutdown.json)", "session_key", rec.SessionKey)
				m.store.Delete(rec.SessionKey)
				deleteShutdownCheckpoint(rec.WorkDir)
				continue
			case "failed":
				// Failed sessions may be retried by the auto-retry system. Continue to restore.
				deleteShutdownCheckpoint(rec.WorkDir)
			}
		}
		// If no shutdown.json and the lifecycle shows 'working', this is a crash recovery.
		// Verification steering will be enabled on the agent after creation.

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
		// If no shutdown checkpoint was found and lifecycle state is 'working',
		// this is a crash recovery — the previous session was interrupted.
		isCrashRecovery := tp == nil && state == lifecycle.StateWorking
		sess := &Session{
			Key:             rec.SessionKey,
			Repository:      rec.Repository,
			IssueNumber:     rec.IssueNumber,
			PRNumber:        rec.PRNumber,
			WorkDir:         rec.WorkDir,
			RepoDir:         rec.RepoDir,
			LastActive:      rec.LastActive,
			Cancel:          cancel,
			IsCrashRecovery: isCrashRecovery,
			events:          make(chan *event.Event, 64),
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

	autoRetryDelay := m.cfg.Agent.AutoRetryDelay
	if autoRetryDelay <= 0 {
		autoRetryDelay = 5 * time.Minute
	}
	autoRetryTicker := time.NewTicker(autoRetryDelay)
	defer autoRetryTicker.Stop()

	reconcileTicker := time.NewTicker(2 * time.Hour)
	defer reconcileTicker.Stop()

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

		case <-autoRetryTicker.C:
			m.runAutoRetry(ctx)

		case <-reconcileTicker.C:
			m.runSchedulerReconcile(ctx)
		}
	}
}

func (m *Manager) handleEvent(ctx context.Context, evt *event.Event) {
	// Auto-register: ensure webhook and labels exist for this repo.
	// Run before any other logic so all repos get registered regardless of event type.
	if m.autoReg != nil {
		if err := m.autoReg.EnsureRegistered(ctx, evt.Repository); err != nil {
			slog.Warn("auto-register failed", "repo", evt.Repository, "error", err)
			// Non-fatal: continue processing the event
		}
	}

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

	// If a PR was merged, process dependent unblock + spec PR detection
	if evt.Type == event.PullRequestMerged && evt.PRNumber > 0 {
		// Check if this is a spec PR and generate implementer issues if so
		go m.handleSpecPRMerged(ctx, evt)

		// Notify scheduler to unblock dependent issues
		go func() {
			schedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			pmResults, err := m.scheduler.OnPRMerged(schedCtx, evt.Repository, evt.PRNumber)
			if err != nil {
				slog.Warn("scheduler: failed to process merged PR", "error", err, "pr", evt.PRNumber, "repo", evt.Repository)
			} else {
				slog.Info("scheduler: processed merged PR", "pr", evt.PRNumber, "repo", evt.Repository)
			}

			// PM reactivation: if all sub-issues of a PM parent are complete,
			// publish PMReactivate events and close the milestone.
			for _, pmr := range pmResults {
				slog.Info("scheduler: PM parent issue fully resolved, emitting PMReactivate",
					"parent_issue", pmr.ParentIssueNumber,
					"triggering_issue", pmr.TriggeringIssue,
					"repo", evt.Repository,
				)

				// Close the parent's milestone if all sub-issues are done
				parentIssue, err := m.forgejoClient.GetIssue(schedCtx, evt.Repository, pmr.ParentIssueNumber)
				if err == nil && parentIssue != nil && parentIssue.Milestone != nil {
					_ = m.forgejoClient.CloseMilestone(schedCtx, evt.Repository, parentIssue.Milestone.ID)
					slog.Info("closed milestone", "milestone_id", parentIssue.Milestone.ID, "title", parentIssue.Milestone.Title)
				}

				reactivateEvt := event.NewEvent(
					event.PMReactivate,
					evt.Repository,
					pmr.ParentIssueNumber,
					0,
					"fordjent-scheduler",
					"reactivate",
				)
				reactivateEvt.TriggeringIssue = pmr.TriggeringIssue
				reactivateEvt.SessionKey = fmt.Sprintf("%s/issues/%d", evt.Repository, pmr.ParentIssueNumber)
				m.bus.Publish(schedCtx, reactivateEvt)
			}

			// Lifecycle orchestration: check if all task issues for the
			// change are closed. If so, dispatch ArchiveChangeRequested
			// so the PM archives the change.
			for _, pmr := range pmResults {
				allClosed, changeName, checkErr := m.scheduler.CheckAllTasksClosed(schedCtx, evt.Repository, pmr.ParentIssueNumber)
				if checkErr != nil {
					slog.Warn("scheduler: failed to check all tasks closed",
						"error", checkErr, "parent_issue", pmr.ParentIssueNumber, "repo", evt.Repository)
					continue
				}
				if allClosed {
					slog.Info("scheduler: all task issues closed for change, dispatching ArchiveChangeRequested",
						"change", changeName, "parent_issue", pmr.ParentIssueNumber, "repo", evt.Repository)
					m.scheduler.MarkTaskInTasksMd(schedCtx, evt.Repository, evt.PRNumber)
					m.scheduler.DispatchArchiveChangeRequested(schedCtx, evt.Repository, changeName, pmr.ParentIssueNumber)
				}
			}

			// Auto-requeue any blocked branches whose merge-gate may now be clear.
			if m.mqClient != nil && m.lc != nil {
				time.Sleep(5 * time.Second) // let Forgejo update PR state after merge
				blocked, err := m.lc.ListBlockedBranches(schedCtx, evt.Repository)
				if err != nil {
					slog.Warn("lifecycle: failed to list blocked branches", "error", err)
					return
				}
				for _, b := range blocked {
					// Don't re-dispatch if we've already retried too many times
					if b.RetryCount >= 2 {
						slog.Warn("merge queue: max retries reached for blocked branch, giving up", "branch", b.Branch, "retries", b.RetryCount)
						_ = m.lc.ResolveBlockedBranch(schedCtx, evt.Repository, b.Branch)
						continue
					}
					cleared, msg, err := m.mqClient.CheckGate(schedCtx, evt.Repository, b.Branch, "main")
					if err != nil {
						slog.Warn("merge queue: re-check failed", "error", err, "branch", b.Branch)
						continue
					}
					if cleared {
						slog.Info("merge gate cleared for blocked branch, re-dispatching session", "branch", b.Branch, "issue", b.IssueNumber)
						// Re-dispatch a new implementer session for this issue.
						retryEvt := event.NewEvent(
							event.IssueOpened,
							evt.Repository,
							b.IssueNumber,
							0,
							"merge-queue-retry",
							"opened",
						)
						retryEvt.SessionKey = fmt.Sprintf("%s/issues/%d", evt.Repository, b.IssueNumber)
						// Remove the blocked label so the agent can pick it up
						if m.forgejoClient != nil {
							_ = m.forgejoClient.RemoveIssueLabel(schedCtx, evt.Repository, b.IssueNumber, "blocked")
						}
						m.handleEvent(schedCtx, retryEvt)
						_ = m.lc.ResolveBlockedBranch(schedCtx, evt.Repository, b.Branch)
					} else {
						// Gate may still appear blocked because Forgejo hasn't updated yet.
						// If the blocking PR was the one that just merged, force a retry anyway.
						if strings.Contains(msg, fmt.Sprintf("#%d", evt.PRNumber)) {
							slog.Info("merge gate still references just-merged PR, re-dispatching anyway", "merged_pr", evt.PRNumber, "branch", b.Branch, "gate_msg", msg)
							retryEvt := event.NewEvent(
								event.IssueOpened,
								evt.Repository,
								b.IssueNumber,
								0,
								"merge-queue-retry",
								"opened",
							)
							retryEvt.SessionKey = fmt.Sprintf("%s/issues/%d", evt.Repository, b.IssueNumber)
							if m.forgejoClient != nil {
								_ = m.forgejoClient.RemoveIssueLabel(schedCtx, evt.Repository, b.IssueNumber, "blocked")
							}
							m.handleEvent(schedCtx, retryEvt)
							_ = m.lc.ResolveBlockedBranch(schedCtx, evt.Repository, b.Branch)
						} else {
							slog.Info("merge gate still blocked for branch", "branch", b.Branch, "reason", msg)
						}
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
					scaffoldClosed := false
					for _, issue := range issues {
						if strings.HasPrefix(issue.Title, "[scaffold]") {
							if err := m.forgejoClient.CloseIssue(schedCtx, evt.Repository, issue.Number); err != nil {
								slog.Warn("push handler: failed to close scaffold issue", "error", err, "issue", issue.Number)
							} else {
								slog.Info("push handler: closed scaffold issue", "issue", issue.Number, "repo", evt.Repository)
								scaffoldClosed = true
							}
						}
					}

					// If a scaffold was closed, remove 'blocked' labels from issues
					// that were blocked by the scaffold detector
					if scaffoldClosed {
						for _, issue := range issues {
							if issue.Number == 0 || strings.HasPrefix(issue.Title, "[scaffold]") {
								continue
							}
							hasBlocked := false
							for _, l := range issue.Labels {
								if l.Name == "blocked" {
									hasBlocked = true
									break
								}
							}
							if hasBlocked {
								if err := m.forgejoClient.RemoveIssueLabel(schedCtx, evt.Repository, issue.Number, "blocked"); err != nil {
									slog.Warn("push handler: failed to remove blocked label", "error", err, "issue", issue.Number)
								} else {
									slog.Info("push handler: unblocked issue after scaffold completion", "issue", issue.Number, "repo", evt.Repository)
								}
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
	// Skip for scaffold issues ([scaffold] prefix) — they are auto-generated and should
	// be implemented immediately.
	// Skip for PRs — PRs don't need role tags, they're already past the implementation stage.
	// Skip for fordjent-yolo repos — untagged issues default to "implementer".
	if m.cfg.Agent.RequireRoleTag && evt.Type == event.IssueOpened && evt.IssueNumber > 0 && evt.Action != "green_light" && evt.PRNumber == 0 {
		issue, err := m.forgejoClient.GetIssue(ctx, evt.Repository, evt.IssueNumber)
		if err != nil {
			slog.Warn("role gate: failed to get issue", "error", err, "issue", evt.IssueNumber)
		} else if detectRoleFromIssue(issue) == "" && !strings.HasPrefix(issue.Title, "[scaffold]") {
			// Check if repo has fordjent-yolo topic → default to implementer
			yolo := false
			if topics, tErr := m.forgejoClient.GetRepoTopics(ctx, evt.Repository); tErr == nil {
				for _, t := range topics {
					if t == "fordjent-yolo" {
						yolo = true
						break
					}
				}
			}
			if yolo {
				slog.Info("role gate: defaulting untagged issue to implementer (fordjent-yolo)", "issue", evt.IssueNumber, "repo", evt.Repository)
			} else {
				slog.Info("role gate: blocking untagged issue", "issue", evt.IssueNumber, "repo", evt.Repository)
				m.postRoleGuidance(ctx, evt.Repository, evt.IssueNumber)
				if err := m.forgejoClient.AddIssueLabels(ctx, evt.Repository, evt.IssueNumber, []string{"needs-role"}); err != nil {
					slog.Warn("role gate: failed to add needs-role label", "error", err, "issue", evt.IssueNumber)
				}
				return
			}
		}
	}

	// Spec lifecycle labels: if this is a PR with spec-proposed label and it's now merged,
	// transition from spec-proposed to spec-approved.
	if evt.Type == event.PullRequestMerged && evt.PRNumber > 0 {
		go m.handleSpecLifecycleLabels(ctx, evt)
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

				// If plan-approved was added to a parent issue, unblock sub-issues.
				// Find issues with "Depends on: #N" in their body and transition them
				// from "planning" to "ready".
				if state == lifecycle.StatePlanApproved {
					m.unblockSubIssues(ctx, evt.Repository, evt.IssueNumber)
				}
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
					slog.Info("automerge label detected on PR, attempting direct merge", "pr", evt.PRNumber, "repo", evt.Repository)
					// Try to merge the PR directly via API. No LLM session needed.
					// Try multiple merge styles as a workaround for Forgejo v9 405 errors.
					if m.forgejoClient != nil {
						prDetail, prErr := m.forgejoClient.GetPR(ctx, evt.Repository, evt.PRNumber)
						if prErr == nil && prDetail.State == "open" && !prDetail.HasConflicts {
							// Try each merge style in order; Forgejo v9 accepts some but not all
							for _, style := range []string{"merge", "squash", "rebase-merge"} {
								mergeErr := m.forgejoClient.MergePR(ctx, evt.Repository, evt.PRNumber, style)
								if mergeErr == nil {
									slog.Info("automerge: direct merge succeeded", "pr", evt.PRNumber, "style", style, "repo", evt.Repository)
									return
								}
								slog.Debug("automerge: merge style failed, trying next", "pr", evt.PRNumber, "style", style, "error", mergeErr)
							}
							// All styles failed — the automerge label is already on the PR.
							// Forgejo's native auto-merge may handle it if configured on the server.
							// Post a comment for visibility and skip the reviewer session.
							slog.Warn("automerge: all API merge styles failed, label is set for Forgejo native auto-merge", "pr", evt.PRNumber)
							if m.forgejoClient != nil {
								_ = m.forgejoClient.PostIssueComment(ctx, evt.Repository, evt.PRNumber, "⚠️ Auto-merge via API failed. The 'automerge' label is set — Forgejo will merge automatically when checks pass, or a human can merge manually.")
							}
							return
						} else {
								slog.Warn("automerge: PR not mergeable", "pr", evt.PRNumber, "error", prErr, "state", prDetail.State, "conflicts", prDetail.HasConflicts)
							}
						}
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

	// ArchiveChangeRequested: when the scheduler detects that all task issues
	// for a change are closed, it dispatches this internal event. Create a PM
	// archival session to move the change directory to archive/.
	if evt.Type == event.ArchiveChangeRequested {
		changeName := evt.Change
		if changeName == "" {
			changeName = "unknown"
		}
		sessionKey := fmt.Sprintf("%s/archive/%s-%d", evt.Repository, changeName, time.Now().UnixNano())
		slog.Info("ArchiveChangeRequested: creating PM archival session",
			"change", changeName,
			"repo", evt.Repository,
			"session_key", sessionKey,
		)
		archiveEvt := event.NewEvent(event.ArchiveChangeRequested, evt.Repository, evt.IssueNumber, 0, "fordjent-scheduler", "archive")
		archiveEvt.SessionKey = sessionKey
		archiveEvt.Change = changeName
		archiveEvt.Payload = map[string]interface{}{
			"archive_change": true,
			"change_name":    changeName,
		}
		m.handleEvent(ctx, archiveEvt)
		return
	}

	// PM Reactivation: when the scheduler detects that all sub-issues of a PM
	// parent are complete, it emits a PMReactivate event. Create a fresh
	// follow-up session for the parent PM issue.
	if evt.Type == event.PMReactivate && evt.IssueNumber > 0 {
		// Check if the original PM session is still running
		prefixKey := fmt.Sprintf("%s/issues/%d", evt.Repository, evt.IssueNumber)
		m.mu.RLock()
		for k, sess := range m.sessions {
			if strings.HasPrefix(k, prefixKey) && sess.isBusy() {
				m.mu.RUnlock()
				slog.Info("skipping PM reactivation — original PM session still running",
					"session_key", k, "issue", evt.IssueNumber)
				return
			}
		}
		m.mu.RUnlock()

		timestamp := time.Now().Unix()
		sessionKey := fmt.Sprintf("%s/issues/%d/pm-followup-%d", evt.Repository, evt.IssueNumber, timestamp)
		reactivateEvt := event.NewEvent(event.PMReactivate, evt.Repository, evt.IssueNumber, 0, evt.Sender, "reactivate")
		reactivateEvt.TriggeringIssue = evt.TriggeringIssue
		reactivateEvt.SessionKey = sessionKey
		reactivateEvt.Payload = map[string]interface{}{
			"pm_followup":       true,
			"parent_issue":      evt.IssueNumber,
			"triggering_issue":  evt.TriggeringIssue,
		}
		slog.Info("PM reactivation: creating follow-up session",
			"parent_issue", evt.IssueNumber,
			"triggering_issue", evt.TriggeringIssue,
			"session_key", sessionKey,
		)
		m.handleEvent(ctx, reactivateEvt)
		return
	}

	if evt.SessionKey == "" {
		slog.Warn("event with empty session key, dropping", "event_id", evt.ID)
		return
	}

	// PR review fix: when a human comments on an open PR requesting changes,
	// create a separate implementer session so the agent has write tools.
	// The existing reviewer session (read-only) may still be running — we
	// create a new session with key `repo/pulls/N-fix` and implementer role.
	if evt.Type == event.IssueCommentCreated && evt.PRNumber > 0 &&
		evt.Sender != "automerge-trigger" &&
		!strings.Contains(evt.Sender, "fordjent") &&
		!strings.Contains(evt.Sender, "djent") {

		// Check that the PR is still open
		if m.forgejoClient != nil {
			pr, prErr := m.forgejoClient.GetPR(ctx, evt.Repository, evt.PRNumber)
			if prErr == nil && pr.State == "open" {
				// Redirect to a new session key with -fix suffix.
				// Use event ID to create unique keys per comment, so conflicting
				// feedback from different reviewers gets separate sessions.
				fixKey := evt.SessionKey + "-fix"
				if evt.ID != "" {
					// Use last 8 chars of event ID for uniqueness
					suffix := evt.ID
					if len(suffix) > 8 {
						suffix = suffix[len(suffix)-8:]
					}
					fixKey = evt.SessionKey + "-fix-" + suffix
				}
				slog.Info("PR review fix: redirecting human comment to implementer session",
					"original_key", evt.SessionKey,
					"fix_key", fixKey,
					"pr", evt.PRNumber,
					"sender", evt.Sender)

				// Create a synthetic event with the new session key
				fixEvt := *evt
				fixEvt.SessionKey = fixKey
				fixSess, fixErr := m.getOrCreate(ctx, &fixEvt)
				if fixErr != nil {
					slog.Error("failed to create review-fix session", "error", fixErr, "key", fixKey)
				} else {
					fixSess.mu.Lock()
					fixSess.IsPRReviewFix = true
					fixSess.mu.Unlock()
					select {
					case fixSess.events <- &fixEvt:
						slog.Info("queued event for review-fix session", "session_key", fixKey)
					default:
						slog.Warn("review-fix session event queue full", "session_key", fixKey)
					}
				}
				return // Don't also send to the reviewer session
			}
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
		slog.Warn("session event queue full, blocking with timeout",
			"event_id", evt.ID,
			"session_key", sess.Key,
		)
		// Instead of silently dropping the event, block with a timeout.
		// A dropped event (human comment, PR merge, etc.) is worse than a brief delay.
		select {
		case sess.events <- evt:
			slog.Info("queued event after brief wait", "event_id", evt.ID)
		case <-time.After(5 * time.Second):
			slog.Error("session event queue still full after 5s, dropping event",
				"event_id", evt.ID,
				"session_key", sess.Key,
			)
		}
	}
}

func buildCloneURL(baseURL, token, repo string) string {
	if token == "" {
		return fmt.Sprintf("%s/%s.git", baseURL, repo)
	}

	// Determine the clone username. Git requires user:token format for authentication.
	// When only a token is provided, git prompts for a password interactively,
	// which fails in automated environments. Use a sensible default username.
	cloneUser := "fordjent-bot"

	if strings.HasPrefix(baseURL, "https://") {
		return fmt.Sprintf("https://%s:%s@%s/%s.git", cloneUser, token, strings.TrimPrefix(baseURL, "https://"), repo)
	}
	if strings.HasPrefix(baseURL, "http://") {
		return fmt.Sprintf("http://%s:%s@%s/%s.git", cloneUser, token, strings.TrimPrefix(baseURL, "http://"), repo)
	}
	return fmt.Sprintf("%s/%s.git", baseURL, repo)
}

func (m *Manager) getOrCreate(ctx context.Context, evt *event.Event) (*Session, error) {
	m.mu.RLock()
	sess, exists := m.sessions[evt.SessionKey]
	m.mu.RUnlock()

	if exists {
		sess.mu.Lock()
		sess.LastActive = time.Now()
		// If this is a human comment on an open PR, mark it as a review fix session.
		// This overrides the reviewer role (no write tools) to implementer (has write tools)
		// so the agent can actually make the requested changes.
		if evt.Type == event.IssueCommentCreated && evt.PRNumber > 0 && evt.Sender != "automerge-trigger" && !strings.Contains(evt.Sender, "fordjent") && !strings.Contains(evt.Sender, "djent") {
			sess.IsPRReviewFix = true
			slog.Info("PR comment from human: marking session as review-fix (implementer role)",
				"session_key", sess.Key, "pr", evt.PRNumber, "sender", evt.Sender)
		}
		sess.mu.Unlock()
		return sess, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if sess, exists = m.sessions[evt.SessionKey]; exists {
		sess.mu.Lock()
		sess.LastActive = time.Now()
		sess.mu.Unlock()
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
		cmd := exec.CommandContext(ctx, "git", "clone", "--filter=blob:none", cloneURL, repoDir)
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
		Key:             evt.SessionKey,
		Repository:      evt.Repository,
		IssueNumber:     evt.IssueNumber,
		PRNumber:        evt.PRNumber,
		IssueTitle:      extractIssueTitle(evt),
		WorkDir:         workDir,
		RepoDir:         repoDir,
		LastActive:      time.Now(),
		Cancel:          cancel,
		Sender:          evt.Sender,
		IsPMFollowUp:    evt.Type == event.PMReactivate,
		IsScaffoldAnswer: hasQuestionLabel(ctx, m.forgejoClient, evt.Repository, evt.IssueNumber),
		IsPRReviewFix:   evt.Type == event.IssueCommentCreated && evt.PRNumber > 0 && evt.Sender != "automerge-trigger" && !strings.Contains(evt.Sender, "fordjent") && !strings.Contains(evt.Sender, "djent"),
		IsArchival:      evt.Type == event.ArchiveChangeRequested,
		ChangeName:       evt.Change,
		TriggeringIssue: evt.TriggeringIssue,
		events:          make(chan *event.Event, 64),
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

	// PM follow-up sessions always use the PM role
	if sess.IsPMFollowUp {
		role = "pm"
	}

	// PM archival sessions always use the PM role
	if sess.IsArchival {
		role = "pm"
	}

	// Scaffold answer sessions: the human replied to the "question" label.
	// Use implementer role to create project files.
	if sess.IsScaffoldAnswer {
		role = "implementer"
		slog.Info("scaffold answer session starting", "session_key", sess.Key,
			"repo", sess.Repository, "issue", sess.IssueNumber)
	}

	// PR review fix: when a human commented on an open PR requesting changes,
	// the agent should be an implementer (write tools) not a reviewer (read-only).
	// This allows the agent to actually make the requested fixes.
	if sess.IsPRReviewFix {
		role = "implementer"
		slog.Info("PR review fix session: using implementer role", "session_key", sess.Key, "pr", sess.PRNumber)
	} else if sess.PRNumber > 0 && (role == "" || role == "implementer") {
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

	agt := NewAgent(m.cfg, sess, m.mqClient, m.costTracker, m.lc, role, m.sandboxReporter, m.scheduler, m.policyDetector)

	// Enable verification steering for crash-recovered sessions
	if sess.IsCrashRecovery {
		agt.SetVerificationSteering(true)
		slog.Info("enabled verification steering for crash-recovered session", "session_key", sess.Key)
	}

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				slog.Warn("session timed out: hard wall-clock limit reached", "session_key", sess.Key, "limit", m.cfg.Agent.SessionTimeout)
				lcCtx, lcCancel := context.WithTimeout(context.Background(), 10*time.Second)
				m.lc.OnSessionFailedError(lcCtx, sess.Repository, sess.IssueNumber, sess.Key, fmt.Errorf("session timed out after %v", m.cfg.Agent.SessionTimeout), m.cfg.Agent.SessionTimeout, m.roleToken(role))
				lcCancel()
				writeShutdownCheckpoint(sess.WorkDir, "failed", agt.LastToolName())
			} else {
				// Context cancelled — likely shutdownAll
				writeShutdownCheckpoint(sess.WorkDir, "cancelled", agt.LastToolName())
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

			// Record wall-clock start time for time tracking
			if sess.StartTime.IsZero() {
				sess.StartTime = time.Now()
			}

			// Assign the issue to the role user (djent-pm, djent-dev, djent-qa)
			if roleName := detectRoleFromSession(ctx, m.forgejoClient, sess); roleName != "" {
				if roleUser, ok := m.cfg.Forgejo.RoleUsers[roleName]; ok && roleUser != "" {
					if err := m.forgejoClient.AddAssignees(ctx, evt.Repository, evt.IssueNumber, []string{roleUser}); err != nil {
						slog.Warn("failed to assign role user to issue", "error", err, "issue", evt.IssueNumber, "role", roleName, "user", roleUser)
					} else {
						slog.Info("assigned role user to issue", "issue", evt.IssueNumber, "role", roleName, "user", roleUser)
					}
				}
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
					writeShutdownCheckpoint(sess.WorkDir, "failed", agt.LastToolName())
				} else if errors.Is(err, sentinel.ErrMaxTurnsReached) {
					m.lc.OnSessionFailedMaxTurns(ctx, evt.Repository, evt.IssueNumber, sess.Key, time.Since(sess.StartTime), m.roleToken(role))
				m.logSessionTime(ctx, evt.Repository, evt.IssueNumber, role, sess.StartTime)
				} else {
					slog.Error("agent processing failed",
						"error", err,
						"event_id", evt.ID,
						"session_key", sess.Key,
					)
					m.lc.OnSessionFailedError(ctx, evt.Repository, evt.IssueNumber, sess.Key, err, time.Since(sess.StartTime), m.roleToken(role))
				m.logSessionTime(ctx, evt.Repository, evt.IssueNumber, role, sess.StartTime)
				}
				// Revert claim: if this session claimed a ready issue, release it back to ready
				if sess.claimedReady && sess.IssueNumber > 0 {
					m.revertClaim(ctx, sess)
				}
			} else {
				// Get head SHA for commit status
				headSHA := ""
				if sess.RepoDir != "" {
					if out, err := exec.CommandContext(ctx, "git", "-C", sess.RepoDir, "rev-parse", "HEAD").Output(); err == nil {
						headSHA = strings.TrimSpace(string(out))
					}
				}
				m.lc.OnSessionComplete(ctx, sess.Key, evt.Repository, evt.IssueNumber, role, headSHA, time.Since(sess.StartTime), m.roleToken(role), m.cfg.FordjentBaseURL())
				m.logSessionTime(ctx, evt.Repository, evt.IssueNumber, role, sess.StartTime)
				writeShutdownCheckpoint(sess.WorkDir, "completed", agt.LastToolName())
				_ = m.store.SetCompletedAt(sess.Key, time.Now().UTC())
			}

			sess.mu.Lock()
			sess.busy = false
			sess.LastActive = time.Now()
			sess.mu.Unlock()
		}
	}
}

// roleToken returns the Forgejo token for a given role, or empty string.
func (m *Manager) roleToken(role string) string {
	if m.cfg.Forgejo.RoleTokens != nil {
		if t, ok := m.cfg.Forgejo.RoleTokens[role]; ok {
			return t
		}
	}
	return ""
}

// logSessionTime logs wall-clock session duration via Forgejo time tracking,
// using the role-specific token so the entry appears under the role user.
func (m *Manager) logSessionTime(ctx context.Context, repo string, issueNumber int, role string, startTime time.Time) {
	dur := time.Since(startTime)
	if dur <= 0 {
		return
	}
	// Use role token if available, so time is logged as djent-dev / djent-qa
	fc := m.forgejoClient
	if roleToken, ok := m.cfg.Forgejo.RoleTokens[role]; ok && roleToken != "" {
		fc = fc.WithToken(roleToken)
	}
	if _, err := fc.AddTrackedTime(ctx, repo, issueNumber, int(dur.Seconds())); err != nil {
		slog.Warn("failed to log session time", "error", err, "duration", dur, "role", role)
	}
}

// shutdownCheckpoint is written to the session's work directory on exit.
type shutdownCheckpoint struct {
	State     string `json:"state"`    // "completed", "failed", "cancelled"
	LastTool  string `json:"last_tool"` // name of the last tool executed
	Timestamp string `json:"timestamp"`
}

// writeShutdownCheckpoint writes a shutdown.json to the session's work directory.
// The file captures the final state, last tool name, and timestamp so that
// restoreSessions can make informed decisions about whether to re-create a session.
func writeShutdownCheckpoint(workDir string, state, lastTool string) {
	if workDir == "" {
		return
	}
	cp := shutdownCheckpoint{
		State:     state,
		LastTool:  lastTool,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(cp)
	if err != nil {
		slog.Warn("failed to marshal shutdown checkpoint", "error", err)
		return
	}
	cpPath := filepath.Join(workDir, "shutdown.json")
	if err := os.WriteFile(cpPath, data, 0644); err != nil {
		slog.Warn("failed to write shutdown checkpoint", "error", err, "path", cpPath)
	}
}

// readShutdownCheckpoint reads the shutdown.json from a work directory.
// Returns nil if the file doesn't exist (crash case).
func readShutdownCheckpoint(workDir string) *shutdownCheckpoint {
	if workDir == "" {
		return nil
	}
	cpPath := filepath.Join(workDir, "shutdown.json")
	data, err := os.ReadFile(cpPath)
	if err != nil {
		return nil // file missing = crash
	}
	var cp shutdownCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil
	}
	return &cp
}

// deleteShutdownCheckpoint removes the shutdown.json from a work directory.
func deleteShutdownCheckpoint(workDir string) {
	if workDir == "" {
		return
	}
	cpPath := filepath.Join(workDir, "shutdown.json")
	_ = os.Remove(cpPath)
}

// pruneMergedBranches runs 'git branch --merged main' in a repo directory and
// deletes any feature branches that have been merged. Excludes 'main' and the
// current branch. Errors are logged non-fatally.
func pruneMergedBranches(repoDir string) {
	if repoDir == "" {
		return
	}
	// Check if git repo exists
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		return
	}

	// Get list of merged branches
	cmd := exec.Command("git", "-C", repoDir, "branch", "--merged", "main")
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("pruneMergedBranches: git branch --merged failed", "error", err, "dir", repoDir)
		return
	}

	// Get current branch name (to avoid pruning it)
	currentCmd := exec.Command("git", "-C", repoDir, "branch", "--show-current")
	currentOutput, _ := currentCmd.CombinedOutput()
	currentBranch := strings.TrimSpace(string(currentOutput))

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	pruned := 0
	for _, line := range lines {
		branch := strings.TrimSpace(line)
		// Skip empty, main, master, and current branch
		if branch == "" || branch == "main" || branch == "master" || branch == currentBranch {
			continue
		}
		// Delete the merged feature branch
		delCmd := exec.Command("git", "-C", repoDir, "branch", "-d", branch)
		if delErr := delCmd.Run(); delErr != nil {
			slog.Debug("pruneMergedBranches: failed to delete branch", "branch", branch, "error", delErr)
		} else {
			pruned++
			slog.Info("pruneMergedBranches: deleted merged branch", "branch", branch, "dir", repoDir)
		}
	}
	if pruned > 0 {
		slog.Info("pruneMergedBranches: pruned merged branches", "count", pruned, "dir", repoDir)
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

// runAutoRetry scans stored sessions for issues with the fordjent/failed:max-turns
// label and retries them up to max_session_retries times. After exhausting retries,
// the issue is permanently blocked.
func (m *Manager) runAutoRetry(ctx context.Context) {
	if !m.cfg.Agent.EnableAutoRetry {
		return
	}
	maxRetries := m.cfg.Agent.MaxSessionRetries
	if maxRetries <= 0 {
		maxRetries = 2
	}

	records, err := m.store.ListAll()
	if err != nil {
		slog.Warn("auto-retry: failed to list stored sessions", "error", err)
		return
	}

	seen := make(map[string]bool)
	for _, rec := range records {
		if rec.IssueNumber <= 0 {
			continue
		}

		issueCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		issue, err := m.forgejoClient.GetIssue(issueCtx, rec.Repository, rec.IssueNumber)
		cancel()
		if err != nil || issue == nil {
			continue
		}

		hasFailedMaxTurns := false
		for _, l := range issue.Labels {
			if l.Name == "fordjent/failed:max-turns" {
				hasFailedMaxTurns = true
				break
			}
		}
		if !hasFailedMaxTurns {
			continue
		}

		if issue.State == "closed" {
			_ = m.forgejoClient.RemoveIssueLabel(ctx, rec.Repository, rec.IssueNumber, "fordjent/failed:max-turns")
			slog.Info("auto-retry: skipping closed issue, cleaning up label",
				"issue", rec.IssueNumber, "repo", rec.Repository)
			continue
		}

		isPR := issue.PullRequest.IsPR()

		var sessionKey string
		if isPR {
			sessionKey = fmt.Sprintf("%s/pulls/%d", rec.Repository, rec.IssueNumber)
		} else {
			sessionKey = fmt.Sprintf("%s/issues/%d", rec.Repository, rec.IssueNumber)
		}
		if seen[sessionKey] {
			continue
		}
		seen[sessionKey] = true

		retryCount, _ := m.lc.CountFailedRetries(ctx, sessionKey)

		if retryCount >= maxRetries {
			slog.Info("auto-retry: max retries exhausted, permanently blocking",
				"issue", rec.IssueNumber, "repo", rec.Repository, "retries", retryCount)
			_ = m.forgejoClient.RemoveIssueLabel(ctx, rec.Repository, rec.IssueNumber, "ready")
			_ = m.forgejoClient.RemoveIssueLabel(ctx, rec.Repository, rec.IssueNumber, "in_progress")
			_ = m.forgejoClient.AddIssueLabels(ctx, rec.Repository, rec.IssueNumber, []string{"blocked", "fordjent/failed:max-retries"})
			_ = m.forgejoClient.RemoveIssueLabel(ctx, rec.Repository, rec.IssueNumber, "fordjent/failed:max-turns")
			body := fmt.Sprintf("Auto-retry exhausted after %d attempts. This issue needs human intervention.\n\n<!-- ford -->", retryCount)
			_ = m.forgejoClient.PostIssueComment(ctx, rec.Repository, rec.IssueNumber, body)
			continue
		}

		slog.Info("auto-retry: retrying failed session",
			"issue", rec.IssueNumber, "repo", rec.Repository, "retry", retryCount+1, "max", maxRetries)

		_ = m.forgejoClient.RemoveIssueLabel(ctx, rec.Repository, rec.IssueNumber, "fordjent/failed:max-turns")
		_ = m.forgejoClient.RemoveIssueLabel(ctx, rec.Repository, rec.IssueNumber, "ready")
		_ = m.forgejoClient.RemoveIssueLabel(ctx, rec.Repository, rec.IssueNumber, "blocked")
		_ = m.forgejoClient.RemoveIssueLabel(ctx, rec.Repository, rec.IssueNumber, "in_progress")

		evt := &event.Event{
			ID:          fmt.Sprintf("auto-retry-%d-%d", rec.IssueNumber, time.Now().UnixNano()),
			Repository:  rec.Repository,
			IssueNumber: rec.IssueNumber,
			Sender:      "fordjent-auto-retry",
		}
		if isPR {
			evt.Type = event.PullRequestOpened
			evt.PRNumber = rec.IssueNumber
			evt.SessionKey = fmt.Sprintf("%s/pulls/%d", rec.Repository, rec.IssueNumber)
		} else {
			evt.Type = event.IssueOpened
			evt.SessionKey = fmt.Sprintf("%s/issues/%d", rec.Repository, rec.IssueNumber)
		}

		_ = m.forgejoClient.AddReaction(ctx, rec.Repository, rec.IssueNumber, 0, "arrows_counterclockwise")

		retryCtx, retryCancel := context.WithTimeout(ctx, 5*time.Minute)
		go func() {
			defer retryCancel()
			m.handleEvent(retryCtx, evt)
		}()
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
		// Prune merged feature branches before removing the workdir
		pruneMergedBranches(rec.RepoDir)

		if err := os.RemoveAll(rec.WorkDir); err != nil {
			slog.Warn("failed to clean up old workdir", "error", err, "dir", rec.WorkDir)
		} else {
			slog.Info("cleaned up old workdir", "session_key", rec.SessionKey, "dir", rec.WorkDir)
		}
	}

	// Also prune merged branches for ALL sessions (not just old ones)
	// If a session has been idle for >1 hour, prune its repo's merged branches.
	m.mu.RLock()
	sessionDirs := make(map[string]time.Time)
	for _, sess := range m.sessions {
		sess.mu.Lock()
		idle := time.Since(sess.LastActive)
		sess.mu.Unlock()
		if idle > 1*time.Hour {
			sessionDirs[sess.RepoDir] = sess.LastActive
		}
	}
	m.mu.RUnlock()
	for dir := range sessionDirs {
		pruneMergedBranches(dir)
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
		writeShutdownCheckpoint(sess.WorkDir, "cancelled", "")
		sess.Cancel()
		m.store.Delete(key)
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
	role = detectRoleFromBody(issue.Body)
	if role != "" {
		return role
	}
	return ""
}

var bodyRolePatterns = []struct {
	role     string
	patterns []string
}{
	{"devops", []string{
		"this is a devops task",
		"role: devops",
		"role:devops",
		"role: infrastructure",
		"as devops",
		"devops should",
		"ci/cd pipeline",
		"ci/cd task",
	}},
	{"pm", []string{
		"this is a pm task",
		"this is a project manager task",
		"role: pm",
		"role:pm",
		"role: project manager",
		"as pm",
		"as project manager",
		"pm should",
		"decompose this",
		"break down this",
		"break down the work",
		"plan and coordinate",
		"coordinate the work",
	}},
	{"reviewer", []string{
		"this is a reviewer task",
		"this is a review task",
		"this is a code review task",
		"role: reviewer",
		"role:reviewer",
		"role: code reviewer",
		"as reviewer",
		"as code reviewer",
		"reviewer should",
	}},
	{"tester", []string{
		"this is a tester task",
		"this is a test task",
		"this is a qa task",
		"role: tester",
		"role:tester",
		"role: test",
		"role: qa",
		"as tester",
		"as qa",
		"tester should",
		"integration test",
	}},
	{"implementer", []string{
		"this is a implementer task",
		"this is a implement task",
		"this is a developer task",
		"this is a dev task",
		"role: implementer",
		"role:implementer",
		"role: implement",
		"role: developer",
		"role:developer",
		"role: dev,",
		"role: dev\n",
		"as implementer",
		"as implement",
		"as developer",
		"as dev,",
		"as dev\n",
		"implementer should",
		"developer should",
		"write code",
	}},
}

func detectRoleFromBody(body string) string {
	if body == "" {
		return ""
	}
	lower := strings.ToLower(body)
	for _, group := range bodyRolePatterns {
		for _, pattern := range group.patterns {
			if strings.Contains(lower, pattern) {
				return group.role
			}
		}
	}
	return ""
}

// unblockSubIssues finds sub-issues that depend on parentNum and transitions them
// from 'planning' to 'ready' by removing the 'planning' label and adding 'ready'.
// This is triggere when a human adds 'plan-approved' to the parent PM issue.
func (m *Manager) unblockSubIssues(ctx context.Context, repo string, parentNum int) {
	issues, err := m.forgejoClient.ListIssues(ctx, repo, "open", 50)
	if err != nil {
		slog.Warn("unblockSubIssues: failed to list issues", "error", err)
		return
	}

	depPattern := fmt.Sprintf("Depends on: #%d", parentNum)
	unblocked := 0
	for _, iss := range issues {
		if !strings.Contains(iss.Body, depPattern) {
			continue
		}
		// Check if this sub-issue has the 'planning' label
		hasPlanning := false
		for _, l := range iss.Labels {
			if l.Name == "planning" {
				hasPlanning = true
				break
			}
		}
		if hasPlanning {
			if err := m.forgejoClient.RemoveIssueLabel(ctx, repo, iss.Number, "planning"); err != nil {
				slog.Warn("unblockSubIssues: failed to remove planning label", "error", err, "issue", iss.Number)
			}
			if err := m.forgejoClient.AddIssueLabels(ctx, repo, iss.Number, []string{"ready"}); err != nil {
				slog.Warn("unblockSubIssues: failed to add ready label", "error", err, "issue", iss.Number)
			}
			unblocked++
			// Add reaction instead of comment — the 'ready' label already signals unblocked
			_ = m.forgejoClient.AddReaction(ctx, repo, iss.Number, 0, "rocket")
			slog.Info("unblocked sub-issue after plan approval", "parent", parentNum, "sub_issue", iss.Number, "repo", repo)
		}
	}
	if unblocked > 0 {
		slog.Info("unblocked sub-issues after plan approval", "parent", parentNum, "count", unblocked, "repo", repo)
	}
}

// OnRalphAppendComplete handles the ralph harness completion.
// On ac_met: true, removes 'ralph' label, adds 'ralph-completed' label.
// Does NOT queue reviewer directly — the routing table handles that on
// the next event (e.g., a comment on the ralph-completed PR).
func (m *Manager) OnRalphAppendComplete(ctx context.Context, repo string, prNumber int) error {
	if m.forgejoClient == nil {
		return nil
	}

	slog.Info("ralph completion: removing ralph label, adding ralph-completed",
		"repo", repo, "pr", prNumber)

	// Remove 'ralph' label
	if err := m.forgejoClient.RemoveIssueLabel(ctx, repo, prNumber, "ralph"); err != nil {
		slog.Warn("ralph completion: failed to remove ralph label", "error", err, "pr", prNumber)
	}

	// Add 'ralph-completed' label
	if err := m.forgejoClient.AddIssueLabels(ctx, repo, prNumber, []string{"ralph-completed"}); err != nil {
		slog.Warn("ralph completion: failed to add ralph-completed label", "error", err, "pr", prNumber)
	}

	return nil
}

// ScanRalphPRs scans for open PRs with the 'ralph' label and spawns
// the next ralph iteration if caps allow. Called by the ralph scheduler ticker.
func (m *Manager) ScanRalphPRs(ctx context.Context) {
	if m.forgejoClient == nil || !m.cfg.Ralph.Enabled {
		return
	}

	repos := m.reposWithRalphLabels()
	slog.Info("ralph scan: scanning repos", "repos", repos)
	for _, repo := range repos {
		m.scanRepoForRalph(ctx, repo)
	}
}

// reposWithRalphLabels returns repos that might have ralph-labeled PRs.
func (m *Manager) reposWithRalphLabels() []string {
	var repos []string
	seen := make(map[string]bool)
	if m.cfg.Scanner.Repo != "" {
		repos = append(repos, m.cfg.Scanner.Repo)
		seen[m.cfg.Scanner.Repo] = true
	}
	// Also check repos from active sessions
	m.mu.RLock()
	for key := range m.sessions {
		if idx := strings.Index(key, "/issues/"); idx > 0 {
			repo := key[:idx]
			if !seen[repo] {
				repos = append(repos, repo)
				seen[repo] = true
			}
		} else if idx := strings.Index(key, "/pulls/"); idx > 0 {
			repo := key[:idx]
			if !seen[repo] {
				repos = append(repos, repo)
				seen[repo] = true
			}
		}
	}
	m.mu.RUnlock()
	return repos
}

// scanRepoForRalph checks a single repo for ralph-labeled PRs and spawns iterations.
func (m *Manager) scanRepoForRalph(ctx context.Context, repo string) {
	prList, err := m.forgejoClient.ListOpenPRsByLabel(ctx, repo, "ralph")
	if err != nil {
		slog.Warn("ralph scan: failed to list PRs", "repo", repo, "error", err)
		return
	}
	slog.Info("ralph scan: found ralph-labeled PRs", "repo", repo, "count", len(prList))

	for _, pr := range prList {
		prKey := fmt.Sprintf("%s/pulls/%d", repo, pr.Number)
		slog.Info("ralph scan: checking PR", "pr", pr.Number, "prKey", prKey)

		// Check if an iteration is already active
		m.mu.RLock()
		active := false
		for k := range m.sessions {
			if strings.HasPrefix(k, prKey+"-ralph-i") {
				active = true
				break
			}
		}
		m.mu.RUnlock()
		if active {
			slog.Info("ralph scan: iteration already active", "pr", pr.Number)
			continue
		}

		// Count existing iterations from lifecycle DB
		var iterCount int
		if m.lc != nil {
			iterCount, _ = m.lc.CountRalphIterations(ctx, prKey)
		}
		slog.Info("ralph scan: iteration count", "pr", pr.Number, "iterCount", iterCount, "max", m.cfg.Ralph.MaxIterationsPerPR)

		// Check iteration cap
		if iterCount >= m.cfg.Ralph.MaxIterationsPerPR {
			slog.Info("ralph scan: max iterations exceeded", "pr", pr.Number, "iterations", iterCount)
			continue
		}

		// Check cooldown — if the last iteration is still running, skip.
		// If completed, we rely on the cooldown timer (the scheduler tick interval
		// itself acts as the cooldown boundary — we don't spawn faster than the
		// ticker interval).
		if m.lc != nil {
			lastIter, err := m.lc.GetLastRalphIteration(ctx, prKey)
			if err == nil && lastIter != nil && lastIter.Status == "running" {
				slog.Info("ralph scan: previous iteration still running", "pr", pr.Number)
				continue
			}
		}

		iterNum := iterCount + 1
		sessKey := fmt.Sprintf("%s-ralph-i%d", prKey, iterNum)
		slog.Info("ralph scan: spawning iteration", "pr", pr.Number, "iteration", iterNum, "key", sessKey)

		// Record the iteration in the lifecycle DB
		if m.lc != nil {
			rec := &lifecycle.RalphRecord{
				PRKey:      prKey,
				Iteration:  iterNum,
				SessionKey: sessKey,
				Status:     "running",
			}
			if err := m.lc.RecordRalphIteration(ctx, rec); err != nil {
				slog.Warn("ralph scan: failed to record iteration", "error", err)
			}
		}

		// Create a synthetic IssueOpened event for the implementer session
		// The session key includes -ralph-iN so the agent will detect it
		ralphEvt := &event.Event{
			Type:        event.IssueOpened,
			Repository:  repo,
			Sender:      "ralph-scheduler",
			IssueNumber:  pr.Number,
			PRNumber:    pr.Number,
			SessionKey:  sessKey,
		}

		go m.handleEvent(ctx, ralphEvt)
	}
}

// hasQuestionLabel checks if an issue has the "question" label.
func hasQuestionLabel(ctx context.Context, client *forgejo.Client, repo string, issueNumber int) bool {
	if client == nil || issueNumber == 0 {
		return false
	}
	iss, err := client.GetIssue(ctx, repo, issueNumber)
	if err != nil {
		return false
	}
	for _, l := range iss.Labels {
		if l.Name == "question" {
			return true
		}
	}
	return false
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
	if strings.Contains(lower, "[implementer]") || strings.Contains(lower, "[implement]") || strings.Contains(lower, "[dev]") || strings.Contains(lower, "[developer]") || strings.Contains(lower, "[scaffold]") {
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
	body := strings.Join([]string{
		"Thanks for creating this issue! I need a **role tag** in the title before I can start working on it.",
		"",
		"## How to assign a role",
		"",
		"Add one of these **tags to the issue title**:",
		"",
		"| Title tag | What I'll do |",
		"|---|---|",
		"| [pm] or [decompose] | Analyze the request and create sub-issues |",
		"| [implementer] or [dev] | Write production code and open a PR |",
		"| [review] or [code review] | Review code, suggest fixes, approve or merge PRs |",
		"| [tester] or [testing] | Write tests and report bugs |",
		"| [devops] or [docker] | Docker, CI/CD, infrastructure changes |",
		"",
		"**Example:** Change \"Add auth system\" to \"[implementer] Add auth system\"",
		"",
		"You can also add a matching **label** (e.g., role:implementer) instead of a title tag.",
		"",
		"## Tags vs Labels",
		"",
		"- **Tags** like [implementer] go in the **issue title** — I detect them automatically.",
		"- **Labels** like role:implementer are added via the issue sidebar — also detected.",
		"- Either one works. You don't need both.",
		"",
		"## Want zero-friction automation?",
		"",
		"By default, I use a **plan-first** policy: PM sub-issues start in planning state and need human approval before implementation begins.",
		"",
		"For full automation with no approval gates, add the **fordjent-yolo** topic to your repo (Settings > Topics).",
		"",
		"<!-- ford -->",
	}, "\n")
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

// handleSpecPRMerged checks if a merged PR is a spec PR and generates
// implementer issues from the tasks.md file.
func (m *Manager) handleSpecPRMerged(ctx context.Context, evt *event.Event) {
	schedCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	info, err := m.specPRManager.IsSpecPR(schedCtx, evt.Repository, evt.PRNumber)
	if err != nil {
		slog.Warn("spec PR: failed to check PR files",
			"error", err, "repo", evt.Repository, "pr", evt.PRNumber)
		return
	}
	if !info.IsSpecPR {
		return // not a spec PR
	}

	slog.Info("spec PR merged: generating implementer issues",
		"change", info.ChangeName, "repo", evt.Repository, "pr", evt.PRNumber)

	// Read tasks.md from the merged repo via Forgejo API (no clone needed).
	// Use the merge commit SHA if available to avoid a race with concurrent merges.
	tasksPath := fmt.Sprintf("openspec/changes/%s/tasks.md", info.ChangeName)
	ref := "main"
	pr, prErr := m.forgejoClient.GetPR(schedCtx, evt.Repository, evt.PRNumber)
	if prErr != nil {
		slog.Debug("spec PR: failed to get PR for merge SHA, falling back to main",
			"error", prErr, "repo", evt.Repository, "pr", evt.PRNumber)
	} else if pr.MergeCommitSHA != "" {
		ref = pr.MergeCommitSHA
		slog.Debug("spec PR: reading tasks.md from merge SHA",
			"sha", ref, "repo", evt.Repository, "pr", evt.PRNumber)
	} else {
		slog.Debug("spec PR: merge SHA unavailable, falling back to main",
			"repo", evt.Repository, "pr", evt.PRNumber)
	}
	file, err := m.forgejoClient.GetFile(schedCtx, evt.Repository, ref, tasksPath)
	if err != nil {
		slog.Warn("spec PR: failed to read tasks.md from repo",
			"error", err, "change", info.ChangeName, "path", tasksPath)
		return
	}

	content := file.DecodedContent()
	tasks, err := speccycle.ParseTasksContent(content)
	if err != nil {
		slog.Warn("spec PR: failed to parse tasks",
			"error", err, "change", info.ChangeName)
		return
	}
	if len(tasks) == 0 {
		slog.Info("spec PR: no tasks found in tasks.md, skipping issue generation",
			"change", info.ChangeName)
		return
	}

	// Create a milestone for the change
	milestoneTitle := fmt.Sprintf("#%d: %s", evt.PRNumber, info.ChangeName)
	ms, err := m.forgejoClient.CreateMilestone(schedCtx, evt.Repository, milestoneTitle, info.ChangeName)
	if err != nil {
		slog.Warn("spec PR: failed to create milestone",
			"error", err, "change", info.ChangeName)
		// Non-fatal: continue without milestone
	}

	// Group tasks: parallel tasks and serial tasks
	var parallelTasks []speccycle.Task
	var serialTasks []speccycle.Task
	var lastSerialIssueNum int

	for _, task := range tasks {
		if task.Done {
			continue // skip already-completed tasks
		}
		if task.Parallel {
			parallelTasks = append(parallelTasks, task)
		} else {
			serialTasks = append(serialTasks, task)
		}
	}

	// Validate parallel tasks for file-path overlaps.
	// mergequeue.CheckGate requires branches that don't exist at issue-creation
	// time, so we do a lightweight heuristic: extract path-like tokens from task
	// descriptions and downgrade overlapping tasks to serial.
	// Detect project language for language-aware path prefix whitelisting.
	repoLang := scaffold.DetectProjectLang(schedCtx, m.forgejoClient, evt.Repository)
	parallelTasks, newSerial, downgradedTasks := validateParallelTasks(parallelTasks, repoLang)
	if len(newSerial) > 0 {
		slog.Info("spec PR: downgraded overlapping parallel tasks to serial",
			"change", info.ChangeName,
			"downgraded", len(newSerial),
		)
		serialTasks = append(serialTasks, newSerial...)
	}

	// Create issues for serial tasks (each depends on the previous)
	var issueSucceeded, issueFailed int
	for _, task := range serialTasks {
		issueBody := fmt.Sprintf(
			"Spec: %s/%s\nTask: %d - %s\nDepends on: #%d",
			info.ChangeName, task.Description,
			task.Index, task.Description,
			evt.PRNumber,
		)
		if lastSerialIssueNum > 0 {
			issueBody += fmt.Sprintf(", #%d", lastSerialIssueNum)
		}
		// Add downgrade annotation for tasks that were originally parallel
		if downgradedTasks[task.Index] {
			issueBody += "\n*(auto-downgraded from parallel)*"
		}

		issueNum, err := m.createSpecIssue(schedCtx, evt.Repository, info.ChangeName, task, issueBody, ms)
		if err != nil {
			slog.Warn("spec PR: failed to create issue",
				"error", err, "task", task.Index)
			issueFailed++
			continue
		}
		issueSucceeded++
		lastSerialIssueNum = issueNum
	}

	// Create issues for parallel tasks (no inter-dependency, but may merge-queue validate)
	for _, task := range parallelTasks {
		issueBody := fmt.Sprintf(
			"Spec: %s/%s\nTask: %d - %s\n[parallel]\nDepends on: #%d",
			info.ChangeName, task.Description,
			task.Index, task.Description,
			evt.PRNumber,
		)

		// If there are serial tasks, parallel tasks also depend on the last serial task
		if lastSerialIssueNum > 0 {
			issueBody += fmt.Sprintf(", #%d", lastSerialIssueNum)
		}

		_, err := m.createSpecIssue(schedCtx, evt.Repository, info.ChangeName, task, issueBody, ms)
		if err != nil {
			slog.Warn("spec PR: failed to create parallel issue",
				"error", err, "task", task.Index)
			issueFailed++
			continue
		}
		issueSucceeded++
	}

	// Post summary comment on the PR
	totalCreated := issueSucceeded + issueFailed
	summary := fmt.Sprintf(
		"✅ Spec **%s** approved and merged. Created %d/%d implementer issue(s) from tasks.md.\n\n",
		info.ChangeName, issueSucceeded, totalCreated,
	)
	for _, t := range serialTasks {
		summary += fmt.Sprintf("- [ ] %s\n", t.Description)
	}
	for _, t := range parallelTasks {
		summary += fmt.Sprintf("- [ ] %s [parallel]\n", t.Description)
	}
	summary += "\n<!-- ford -->"
	_ = m.forgejoClient.PostIssueComment(schedCtx, evt.Repository, evt.PRNumber, summary)

	// Add spec-implementing label to the merged PR
	if err := m.forgejoClient.AddIssueLabels(schedCtx, evt.Repository, evt.PRNumber, []string{"spec-implementing"}); err != nil {
		slog.Warn("spec PR: failed to add spec-implementing label", "error", err, "repo", evt.Repository, "pr", evt.PRNumber)
	}

	slog.Info("spec PR: implementer issues created",
		"change", info.ChangeName,
		"succeeded", issueSucceeded,
		"failed", issueFailed,
	)
}

// validateParallelTasks checks task descriptions for overlapping file paths.
// Tasks that mention the same directory or file pattern are downgraded from
// parallel to serial to prevent merge conflicts. Full merge-queue protection
// still applies at PR creation time when branches exist.
// Returns: parallel tasks, downgraded serial tasks, and a set of task indices
// that were downgraded (for annotation).
func validateParallelTasks(tasks []speccycle.Task, lang string) (parallel []speccycle.Task, serial []speccycle.Task, downgraded map[int]bool) {
	if len(tasks) <= 1 {
		return tasks, nil, nil
	}

	downgraded = make(map[int]bool)

	// Extract path prefixes from each task description.
	type taskPaths struct {
		task   speccycle.Task
		paths  []string // e.g., ["pkg/auth", "internal/forgejo"]
	}
	var entries []taskPaths
	for _, t := range tasks {
		entries = append(entries, taskPaths{task: t, paths: extractPathPrefixes(t.Description, lang)})
	}

	// Greedy conflict resolution: start with first task as parallel, then for each
	// subsequent task check if its paths overlap with any already-accepted parallel task.
	accepted := []taskPaths{entries[0]}
	for i := 1; i < len(entries); i++ {
		conflicts := false
		for _, acc := range accepted {
			if pathsOverlap(entries[i].paths, acc.paths) {
				conflicts = true
				break
			}
		}
		if conflicts {
			serial = append(serial, entries[i].task)
			downgraded[entries[i].task.Index] = true
		} else {
			accepted = append(accepted, entries[i])
		}
	}

	for _, a := range accepted {
		parallel = append(parallel, a.task)
	}
	return parallel, serial, downgraded
}

// pathLikeRe matches path-like tokens: sequences of word chars separated by slashes.
var pathLikeRe = regexp.MustCompile(`[a-zA-Z0-9_]+(?:/[a-zA-Z0-9_.-]+)+`)

// langPrefixes maps programming languages to their first-segment directory prefix whitelists.
// Used by extractPathPrefixes to filter path-like tokens in task descriptions.
var langPrefixes = map[string]map[string]bool{
	"go": {
		"cmd": true, "internal": true, "pkg": true, "api": true,
		"web": true, "docs": true, "configs": true, "scripts": true,
	},
	"python": {
		"src": true, "app": true, "tests": true, "migrations": true,
		"scripts": true, "configs": true, "docs": true,
	},
	"javascript": {
		"lib": true, "test": true, "public": true, "pages": true,
		"components": true, "src": true, "scripts": true, "docs": true,
	},
	"unknown": {},
}

// extractPathPrefixes finds path-like tokens in a task description.
// It looks for sequences like "pkg/auth/handler.go" or "internal/foo".
// Only tokens whose first segment is in the language-specific whitelist (from langPrefixes)
// are accepted. The lang parameter selects the whitelist ("go", "python", "javascript", "unknown").
func extractPathPrefixes(description string, lang string) []string {
	prefixWhitelist, ok := langPrefixes[lang]
	if !ok {
		prefixWhitelist = langPrefixes["unknown"]
	}

	matches := pathLikeRe.FindAllString(description, -1)
	if matches == nil {
		return nil
	}
	// Deduplicate and keep only directory prefixes (strip file names).
	seen := make(map[string]bool)
	var result []string
	for _, m := range matches {
		// Use the first two segments as the directory prefix (e.g., "pkg/auth").
		parts := strings.Split(m, "/")
		if len(parts) >= 2 {
			// Skip matches whose first segment isn't in the language-specific whitelist.
			if !prefixWhitelist[parts[0]] {
				continue
			}
			prefix := parts[0] + "/" + parts[1]
			if !seen[prefix] {
				seen[prefix] = true
				result = append(result, prefix)
			}
		}
	}
	return result
}

// pathsOverlap returns true if two path prefix lists share any entry.
func pathsOverlap(a, b []string) bool {
	set := make(map[string]bool, len(a))
	for _, p := range a {
		set[p] = true
	}
	for _, p := range b {
		if set[p] {
			return true
		}
	}
	return false
}

// createSpecIssue creates a single implementer issue for a spec task.
// Returns the issue number.
func (m *Manager) createSpecIssue(ctx context.Context, repo, changeName string, task speccycle.Task, body string, ms *forgejo.Milestone) (int, error) {
	title := fmt.Sprintf("[implementer] %s: %s", changeName, task.Description)

	issue, err := m.forgejoClient.CreateIssue(ctx, repo, title, body)
	if err != nil {
		return 0, fmt.Errorf("create issue for task %d: %w", task.Index, err)
	}

	// Attach to milestone if created
	if ms != nil {
		_ = m.forgejoClient.SetIssueMilestone(ctx, repo, issue.Number, ms.ID)
	}

	return issue.Number, nil
}

// handleSpecLifecycleLabels transitions spec lifecycle labels when a spec PR is merged.
// spec-proposed → spec-approved on merge.
func (m *Manager) handleSpecLifecycleLabels(ctx context.Context, evt *event.Event) {
	labelCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Check if the PR has the spec-proposed label
	pr, err := m.forgejoClient.GetPR(labelCtx, evt.Repository, evt.PRNumber)
	if err != nil {
		slog.Warn("spec labels: failed to get PR",
			"error", err, "repo", evt.Repository, "pr", evt.PRNumber)
		return
	}

	// Check the PR head branch name — spec PRs use spec/<name> branch
	// This is more reliable than label detection since Forgejo's PR API
	// doesn't always return labels on the PullRequest model.
	if !strings.HasPrefix(pr.Head.Ref, "spec/") {
		slog.Debug("spec labels: PR is not on a spec/ branch",
			"branch", pr.Head.Ref, "repo", evt.Repository, "pr", evt.PRNumber)
		return // not a spec PR
	}

	// Remove spec-proposed, add spec-approved
	if err := m.forgejoClient.RemoveIssueLabel(ctx, evt.Repository, evt.PRNumber, "spec-proposed"); err != nil {
		slog.Warn("spec labels: failed to remove spec-proposed", "error", err, "repo", evt.Repository, "pr", evt.PRNumber)
	}
	if err := m.forgejoClient.AddIssueLabels(ctx, evt.Repository, evt.PRNumber, []string{"spec-approved"}); err != nil {
		slog.Warn("spec labels: failed to add spec-approved", "error", err, "repo", evt.Repository, "pr", evt.PRNumber)
	}

	slog.Info("spec labels: transitioned spec-proposed → spec-approved",
		"repo", evt.Repository, "pr", evt.PRNumber)
}

// runSchedulerReconcile periodically re-checks all blocked issues whose
// dependencies may now be satisfied. This is a safety net for cases where
// the event-driven unblock was missed (network errors, webhook failures,
// restarts). Runs every 2 hours.
func (m *Manager) runSchedulerReconcile(ctx context.Context) {
	if m.scheduler == nil || m.forgejoClient == nil {
		return
	}

	// Get all repos that have had activity
	repos := make(map[string]bool)
	m.mu.RLock()
	for k := range m.sessions {
		parts := strings.SplitN(k, "/", 2)
		if len(parts) == 2 {
			repo := parts[0] + "/" + strings.SplitN(parts[1], "/", 2)[0]
			repos[repo] = true
		}
	}
	m.mu.RUnlock()

	for repo := range repos {
		reconcileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		unblocked, err := m.scheduler.ReconcileBlocked(reconcileCtx, repo)
		cancel()
		if err != nil {
			slog.Warn("scheduler reconcile failed", "repo", repo, "error", err)
		} else if unblocked > 0 {
			slog.Info("scheduler reconcile unblocked issues", "repo", repo, "count", unblocked)
		}
	}
}
