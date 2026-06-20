package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

// bugReportDepConfig returns a config that enables only the A3 bug-report
// dependency pre-flight (every other enable_* flag is off, scaffold/role-gate
// disabled, yolo not relevant). Reuses interactionTestConfig and overrides.
func bugReportDepConfig(t *testing.T, forgejoURL string) *config.Config {
	cfg := interactionTestConfig(t, forgejoURL)
	cfg.Agent.RequireRoleTag = false
	cfg.Agent.EnableScaffoldDetection = false
	cfg.Agent.EnableBugReportDepBlock = true
	return cfg
}

// issueOpenedWithBody builds an IssueOpened event whose payload carries the
// given issue title and body so extractIssueTitle + the GetIssue handler
// (which mirrors the override) can rely on them.
func issueOpenedWithBody(num int, title, body string) *event.Event {
	evt := event.NewEvent(event.IssueOpened, "org/repo", num, 0, "alice", "opened")
	evt.SessionKey = "org/repo/issues/" + itoa(num)
	evt.Payload = map[string]interface{}{
		"issue": map[string]interface{}{
			"number": float64(num),
			"title":  title,
			"body":   body,
		},
	}
	return evt
}

func itoa(n int) string {
	// tiny strconv without pulling strconv into every test file
	if n == 0 {
		return "0"
	}
	var b []byte
	if n < 0 {
		return "negative"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// newBugReportManager wires up a Manager + interactionForgejo with the A3
// flag enabled. Returns the manager, the fake (for assertions), and a
// shutdown release.
func newBugReportManager(t *testing.T) (*Manager, *interactionForgejo, func()) {
	f := newInteractionForgejo(t)
	cfg := bugReportDepConfig(t, f.URL())
	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		f.Close()
		t.Fatalf("new manager: %v", err)
	}
	return mgr, f, func() {
		mgr.shutdownAll()
		f.Close()
	}
}

// waitFor runs fn until it returns true or the timeout elapses. Used to give
// the bus-dispatch path a moment when handleEvent dispatches asynchronously
// (handleEvent itself is synchronous in tests; the bus may insert a tiny
// window before the session is created if a synthetic event was auto-spawned).
func waitFor(t *testing.T, what string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestBugReportDepBlock_BlocksOnUnmergedRef covers A3 scenario 1: a bug
// report whose body references an open unmerged PR is auto-blocked.
func TestBugReportDepBlock_BlocksOnUnmergedRef(t *testing.T) {
	mgr, f, release := newBugReportManager(t)
	defer release()

	// Trigger issue #10: bug report body references PR #8.
	f.mu.Lock()
	if f.issueOverrides == nil {
		f.issueOverrides = make(map[int]issueOverride)
	}
	f.issueOverrides[10] = issueOverride{
		Title: "Bug: prime 2 says no",
		Body:  "Bug introduced in PR #8. Running `prime 2` reports 'no' but should say 'yes'.",
		State: "open",
		IsPR:  false,
	}
	f.issueOverrides[8] = issueOverride{
		Title: "Implement prime checker",
		Body:  "PR adding the prime subcommand",
		State: "open",
		IsPR:  true, // pulls the pull_request field into the GetIssue response
	}
	f.mu.Unlock()

	evt := issueOpenedWithBody(10, "Bug: prime 2 says no", "Bug introduced in PR #8.")

	mgr.handleEvent(context.Background(), evt)

	// Assertions: blocked label applied, comment posted with marker.
	f.mu.Lock()
	blocked := false
	for _, l := range f.addedLabels {
		if l == "blocked" {
			blocked = true
		}
	}
	comments := append([]string(nil), f.comments...)
	f.mu.Unlock()

	if !blocked {
		t.Error("expected 'blocked' label to be added to the bug-report issue")
	}
	var markerComment bool
	var mentionsPR bool
	for _, c := range comments {
		if strings.Contains(c, "<!-- ford -->") {
			markerComment = true
		}
		if strings.Contains(c, "PR #8") {
			mentionsPR = true
		}
	}
	if !markerComment {
		t.Error("expected an auto-block comment containing the agent marker")
	}
	if !mentionsPR {
		t.Error("expected auto-block comment to reference the blocking PR")
	}

	// Most important: no session should have been created for the bug report.
	mgr.mu.RLock()
	_, exists := mgr.sessions["org/repo/issues/10"]
	mgr.mu.RUnlock()
	if exists {
		t.Error("expected NO implementer session to be created for the auto-blocked bug report")
	}
}

// TestBugReportDepBlock_NoBlockWhenRefPRMerged covers A3 scenario 2: a bug
// report referencing an already-merged (closed-state) PR is NOT blocked.
func TestBugReportDepBlock_NoBlockWhenRefPRMerged(t *testing.T) {
	mgr, f, release := newBugReportManager(t)
	defer release()

	f.mu.Lock()
	if f.issueOverrides == nil {
		f.issueOverrides = make(map[int]issueOverride)
	}
	f.issueOverrides[15] = issueOverride{
		Title: "Bug from PR #6",
		Body:  "Bug introduced in PR #6. The export command adds an empty header line.",
		State: "open",
		IsPR:  false,
	}
	f.issueOverrides[6] = issueOverride{
		Title: "Implement export command",
		Body:  "Adds CSV export",
		State: "closed", // merged
		IsPR:  true,
	}
	// The default issue (returned when no override matches) is non-PR open;
	// to mimic the trigger being implementer we set the role via label.
	f.issueLabels = []string{"role:implementer"}
	f.mu.Unlock()

	evt := issueOpenedWithBody(15, "Bug from PR #6", "Bug introduced in PR #6.")
	mgr.handleEvent(context.Background(), evt)

	// Should NOT have blocked the issue.
	f.mu.Lock()
	blocked := false
	for _, l := range f.addedLabels {
		if l == "blocked" {
			blocked = true
		}
	}
	f.mu.Unlock()
	if blocked {
		t.Error("expected the bug report NOT to be blocked when the referenced PR is merged")
	}
}

// TestBugReportDepBlock_NoBlockWhenRefIsPMIssue covers A3 scenario 3: a
// bug report referencing another issue that is NOT a PR (a PM/coordination
// issue) is NOT blocked.
func TestBugReportDepBlock_NoBlockWhenRefIsPMIssue(t *testing.T) {
	mgr, f, release := newBugReportManager(t)
	defer release()

	f.mu.Lock()
	if f.issueOverrides == nil {
		f.issueOverrides = make(map[int]issueOverride)
	}
	f.issueOverrides[20] = issueOverride{
		Title: "Follow-up work",
		Body:  "See issue #9 for the PM decomposition of this work.",
		State: "open",
		IsPR:  false,
	}
	f.issueOverrides[9] = issueOverride{
		Title: "[pm] Decompose release",
		Body:  "Plan the release",
		State: "open",
		IsPR:  false, // a PM issue, no pull_request field
	}
	f.issueLabels = []string{"role:implementer"}
	f.mu.Unlock()

	evt := issueOpenedWithBody(20, "Follow-up work", "See issue #9 for the PM decomposition.")
	mgr.handleEvent(context.Background(), evt)

	f.mu.Lock()
	blocked := false
	for _, l := range f.addedLabels {
		if l == "blocked" {
			blocked = true
		}
	}
	f.mu.Unlock()
	if blocked {
		t.Error("expected the bug report NOT to be blocked when the referenced issue is a PM issue (no PR)")
	}
}

// TestBugReportDepBlock_SkippedForPMTitle covers A3 scenario 4: a PM issue
// whose title is [pm] is exempt from the pre-flight, even if its body
// references open PRs.
func TestBugReportDepBlock_SkippedForPMTitle(t *testing.T) {
	mgr, f, release := newBugReportManager(t)
	defer release()

	f.mu.Lock()
	if f.issueOverrides == nil {
		f.issueOverrides = make(map[int]issueOverride)
	}
	f.issueOverrides[30] = issueOverride{
		Title: "[pm] Plan the release",
		Body:  "Coordination for items in PR #8.",
		State: "open",
		IsPR:  false,
	}
	f.issueOverrides[8] = issueOverride{
		Title: "Implement prime checker",
		Body:  "PR adding the prime subcommand",
		State: "open",
		IsPR:  true,
	}
	f.mu.Unlock()

	evt := issueOpenedWithBody(30, "[pm] Plan the release", "Coordination for items in PR #8.")
	mgr.handleEvent(context.Background(), evt)

	// PM issues should not be auto-blocked by the implementer pre-flight.
	f.mu.Lock()
	blocked := false
	for _, l := range f.addedLabels {
		if l == "blocked" {
			blocked = true
		}
	}
	f.mu.Unlock()
	if blocked {
		t.Error("expected PM-titled issue to be exempt from the bug-report dependency pre-flight")
	}
}

// TestBugReportDepBlock_ConfigFlagDisabled covers A3 scenario 5: when the
// enable_bug_report_dep_block flag is off, the pre-flight is a no-op even if
// the body references an open unmerged PR.
func TestBugReportDepBlock_ConfigFlagDisabled(t *testing.T) {
	f := newInteractionForgejo(t)
	defer f.Close()

	cfg := interactionTestConfig(t, f.URL())
	cfg.Agent.RequireRoleTag = false
	cfg.Agent.EnableScaffoldDetection = false
	cfg.Agent.EnableBugReportDepBlock = false // disabled

	bus := event.NewBus()
	mgr, err := NewManager(cfg, bus)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.shutdownAll()

	f.mu.Lock()
	if f.issueOverrides == nil {
		f.issueOverrides = make(map[int]issueOverride)
	}
	f.issueOverrides[10] = issueOverride{
		Title: "Bug: prime 2 says no",
		Body:  "Bug introduced in PR #8.",
		State: "open",
		IsPR:  false,
	}
	f.issueOverrides[8] = issueOverride{
		Title: "Implement prime checker",
		Body:  "the prime subcommand",
		State: "open",
		IsPR:  true,
	}
	f.mu.Unlock()

	evt := issueOpenedWithBody(10, "Bug: prime 2 says no", "Bug introduced in PR #8.")
	mgr.handleEvent(context.Background(), evt)

	f.mu.Lock()
	blocked := false
	for _, l := range f.addedLabels {
		if l == "blocked" {
			blocked = true
		}
	}
	f.mu.Unlock()
	if blocked {
		t.Error("expected no auto-block when enable_bug_report_dep_block is false")
	}
}

// TestExtractBugReportRefs is a focused unit test for the parser used by A3.
func TestExtractBugReportRefs(t *testing.T) {
	cases := []struct {
		name  string
		title string
		body  string
		want  []int
	}{
		{"standalone hash", "Bug", "introduced in #8", []int{8}},
		{"PR-prefix", "Bug", "PR #8 misbehaves", []int{8}},
		{"issue-prefix", "Follow-up", "see issue #9 for details", []int{9}},
		{"multiple refs deduped", "Multi-ref", "PR #4 and #4 again, plus #6", []int{4, 6}},
		{"no refs", "Nothing", "There is no reference here", nil},
		{"depends-on-syntax", "Bug", "Depends on: #7", []int{7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBugReportRefs(tc.title, tc.body)
			if !equalInts(got, tc.want) {
				t.Errorf("extractBugReportRefs(%q,%q)=%v, want %v", tc.title, tc.body, got, tc.want)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	// both slices are produced by a deterministic parser (map iteration order
	// matters here); compare as sets when at least one entry exists but the
	// parser returns unique refs in match order — so order is stable.
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
