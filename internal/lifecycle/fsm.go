package lifecycle

import "strings"

type IssueState string

const (
	StateOpened       IssueState = "opened"
	StateNeedsRole    IssueState = "needs-role"
	StateReady        IssueState = "ready"
	StateInProgress   IssueState = "in_progress"
	StatePlanning     IssueState = "planning"
	StatePlanApproved IssueState = "plan-approved"
	StateImplementing IssueState = "implementing"
	StateFSMBlocked   IssueState = "blocked"
	StateReview       IssueState = "review"
	StateMerging      IssueState = "merging"
	StateDone         IssueState = "done"
)

var statePriority = map[IssueState]int{
	StateDone:         100,
	StateMerging:      90,
	StateFSMBlocked:   80,
	StateReview:       70,
	StateImplementing: 60,
	StateInProgress:   55,
	StatePlanApproved: 50,
	StatePlanning:     40,
	StateReady:        30,
	StateNeedsRole:    20,
	StateOpened:       0,
}

var labelToState = map[string]IssueState{
	"done":          StateDone,
	"automerge":     StateMerging,
	"blocked":       StateFSMBlocked,
	"in_progress":   StateInProgress,
	"review":        StateReview,
	"implementing":  StateImplementing,
	"plan-approved": StatePlanApproved,
	"planning":      StatePlanning,
	"ready":         StateReady,
	"needs-role":    StateNeedsRole,
}

var allowedTransitions = map[IssueState][]IssueState{
	StateOpened:       {StateNeedsRole, StateReady, StatePlanning, StateImplementing, StateFSMBlocked, StateDone},
	StateNeedsRole:    {StateOpened, StateReady, StatePlanning, StateFSMBlocked, StateDone},
	StateReady:        {StateInProgress, StatePlanning, StatePlanApproved, StateImplementing, StateFSMBlocked, StateDone},
	StateInProgress:   {StateImplementing, StateReady, StateFSMBlocked, StateDone},
	StatePlanning:     {StatePlanApproved, StateFSMBlocked, StateDone},
	StatePlanApproved: {StateImplementing, StateFSMBlocked, StateDone},
	StateImplementing: {StateReview, StateFSMBlocked, StateDone},
	StateFSMBlocked:   {StateReady, StatePlanning, StatePlanApproved, StateImplementing, StateReview, StateDone},
	StateReview:       {StateImplementing, StateMerging, StateDone, StateFSMBlocked},
	StateMerging:      {StateDone, StateReview, StateFSMBlocked},
	StateDone:         {StateReady, StateImplementing},
}

func StateFromLabels(labels []string) IssueState {
	best := StateOpened
	bestPri := 0
	hasBlocked := false
	hasReady := false
	hasFailedLabel := false
	for _, label := range labels {
		name := strings.ToLower(strings.TrimSpace(label))
		if state, ok := labelToState[name]; ok {
			if pri := statePriority[state]; pri > bestPri {
				best = state
				bestPri = pri
			}
		}
		if name == "blocked" {
			hasBlocked = true
		}
		if name == "ready" {
			hasReady = true
		}
		if strings.HasPrefix(name, "fordjent/failed:") {
			hasFailedLabel = true
		}
	}
	if hasBlocked && hasReady {
		if !hasFailedLabel {
			return StateReady
		}
		return StateFSMBlocked
	}
	return best
}

func IsTransitionValid(from, to IssueState) bool {
	allowed, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}
