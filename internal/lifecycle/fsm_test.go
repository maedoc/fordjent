package lifecycle

import "testing"

func TestStateFromLabels(t *testing.T) {
	tests := []struct {
		labels []string
		want   IssueState
	}{
		{nil, StateOpened},
		{[]string{}, StateOpened},
		{[]string{"needs-role"}, StateNeedsRole},
		{[]string{"ready"}, StateReady},
		{[]string{"blocked", "ready"}, StateReady},
		{[]string{"blocked", "ready", "fordjent/failed:max-turns"}, StateFSMBlocked},
		{[]string{"blocked", "ready", "fordjent/failed:error"}, StateFSMBlocked},
		{[]string{"blocked", "ready", "bug"}, StateReady},
		{[]string{"implementing"}, StateImplementing},
		{[]string{"review", "automerge"}, StateMerging},
		{[]string{"done"}, StateDone},
		{[]string{"planning"}, StatePlanning},
		{[]string{"plan-approved"}, StatePlanApproved},
		{[]string{"bug", "enhancement"}, StateOpened},
		{[]string{"bug", "implementing"}, StateImplementing},
		{[]string{"Planning"}, StatePlanning},
		{[]string{"  planning  "}, StatePlanning},
		{[]string{"BLOCKED"}, StateFSMBlocked},
	}
	for _, tt := range tests {
		got := StateFromLabels(tt.labels)
		if got != tt.want {
			t.Errorf("StateFromLabels(%v) = %q, want %q", tt.labels, got, tt.want)
		}
	}
}

func TestIsTransitionValid(t *testing.T) {
	valid := []struct{ from, to IssueState }{
		{StateOpened, StateNeedsRole},
		{StateOpened, StateReady},
		{StateOpened, StatePlanning},
		{StateOpened, StateFSMBlocked},
		{StateNeedsRole, StateReady},
		{StateReady, StateImplementing},
		{StateReady, StatePlanning},
		{StatePlanning, StatePlanApproved},
		{StatePlanning, StateDone},
		{StatePlanApproved, StateImplementing},
		{StateImplementing, StateReview},
		{StateImplementing, StateDone},
		{StateFSMBlocked, StateReady},
		{StateFSMBlocked, StateImplementing},
		{StateFSMBlocked, StateDone},
		{StateReview, StateMerging},
		{StateReview, StateDone},
		{StateReview, StateFSMBlocked},
		{StateMerging, StateDone},
		{StateDone, StateReady},
		{StateDone, StateImplementing},
	}
	for _, tt := range valid {
		if !IsTransitionValid(tt.from, tt.to) {
			t.Errorf("IsTransitionValid(%s, %s) should be true", tt.from, tt.to)
		}
	}

	invalid := []struct{ from, to IssueState }{
		{StateDone, StatePlanning},
		{StateDone, StateFSMBlocked},
		{StateOpened, StateMerging},
		{StateOpened, StateReview},
		{StateNeedsRole, StateImplementing},
		{StatePlanning, StateImplementing},
		{StateImplementing, StatePlanning},
	}
	for _, tt := range invalid {
		if IsTransitionValid(tt.from, tt.to) {
			t.Errorf("IsTransitionValid(%s, %s) should be false", tt.from, tt.to)
		}
	}
}

func TestIsTransitionValidUnknownFrom(t *testing.T) {
	if IsTransitionValid(IssueState("unknown"), StateReady) {
		t.Error("transition from unknown state should be invalid")
	}
}
