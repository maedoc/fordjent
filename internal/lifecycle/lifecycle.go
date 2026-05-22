// Package lifecycle implements an event-driven session lifecycle state machine
// for Fordjent. It persists transitions to SQLite so failures and completions
// are observable across restarts.
package lifecycle

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/forgejo"
	_ "modernc.org/sqlite"
)

const (
	StateCreated        = "created"
	StateWorking        = "working"
	StatePRCreated      = "pr_created"
	StateBlocked        = "blocked"
	StateCompleted      = "completed"
	StateFailedMaxTurns = "failed_max_turns"
	StateFailedError    = "failed_error"
)

// Lifecycle tracks session state transitions and surfaces failures via
// Forgejo API labels and comments.
type Lifecycle struct {
	db          *sql.DB
	forgejo     *forgejo.Client
	costTracker *cost.Tracker
	labelPrefix string
	sseMgr      *SubscriberManager
}

// New opens (or creates) the lifecycle SQLite DB and returns a tracker.
func New(dbPath string, client *forgejo.Client, costTracker *cost.Tracker) (*Lifecycle, error) {
	if err := ensureDir(dbPath); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open lifecycle db: %w", err)
	}

	if err := initSchema(db); err != nil {
		return nil, fmt.Errorf("init lifecycle schema: %w", err)
	}

	return &Lifecycle{
		db:          db,
		forgejo:     client,
		costTracker: costTracker,
		labelPrefix: "fordjent/",
		sseMgr:      NewSubscriberManager(100),
	}, nil
}

func (l *Lifecycle) SSEManager() *SubscriberManager {
	return l.sseMgr
}

// RecordTransition inserts a new state row for a session. Each call appends
// a row; the latest row is the current state.
func (l *Lifecycle) RecordTransition(ctx context.Context, sessionKey, from, to, reason string) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO session_transitions (session_key, from_state, to_state, reason, occurred_at)
		 VALUES (?, ?, ?, ?, ?)`,
		sessionKey, from, to, reason, time.Now().UTC(),
	)
	if err != nil {
		slog.Warn("lifecycle: failed to record transition", "error", err, "session_key", sessionKey, "to", to)
	}
	l.sseMgr.Broadcast(SSEEvent{
		Type: "transition",
		Data: fmt.Sprintf(`{"session_key":%q,"from_state":%q,"to_state":%q,"reason":%q,"timestamp":%q}`,
			sessionKey, from, to, reason, time.Now().UTC().Format(time.RFC3339)),
	})
	return err
}

// GetState returns the most recent state for a session.
func (l *Lifecycle) GetState(ctx context.Context, sessionKey string) (string, error) {
	var state string
	err := l.db.QueryRowContext(ctx,
		`SELECT to_state FROM session_transitions
		 WHERE session_key = ? ORDER BY occurred_at DESC LIMIT 1`,
		sessionKey,
	).Scan(&state)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return state, err
}

// OnSessionStart records the transition from created → working.
func (l *Lifecycle) OnSessionStart(ctx context.Context, sessionKey string) {
	current, _ := l.GetState(ctx, sessionKey)
	if current == StateWorking {
		return
	}
	_ = l.RecordTransition(ctx, sessionKey, StateCreated, StateWorking, "session event received")
}

// OnPRCreated records that a PR was created for this session.
func (l *Lifecycle) OnPRCreated(ctx context.Context, sessionKey string, prNumber int) {
	_ = l.RecordTransition(ctx, sessionKey, StateWorking, StatePRCreated, fmt.Sprintf("pr #%d created", prNumber))
}

// OnSessionBlocked records that the session was blocked by the merge queue
// and labels the issue accordingly. Also saves the blocked branch for auto-requeue.
func (l *Lifecycle) OnSessionBlocked(ctx context.Context, repo string, issueNumber int, sessionKey string, branch string) {
	_ = l.RecordTransition(ctx, sessionKey, StateWorking, StateBlocked, "merge queue blocked")

	if l.db != nil && branch != "" {
		_, _ = l.db.ExecContext(ctx, `
			INSERT INTO blocked_branches (repo, branch, issue_number, session_key, status, created_at)
			VALUES (?, ?, ?, ?, 'blocked', ?)
			ON CONFLICT(repo, branch) DO UPDATE SET
				status = 'blocked',
				created_at = excluded.created_at,
				resolved_at = NULL
		`, repo, branch, issueNumber, sessionKey, time.Now().UTC())
	}

	if l.forgejo == nil || issueNumber <= 0 {
		return
	}
	_ = l.forgejo.AddIssueLabels(ctx, repo, issueNumber, []string{"blocked"})
}

// BlockedBranch represents a queued branch waiting for merge-gate clearance.
type BlockedBranch struct {
	Repo        string    `json:"repo"`
	Branch      string    `json:"branch"`
	IssueNumber int       `json:"issue_number"`
	SessionKey  string    `json:"session_key"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListBlockedBranches returns all currently blocked branches for a repo.
