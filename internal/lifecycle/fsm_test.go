package lifecycle

import "testing"

func TestStateFromLabels(t *testing.T) {
	tests := []struct {
		labels []string
		want   IssueState
	}{
		{nil, StateOpened},
		{[]string{"needs-role"}, StateNeedsRole},
		{[]string{"ready"}, StateReady},
		{[]string{"blocked", "ready"}, StateFSMBlocked},
		{[]string{"implementing"}, StateImplementing},
		{[]string{"review", "automerge"}, StateMerging},
		{[]string{"done"}, StateDone},
		{[]string{"planning"}, StatePlanning},
		{[]string{"plan-approved"}, StatePlanApproved},
	}
	for _, tt := range tests {
		got := StateFromLabels(tt.labels)
		if got != tt.want {
			t.Errorf("StateFromLabels(%v) = %q, want %q", tt.labels, got, tt.want)
		}
	}
}

func TestIsTransitionValid(t *testing.T) {
	if !IsTransitionValid(StateReady, StateImplementing) {
		t.Error("ready -> implementing should be valid")
	}
	if !IsTransitionValid(StatePlanning, StatePlanApproved) {
		t.Error("planning -> plan-approved should be valid")
	}
	if IsTransitionValid(StateDone, StatePlanning) {
		t.Error("done -> planning should be invalid")
	}
	if !IsTransitionValid(StateFSMBlocked, StateReady) {
		t.Error("blocked -> ready should be valid")
	}
}
