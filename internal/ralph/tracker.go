// Package ralph implements the iterative PR refinement engine for Fordjent.
// It provides stage tracking (4-A protocol), progress file management,
// spec immutability enforcement, and scheduler-driven iteration spawning.
package ralph

import (
	"fmt"
	"sync"
)

// Stage constants for the 4-A protocol.
const (
	StageAwareness = "awareness"
	StageAct       = "act"
	StageAssert    = "assert"
	StageAppend    = "append"
)

// validStages is the ordered set of 4-A stages.
var validStages = []string{StageAwareness, StageAct, StageAssert, StageAppend}

// Tracker tracks stage completion within a single Ralph iteration.
// It enforces the 4-A ordering (awareness → act → assert → append)
// and provides turn-based nudging.
type Tracker struct {
	mu              sync.Mutex
	stagesCompleted map[string]bool
	stageMessages   map[string]string
	turnBudget      int
	nudgeThresholds []float64
	expectedOrder   []string
}

// NewTracker creates a Tracker for a single Ralph iteration.
// turnBudget is the maximum number of turns for this iteration.
func NewTracker(turnBudget int) *Tracker {
	return &Tracker{
		stagesCompleted: make(map[string]bool),
		stageMessages:   make(map[string]string),
		turnBudget:      turnBudget,
		nudgeThresholds: []float64{0.25, 0.50, 0.75},
		expectedOrder:   validStages,
	}
}

// RecordStage records that the agent has completed a stage.
// It validates ordering: awareness must precede act, act must precede assert, etc.
// Duplicate calls are idempotent (the message is overwritten).
func (t *Tracker) RecordStage(stage, message string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Validate stage name
	if !isValidStage(stage) {
		return fmt.Errorf("invalid stage %q; must be one of: awareness, act, assert, append", stage)
	}

	// Validate ordering: all prerequisite stages must be completed
	for _, prerequisite := range t.expectedOrder {
		if prerequisite == stage {
			break
		}
		if !t.stagesCompleted[prerequisite] {
			return fmt.Errorf("must complete %q before %q. Current completed stages: %v", prerequisite, stage, t.completedStages())
		}
	}

	// Idempotent: overwrite message if stage already recorded
	t.stagesCompleted[stage] = true
	t.stageMessages[stage] = message
	return nil
}

// IsComplete returns true if all 4 stages have been completed.
func (t *Tracker) IsComplete() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, stage := range t.expectedOrder {
		if !t.stagesCompleted[stage] {
			return false
		}
	}
	return true
}

// ShouldNudge returns a nudge message if the agent is behind schedule
// based on turn budget consumption. Returns ("", false) if no nudge is needed.
// Thresholds: 25% → nudge awareness, 50% → act, 75% → assert.
// Final 2 turns → urgent nudge to append.
func (t *Tracker) ShouldNudge(turn int) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.turnBudget <= 0 {
		return "", false
	}

	pct := float64(turn) / float64(t.turnBudget)

	// Urgent: final 2 turns, append not done
	if turn >= t.turnBudget-2 && !t.stagesCompleted[StageAppend] {
		return fmt.Sprintf("[RALPH URGENT] Only %d turn(s) remaining. Call ralph_update with stage='append' and commit your work NOW.", t.turnBudget-turn), true
	}

	// 75% threshold: nudge assert
	if pct >= 0.75 && !t.stagesCompleted[StageAssert] {
		return "[RALPH NUDGE] 75% budget used. Call ralph_update with stage='assert' immediately. Document what passes and what remains.", true
	}

	// 50% threshold: nudge act
	if pct >= 0.50 && !t.stagesCompleted[StageAct] {
		return "[RALPH NUDGE] 50% of budget consumed. Call ralph_update with stage='act' and proceed with implementation.", true
	}

	// 25% threshold: nudge awareness
	if pct >= 0.25 && !t.stagesCompleted[StageAwareness] {
		return "[RALPH NUDGE] You are at 25% of your turn budget. Begin the protocol: call ralph_update with stage='awareness'.", true
	}

	return "", false
}

// CompletedStages returns a copy of the completed stages map.
func (t *Tracker) CompletedStages() map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make(map[string]bool, len(t.stagesCompleted))
	for k, v := range t.stagesCompleted {
		result[k] = v
	}
	return result
}

// StageMessages returns a copy of the stage messages.
func (t *Tracker) StageMessages() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make(map[string]string, len(t.stageMessages))
	for k, v := range t.stageMessages {
		result[k] = v
	}
	return result
}

// Reset clears all stage tracking for a new iteration.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stagesCompleted = make(map[string]bool)
	t.stageMessages = make(map[string]string)
}

// completedStages returns the list of stage names that have been completed.
// Must be called with lock held.
func (t *Tracker) completedStages() []string {
	var result []string
	for _, stage := range t.expectedOrder {
		if t.stagesCompleted[stage] {
			result = append(result, stage)
		}
	}
	return result
}

// isValidStage checks if the given string is a valid 4-A stage name.
func isValidStage(stage string) bool {
	for _, s := range validStages {
		if s == stage {
			return true
		}
	}
	return false
}