func (l *Lifecycle) ListBlockedBranches(ctx context.Context, repo string) ([]BlockedBranch, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT repo, branch, issue_number, session_key, status, created_at
		FROM blocked_branches
		WHERE repo = ? AND status = 'blocked'
		ORDER BY created_at DESC
	`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlockedBranch
	for rows.Next() {
		var b BlockedBranch
		var t time.Time
		_ = rows.Scan(&b.Repo, &b.Branch, &b.IssueNumber, &b.SessionKey, &b.Status, &t)
		b.CreatedAt = t
		out = append(out, b)
	}
	return out, nil
}

// ResolveBlockedBranch marks a branch as resolved.
func (l *Lifecycle) ResolveBlockedBranch(ctx context.Context, repo, branch string) error {
	_, _ = l.db.ExecContext(ctx, `
		UPDATE blocked_branches
		SET status = 'resolved', resolved_at = ?
		WHERE repo = ? AND branch = ?
	`, time.Now().UTC(), repo, branch)
	return nil
}

// OnSessionComplete records a successful completion and optionally posts a cost summary.
func (l *Lifecycle) OnSessionComplete(ctx context.Context, sessionKey, repo string, issueNumber int, role string, headSHA string) {
	_ = l.RecordTransition(ctx, sessionKey, StateWorking, StateCompleted, "session finished successfully")

	if l.forgejo == nil || issueNumber <= 0 {
		return
	}
	roleLabel := "implementation"
	if role == "reviewer" {
		roleLabel = "code review"
	} else if role == "pm" {
		roleLabel = "project management"
	} else if role == "tester" {
		roleLabel = "testing"
	} else if role == "devops" {
		roleLabel = "devops"
	}

	description := "Session completed"
	var totalTokens int64
	var totalCost float64
	if l.costTracker != nil {
		tokens, cost, _ := l.costTracker.GetSessionCost(sessionKey)
		totalTokens = tokens
		totalCost = cost
		if tokens > 0 {
			description = fmt.Sprintf("%s: %.0f tokens ($%.4f USD)", roleLabel, float64(tokens), cost)
		}
	}

	// Post commit status on the head SHA if available (shows green checkmark in PR UI)
	if headSHA != "" {
		_ = l.forgejo.CreateCommitStatus(ctx, repo, headSHA, "success",
			"fordjent/agent", description, "")
	}

	// Add a reaction to the issue (visible in UI, no comment noise)
	_ = l.forgejo.AddReaction(ctx, repo, issueNumber, 0, "white_check_mark")

	// Only post a cost comment if there is meaningful cost data.
	// This drastically reduces comment noise while preserving cost transparency.
	if totalTokens > 0 {
		msg := fmt.Sprintf("Session completed (%s): %.0f tokens, $%.4f USD\n\n<!-- ford -->", roleLabel, float64(totalTokens), totalCost)
		_ = l.forgejo.PostIssueComment(ctx, repo, issueNumber, msg)
	}
}

// OnSessionFailedMaxTurns records that the session exhausted its turn budget.
func (l *Lifecycle) OnSessionFailedMaxTurns(ctx context.Context, repo string, issueNumber int, sessionKey string) {
	_ = l.RecordTransition(ctx, sessionKey, StateWorking, StateFailedMaxTurns,
		fmt.Sprintf("reached max turns on issue #%d", issueNumber))

	if l.forgejo == nil {
		return
	}
	if issueNumber <= 0 {
		return
	}

	_ = l.forgejo.RemoveIssueLabel(ctx, repo, issueNumber, "ready")
	_ = l.forgejo.RemoveIssueLabel(ctx, repo, issueNumber, "in_progress")
	_ = l.forgejo.AddIssueLabels(ctx, repo, issueNumber, []string{"fordjent/failed:max-turns"})

	body := "Max turns reached. Auto-retry may be attempted.\n\n\x3c!-- ford --\x3e"
	if err := l.postIssueComment(ctx, repo, issueNumber, body); err != nil {
		slog.Warn("lifecycle: failed to post max-turns comment", "error", err, "issue", issueNumber)
	}
	_ = l.forgejo.AddReaction(ctx, repo, issueNumber, 0, "x")
}

// OnSessionFailedError records an arbitrary runtime error that killed the session.
func (l *Lifecycle) OnSessionFailedError(ctx context.Context, repo string, issueNumber int, sessionKey string, runErr error) {
	reason := "session encountered an error"
	if runErr != nil {
		reason = runErr.Error()
	}
	_ = l.RecordTransition(ctx, sessionKey, StateWorking, StateFailedError, reason)

	if l.forgejo == nil {
		return
	}
	if issueNumber <= 0 {
		return
	}

	_ = l.forgejo.RemoveIssueLabel(ctx, repo, issueNumber, "ready")
	_ = l.forgejo.RemoveIssueLabel(ctx, repo, issueNumber, "in_progress")
	_ = l.forgejo.AddIssueLabels(ctx, repo, issueNumber, []string{"fordjent/failed:error"})
	_ = l.forgejo.AddIssueLabels(ctx, repo, issueNumber, []string{"blocked"})

	body := fmt.Sprintf("Session error: %s\n\n<!-- ford -->", reason)
	if err := l.postIssueComment(ctx, repo, issueNumber, body); err != nil {
		slog.Warn("lifecycle: failed to post error comment", "error", err, "issue", issueNumber)
	}
	_ = l.forgejo.AddReaction(ctx, repo, issueNumber, 0, "x")
}

// CountFailedRetries counts how many times a session has hit failed_max_turns.
// This is used by the auto-retry logic to determine whether an issue has
// exhausted its retry budget.
func (l *Lifecycle) CountFailedRetries(ctx context.Context, sessionKey string) (int, error) {
	var count int
	err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_transitions WHERE session_key = ? AND to_state = ?`,
		sessionKey, StateFailedMaxTurns,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ListFailedSessions returns session keys currently in a failed state.
func (l *Lifecycle) ListFailedSessions(ctx context.Context) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT session_key FROM session_transitions t1
		WHERE occurred_at = (
			SELECT MAX(occurred_at) FROM session_transitions t2 WHERE t2.session_key = t1.session_key
		)
		AND t1.to_state IN (?, ?)
	`, StateFailedMaxTurns, StateFailedError)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			continue
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (l *Lifecycle) postIssueComment(ctx context.Context, repo string, issueNumber int, body string) error {
	return l.forgejo.PostIssueComment(ctx, repo, issueNumber, body)
}

func ensureDir(dbPath string) error {
	dir := filepath.Dir(dbPath)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0755)
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS session_transitions (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			session_key TEXT NOT NULL,
			from_state  TEXT,
			to_state    TEXT NOT NULL,
			reason      TEXT,
			occurred_at DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_transitions_session ON session_transitions(session_key);
		CREATE INDEX IF NOT EXISTS idx_transitions_time    ON session_transitions(occurred_at);

		CREATE TABLE IF NOT EXISTS blocked_branches (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			repo         TEXT NOT NULL,
			branch       TEXT NOT NULL,
			issue_number INTEGER NOT NULL DEFAULT 0,
			session_key  TEXT NOT NULL,
			status       TEXT NOT NULL DEFAULT 'blocked',
			created_at   DATETIME NOT NULL,
			resolved_at  DATETIME
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_blocked_branch ON blocked_branches(repo, branch);

		CREATE TABLE IF NOT EXISTS session_turns (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			session_key  TEXT NOT NULL,
			turn         INTEGER NOT NULL,
			tool_calls   INTEGER NOT NULL DEFAULT 0,
			latency_ms   INTEGER NOT NULL DEFAULT 0,
			tokens_in    INTEGER NOT NULL DEFAULT 0,
			tokens_out   INTEGER NOT NULL DEFAULT 0,
			error        TEXT,
			occurred_at  DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_turns_session ON session_turns(session_key);

		CREATE TABLE IF NOT EXISTS webhook_deliveries (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
		event_type   TEXT NOT NULL,
		action       TEXT NOT NULL DEFAULT '',
		repository   TEXT NOT NULL,
		number       INTEGER NOT NULL DEFAULT 0,
		sender       TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'accepted',
		error        TEXT,
		occurred_at  DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_webhook_time ON webhook_deliveries(occurred_at);
	`)
	return err
}

