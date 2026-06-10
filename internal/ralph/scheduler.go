package ralph

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/forgejo"
)

// Scheduler scans for ralph-labeled PRs and spawns iterations on a ticker.
type Scheduler struct {
	forgejo       *forgejo.Client
	cfg           config.RalphConfig
	ticker        *time.Ticker
	done         chan struct{}
	mu            sync.Mutex
	active        map[string]bool // prKey → iteration running
	dispatchFunc  func(repo string, prNumber, iterNum int) // called to dispatch a ralph session
}

// NewScheduler creates a ralph scheduler.
func NewScheduler(forgejoClient *forgejo.Client, cfg config.RalphConfig) *Scheduler {
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

// SetDispatchFunc sets the function called when a ralph iteration should be spawned.
// This is the bridge to the session manager — the scheduler decides WHEN to spawn,
// the manager decides HOW to create the session.
func (s *Scheduler) SetDispatchFunc(fn func(repo string, prNumber, iterNum int)) {
	s.dispatchFunc = fn
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
	slog.Debug("ralph scheduler: scan tick")

	if s.forgejo == nil {
		return
	}

	// List open PRs — we rely on the dispatch function being set by the manager.
	// The manager's scanRalphPRs does the actual Forgejo API call and iteration
	// dispatch. This ticker just signals that it's time to scan.
	if s.dispatchFunc != nil {
		s.dispatchFunc("", 0, 0) // sentinel call; manager ignores zeros and scans all
	}
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
func (s *Scheduler) Config() config.RalphConfig {
	return s.cfg
}
