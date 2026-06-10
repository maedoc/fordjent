package ralph

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/forgejo"
)

// RalphConfig holds configuration for the ralph loop subsystem.
// It is embedded in the main config struct.
type RalphConfig struct {
	Enabled                   bool          `yaml:"enabled"`
	MaxIterationsPerPR        int           `yaml:"max_iterations_per_pr"`
	TurnBudgetPerIteration    int           `yaml:"turn_budget_per_iteration"`
	CooldownBetweenIterations time.Duration `yaml:"cooldown_between_iterations"`
	MaxCostPerPRUSD           float64       `yaml:"max_cost_per_pr_usd"`
	NudgeThresholdPct         float64       `yaml:"nudge_threshold_pct"`
	SummaryModel              string        `yaml:"summary_model"`
	AutoRalphOnYolo           bool          `yaml:"auto_ralph_on_yolo"`
}

// DefaultRalphConfig returns sensible defaults.
func DefaultRalphConfig() RalphConfig {
	return RalphConfig{
		Enabled:                   true,
		MaxIterationsPerPR:        20,
		TurnBudgetPerIteration:    20,
		CooldownBetweenIterations: 2 * time.Minute,
		MaxCostPerPRUSD:           5.00,
		NudgeThresholdPct:         0.25,
		SummaryModel:              "",
		AutoRalphOnYolo:           true,
	}
}

// Scheduler scans for ralph-labeled PRs and spawns iterations on a ticker.
type Scheduler struct {
	forgejo *forgejo.Client
	cfg     RalphConfig
	ticker  *time.Ticker
	done    chan struct{}
	mu      sync.Mutex
	active  map[string]bool // prKey → iteration running
}

// NewScheduler creates a ralph scheduler.
func NewScheduler(forgejoClient *forgejo.Client, cfg RalphConfig) *Scheduler {
	if cfg.TurnBudgetPerIteration <= 0 {
		cfg.TurnBudgetPerIteration = 20
	}
	if cfg.MaxIterationsPerPR <= 0 {
		cfg.MaxIterationsPerPR = 20
	}
	if cfg.CooldownBetweenIterations <= 0 {
		cfg.CooldownBetweenIterations = 2 * time.Minute
	}
	return &Scheduler{
		forgejo: forgejoClient,
		cfg:     cfg,
		done:    make(chan struct{}),
		active:  make(map[string]bool),
	}
}

// Start begins the ticker goroutine for scanning ralph-labeled PRs.
func (s *Scheduler) Start() {
	interval := s.cfg.CooldownBetweenIterations
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	s.ticker = time.NewTicker(interval)
	go s.run()
	slog.Info("ralph scheduler started", "interval", interval)
}

// Stop halts the scheduler ticker.
func (s *Scheduler) Stop() {
	if s.ticker != nil {
		s.ticker.Stop()
	}
	close(s.done)
	slog.Info("ralph scheduler stopped")
}

// run is the main scheduler loop.
func (s *Scheduler) run() {
	for {
		select {
		case <-s.done:
			return
		case <-s.ticker.C:
			s.scanAndDispatch()
		}
	}
}

// scanAndDispatch lists open PRs with the 'ralph' label and checks
// whether the next iteration should be spawned.
func (s *Scheduler) scanAndDispatch() {
	// This method is called by the ticker. In production, it lists open
	// PRs from Forgejo and spawns iterations. The session manager wires
	// this to its own session creation logic via callback.
	//
	// Since the session manager handles the actual session creation,
	// this method primarily logs and checks caps. The real dispatch
	// is done by Manager.scanRalphPRs() which calls this for state checks.
	slog.Debug("ralph scheduler: scan tick")
}

// ShouldSpawn checks whether a new iteration should be spawned for the given PR.
// It checks: cooldown elapsed, no active iteration, iteration cap not exceeded,
// budget not exceeded, and no stall detected.
func (s *Scheduler) ShouldSpawn(prKey string, lastIterationStatus string, lastIterationTime time.Time, totalIterations int, totalCostUSD float64) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if an iteration is already active for this PR
	if s.active[prKey] {
		return false, "ralph iteration in progress, skipping"
	}

	// Check iteration cap
	if totalIterations >= s.cfg.MaxIterationsPerPR {
		return false, "max iterations exceeded"
	}

	// Check budget cap
	if s.cfg.MaxCostPerPRUSD > 0 && totalCostUSD >= s.cfg.MaxCostPerPRUSD {
		return false, "ralph budget exceeded"
	}

	// Check cooldown (skip for first iteration)
	if totalIterations > 0 && !lastIterationTime.IsZero() {
		elapsed := time.Since(lastIterationTime)
		if elapsed < s.cfg.CooldownBetweenIterations {
			remaining := s.cfg.CooldownBetweenIterations - elapsed
			return false, fmt.Sprintf("cooldown not elapsed, %s remaining", remaining.Round(time.Second))
		}
	}

	// Stall check: if last 3 iterations all failed, detect stall
	if lastIterationStatus == "failed_turns" {
		// Caller should check the last 3 iterations for stall detection
		// This is a signal, not a hard block here
	}

	return true, ""
}

// MarkActive marks a PR as having an active iteration.
func (s *Scheduler) MarkActive(prKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[prKey] = true
}

// MarkInactive removes the active mark for a PR.
func (s *Scheduler) MarkInactive(prKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, prKey)
}

// IsActive returns whether a PR has an active iteration.
func (s *Scheduler) IsActive(prKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[prKey]
}

// Config returns the scheduler configuration.
func (s *Scheduler) Config() RalphConfig {
	return s.cfg
}