// RecordTurn persists per-turn progress data for diagnostics.
func (l *Lifecycle) RecordTurn(ctx context.Context, sessionKey string, turn, toolCalls, latencyMs, tokensIn, tokensOut int, turnErr error) {
	var errStr string
	if turnErr != nil {
		errStr = turnErr.Error()
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO session_turns (session_key, turn, tool_calls, latency_ms, tokens_in, tokens_out, error, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionKey, turn, toolCalls, latencyMs, tokensIn, tokensOut, errStr, time.Now().UTC(),
	)
	if err != nil {
		slog.Warn("lifecycle: failed to record turn", "error", err, "session_key", sessionKey)
	}
	errMsg := ""
	if turnErr != nil {
		errMsg = turnErr.Error()
	}
	l.sseMgr.Broadcast(SSEEvent{
		Type: "turn",
		Data: fmt.Sprintf(`{"session_key":%q,"turn":%d,"tool_calls":%d,"latency_ms":%d,"tokens_in":%d,"tokens_out":%d,"error":%q,"timestamp":%q}`,
			sessionKey, turn, toolCalls, latencyMs, tokensIn, tokensOut, errMsg, time.Now().UTC().Format(time.RFC3339)),
	})
}

// RecordDelivery logs a webhook delivery to the database for tracking.
func (l *Lifecycle) RecordDelivery(ctx context.Context, eventType, action, repo string, number int, sender, status string, deliveryErr error) {
	var errStr string
	if deliveryErr != nil {
		errStr = deliveryErr.Error()
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (event_type, action, repository, number, sender, status, error, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		eventType, action, repo, number, sender, status, errStr, time.Now().UTC(),
	)
	if err != nil {
		slog.Warn("lifecycle: failed to record webhook delivery", "error", err)
	}
	delErrStr := ""
	if deliveryErr != nil {
		delErrStr = deliveryErr.Error()
	}
	l.sseMgr.Broadcast(SSEEvent{
		Type: "delivery",
		Data: fmt.Sprintf(`{"event_type":%q,"action":%q,"repository":%q,"number":%d,"sender":%q,"status":%q,"error":%q,"timestamp":%q}`,
			eventType, action, repo, number, sender, status, delErrStr, time.Now().UTC().Format(time.RFC3339)),
	})
}
