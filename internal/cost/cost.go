package cost

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// UsageRecord tracks a single LLM call.
type UsageRecord struct {
	SessionKey       string
	ProviderName     string
	Model            string
	Repository       string
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CostUSD          float64
	Timestamp        time.Time
}

// Tracker persists and queries cost data.
type Tracker struct {
	db *sql.DB
	mu sync.Mutex
}

// NewTracker creates a cost tracker backed by SQLite.
func NewTracker(dbPath string) (*Tracker, error) {
	if dbPath == "" {
		dbPath = "costs.db"
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open cost db: %w", err)
	}
	t := &Tracker{db: db}
	if err := t.migrate(); err != nil {
		return nil, fmt.Errorf("migrate cost db: %w", err)
	}
	return t, nil
}

func (t *Tracker) migrate() error {
	_, err := t.db.Exec(`
CREATE TABLE IF NOT EXISTS usage (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_key TEXT NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	repository TEXT,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_usage_session ON usage(session_key);
CREATE INDEX IF NOT EXISTS idx_usage_repo ON usage(repository);
CREATE INDEX IF NOT EXISTS idx_usage_created ON usage(created_at);
`)
	return err
}

// Record saves a usage record to the database.
func (t *Tracker) Record(r *UsageRecord) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	_, err := t.db.Exec(
		`INSERT INTO usage (session_key, provider, model, repository, input_tokens, output_tokens, total_tokens, cost_usd, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.SessionKey, r.ProviderName, r.Model, r.Repository,
		r.InputTokens, r.OutputTokens, r.TotalTokens, r.CostUSD, r.Timestamp,
	)
	if err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}
	return nil
}

// GetSessionCost returns total cost and tokens for a session.
func (t *Tracker) GetSessionCost(sessionKey string) (tokens int64, cost float64, err error) {
	row := t.db.QueryRow(
		`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM usage WHERE session_key = ?`,
		sessionKey,
	)
	var totalTokens sql.NullInt64
	var totalCost sql.NullFloat64
	err = row.Scan(&totalTokens, &totalCost)
	if err != nil {
		return 0, 0, fmt.Errorf("query session cost: %w", err)
	}
	return totalTokens.Int64, totalCost.Float64, nil
}

// GetRepoCost returns total cost for a repository.
func (t *Tracker) GetRepoCost(repo string) float64 {
	row := t.db.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0) FROM usage WHERE repository = ?`,
		repo,
	)
	var totalCost sql.NullFloat64
	if err := row.Scan(&totalCost); err != nil {
		slog.Warn("failed to query repo cost", "repo", repo, "error", err)
		return 0
	}
	return totalCost.Float64
}

// GetMonthlyCost returns total cost for the current month.
func (t *Tracker) GetMonthlyCost() float64 {
	row := t.db.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0) FROM usage 
		WHERE strftime('%Y-%m', created_at) = strftime('%Y-%m', 'now')`,
	)
	var totalCost sql.NullFloat64
	if err := row.Scan(&totalCost); err != nil {
		slog.Warn("failed to query monthly cost", "error", err)
		return 0
	}
	return totalCost.Float64
}

// Close closes the underlying database connection.
func (t *Tracker) Close() error {
	return t.db.Close()
}

// CheckBudget returns true if the session and monthly budgets are still within limits.
func (t *Tracker) CheckBudget(sessionKey string, budgetEnabled bool, maxSessionCost, maxMonthlyCost float64) (allowed bool, reason string) {
	if !budgetEnabled {
		return true, ""
	}
	if maxSessionCost > 0 {
		_, sessionCost, err := t.GetSessionCost(sessionKey)
		if err != nil {
			slog.Warn("failed to get session cost for budget check", "error", err)
		}
		if sessionCost >= maxSessionCost {
			return false, fmt.Sprintf("session budget $%.4f exceeded (current $%.4f)", maxSessionCost, sessionCost)
		}
	}
	if maxMonthlyCost > 0 {
		monthlyCost := t.GetMonthlyCost()
		if monthlyCost >= maxMonthlyCost {
			return false, fmt.Sprintf("monthly budget $%.4f exceeded (current $%.4f)", maxMonthlyCost, monthlyCost)
		}
	}
	return true, ""
}

// TokenMinute aggregates input/output tokens for a single minute.
type TokenMinute struct {
	Minute       string `json:"minute"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Calls        int64  `json:"calls"`
}

// GetPerMinuteTokens aggregates token usage by minute for the last N hours.
func (t *Tracker) GetPerMinuteTokens(hours int) ([]TokenMinute, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Format("2006-01-02 15:04:05")
	rows, err := t.db.Query(`
		SELECT strftime('%Y-%m-%d %H:%M', created_at) as minute,
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COUNT(*)
		FROM usage
		WHERE created_at >= ?
		GROUP BY minute
		ORDER BY minute DESC
		LIMIT 500
	`, since)
	if err != nil {
		return nil, fmt.Errorf("query per-minute tokens: %w", err)
	}
	defer rows.Close()

	var out []TokenMinute
	for rows.Next() {
		var m TokenMinute
		_ = rows.Scan(&m.Minute, &m.InputTokens, &m.OutputTokens, &m.TotalTokens, &m.Calls)
		out = append(out, m)
	}
	return out, rows.Err()
}
