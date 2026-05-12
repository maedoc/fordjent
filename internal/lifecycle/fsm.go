package lifecycle

import "strings"

type IssueState string

const (
	StateOpened       IssueState = "opened"
	StateNeedsRole    IssueState = "needs-role"
	StateReady        IssueState = "ready"
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
	"review":        StateReview,
	"implementing":  StateImplementing,
	"plan-approved": StatePlanApproved,
	"planning":      StatePlanning,
	"ready":         StateReady,
	"needs-role":    StateNeedsRole,
}

var allowedTransitions = map[IssueState][]IssueState{
	StateOpened:       {StateNeedsRole, StateReady, StatePlanning, StateFSMBlocked},
	StateNeedsRole:    {StateReady, StatePlanning, StateFSMBlocked},
	StateReady:        {StatePlanning, StateImplementing, StateFSMBlocked},
	StatePlanning:     {StatePlanApproved, StateFSMBlocked, StateDone},
	StatePlanApproved: {StateImplementing, StateFSMBlocked},
	StateImplementing: {StateReview, StateFSMBlocked, StateDone},
	StateFSMBlocked:   {StateReady, StatePlanning, StateImplementing, StateReview, StateDone},
	StateReview:       {StateImplementing, StateMerging, StateDone, StateFSMBlocked},
	StateMerging:      {StateDone, StateReview, StateFSMBlocked},
	StateDone:         {StateReady, StateImplementing},
}

func StateFromLabels(labels []string) IssueState {
	best := StateOpened
	bestPri := 0
	for _, label := range labels {
		name := strings.ToLower(strings.TrimSpace(label))
		if state, ok := labelToState[name]; ok {
			if pri := statePriority[state]; pri > bestPri {
				best = state
				bestPri = pri
			}
		}
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
