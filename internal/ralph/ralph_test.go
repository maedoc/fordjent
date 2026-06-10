package ralph

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/config"
)

// --- Tracker tests ---

func TestTrackerCorrectStageSequence(t *testing.T) {
	tr := NewTracker(20)

	if err := tr.RecordStage(StageAwareness, "read git log"); err != nil {
		t.Fatalf("awareness should succeed: %v", err)
	}
	if err := tr.RecordStage(StageAct, "added nil guard"); err != nil {
		t.Fatalf("act should succeed: %v", err)
	}
	if err := tr.RecordStage(StageAssert, "tests pass"); err != nil {
		t.Fatalf("assert should succeed: %v", err)
	}
	if err := tr.RecordStage(StageAppend, "committed"); err != nil {
		t.Fatalf("append should succeed: %v", err)
	}

	if !tr.IsComplete() {
		t.Fatal("tracker should report complete after all 4 stages")
	}
}

func TestTrackerOutOfOrderStage(t *testing.T) {
	tr := NewTracker(20)

	err := tr.RecordStage(StageAppend, "try to commit early")
	if err == nil {
		t.Fatal("append before awareness/act/assert should fail")
	}
	if err := tr.RecordStage(StageAwareness, "ok"); err != nil {
		t.Fatalf("awareness should succeed: %v", err)
	}

	// act should fail because assert hasn't been called... no, act follows awareness
	if err := tr.RecordStage(StageAct, "ok"); err != nil {
		t.Fatalf("act after awareness should succeed: %v", err)
	}

	// append should fail because assert wasn't called
	err = tr.RecordStage(StageAppend, "try again")
	if err == nil {
		t.Fatal("append before assert should fail")
	}
}

func TestTrackerDuplicateStageIdempotent(t *testing.T) {
	tr := NewTracker(20)

	if err := tr.RecordStage(StageAwareness, "first"); err != nil {
		t.Fatalf("first awareness: %v", err)
	}
	if err := tr.RecordStage(StageAwareness, "second"); err != nil {
		t.Fatalf("duplicate awareness should be idempotent: %v", err)
	}

	msgs := tr.StageMessages()
	if msgs[StageAwareness] != "second" {
		t.Fatalf("expected message to be overwritten to 'second', got %q", msgs[StageAwareness])
	}
}

func TestTrackerInvalidStage(t *testing.T) {
	tr := NewTracker(20)
	err := tr.RecordStage("invalid", "bad")
	if err == nil {
		t.Fatal("invalid stage should be rejected")
	}
}

func TestTrackerIsCompletePartial(t *testing.T) {
	tr := NewTracker(20)
	tr.RecordStage(StageAwareness, "ok")
	tr.RecordStage(StageAct, "ok")
	if tr.IsComplete() {
		t.Fatal("should not be complete with only 2 stages")
	}
}

func TestTrackerReset(t *testing.T) {
	tr := NewTracker(20)
	tr.RecordStage(StageAwareness, "ok")
	tr.RecordStage(StageAct, "ok")
	tr.Reset()

	completed := tr.CompletedStages()
	if len(completed) > 0 {
		t.Fatalf("after reset, should have no completed stages, got %d", len(completed))
	}
	if tr.IsComplete() {
		t.Fatal("after reset, should not be complete")
	}
}

// --- Nudge tests ---

func TestNudge25Percent(t *testing.T) {
	tr := NewTracker(20)
	msg, ok := tr.ShouldNudge(5) // 5/20 = 25%
	if !ok {
		t.Fatal("should nudge at 25% when awareness not done")
	}
	if msg == "" {
		t.Fatal("nudge message should not be empty")
	}
}

func TestNudge50Percent(t *testing.T) {
	tr := NewTracker(20)
	tr.RecordStage(StageAwareness, "ok")
	msg, ok := tr.ShouldNudge(10) // 10/20 = 50%
	if !ok {
		t.Fatal("should nudge at 50% when act not done")
	}
	if msg == "" {
		t.Fatal("nudge message should not be empty")
	}
}

func TestNudge75Percent(t *testing.T) {
	tr := NewTracker(20)
	tr.RecordStage(StageAwareness, "ok")
	tr.RecordStage(StageAct, "ok")
	msg, ok := tr.ShouldNudge(15) // 15/20 = 75%
	if !ok {
		t.Fatal("should nudge at 75% when assert not done")
	}
	if msg == "" {
		t.Fatal("nudge message should not be empty")
	}
}

func TestNudgeUrgentFinalTurns(t *testing.T) {
	tr := NewTracker(20)
	tr.RecordStage(StageAwareness, "ok")
	tr.RecordStage(StageAct, "ok")
	tr.RecordStage(StageAssert, "ok")
	msg, ok := tr.ShouldNudge(18) // 2 turns left, append not done
	if !ok {
		t.Fatal("should nudge urgently at final turns")
	}
	if msg == "" {
		t.Fatal("urgent nudge message should not be empty")
	}
}

func TestNoNudgeWhenOnTrack(t *testing.T) {
	tr := NewTracker(20)
	tr.RecordStage(StageAwareness, "ok")
	tr.RecordStage(StageAct, "ok")
	_, ok := tr.ShouldNudge(12) // 60%, both awareness and act done
	if ok {
		t.Fatal("should not nudge when protocol is on track")
	}
}

// --- Guard tests ---

func TestGuardIsSpecPath(t *testing.T) {
	g := NewGuard("/tmp/repo")

	tests := []struct {
		path     string
		expected bool
	}{
		{"openspec/changes/my-feature/spec.md", true},
		{"openspec/specs/auth-core/spec.md", true},
		{"openspec/changes/my-feature/notes.md", false},
		{"pkg/math/math.go", false},
		{"main.go", false},
		{".ralph/progress/pr-42-iteration-3.md", false},
	}

	for _, tt := range tests {
		result := g.IsSpecPath(tt.path)
		if result != tt.expected {
			t.Errorf("IsSpecPath(%q) = %v, want %v", tt.path, result, tt.expected)
		}
	}
}

func TestGuardValidateCommitDiff(t *testing.T) {
	g := NewGuard("/tmp/repo")

	// No spec files
	err := g.ValidateCommitDiff("main.go\npkg/math/math.go")
	if err != nil {
		t.Fatalf("should not error on non-spec diff: %v", err)
	}

	// With spec files
	err = g.ValidateCommitDiff("main.go\nopenspec/changes/feature/spec.md")
	if err == nil {
		t.Fatal("should error when diff contains spec file")
	}

	// Empty diff
	err = g.ValidateCommitDiff("")
	if err != nil {
		t.Fatalf("should not error on empty diff: %v", err)
	}
}

func TestGuardIsProgressPath(t *testing.T) {
	g := NewGuard("/tmp/repo")

	if !g.IsProgressPath(".ralph/progress/pr-42-iteration-3.md") {
		t.Error("should recognize progress path")
	}
	if g.IsProgressPath(".ralph/progress/") {
		t.Error("directory alone is not a progress file")
	}
	if g.IsProgressPath("main.go") {
		t.Error("regular file should not match progress path")
	}
}

// --- Progress tests ---

func TestProgressWriteAndRead(t *testing.T) {
	dir := t.TempDir()

	stageMsgs := map[string]string{
		StageAwareness: "read git log, found nil guard missing",
		StageAct:      "added nil guard to main.go",
		StageAssert:   "tests pass",
		StageAppend:   "committed with nil guard",
	}

	path, err := WriteProgress(dir, 42, 3, stageMsgs)
	if err != nil {
		t.Fatalf("WriteProgress: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty file path")
	}

	// Verify file exists
	expectedFile := filepath.Join(dir, ".ralph", "progress", "pr-42-iteration-3.md")
	if path != expectedFile {
		t.Errorf("expected path %q, got %q", expectedFile, path)
	}

	// Read it back
	p, err := ReadProgress(dir, 42, 3)
	if err != nil {
		t.Fatalf("ReadProgress: %v", err)
	}
	if p.PRNumber != 42 {
		t.Errorf("expected PRNumber=42, got %d", p.PRNumber)
	}
	if p.Iteration != 3 {
		t.Errorf("expected Iteration=3, got %d", p.Iteration)
	}

	// Verify file content contains stage info
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	content := string(data)
	if !contains(content, "Awareness") {
		t.Error("progress file should contain Awareness section")
	}
	if !contains(content, "nil guard missing") {
		t.Error("progress file should contain awareness message")
	}
}

func TestProgressIdempotentOverwrite(t *testing.T) {
	dir := t.TempDir()

	stageMsgs := map[string]string{
		StageAwareness: "first version",
	}
	_, err := WriteProgress(dir, 42, 1, stageMsgs)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Overwrite
	stageMsgs[StageAwareness] = "second version"
	_, err = WriteProgress(dir, 42, 1, stageMsgs)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}

	p, err := ReadProgress(dir, 42, 1)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = p // basic read succeeded
}

func TestProgressList(t *testing.T) {
	dir := t.TempDir()

	// Write 3 iterations
	for i := 1; i <= 3; i++ {
		stageMsgs := map[string]string{
			StageAwareness: "awareness",
		}
		_, err := WriteProgress(dir, 42, i, stageMsgs)
		if err != nil {
			t.Fatalf("write iteration %d: %v", i, err)
		}
	}

	progress, err := ListProgress(dir, 42)
	if err != nil {
		t.Fatalf("ListProgress: %v", err)
	}
	if len(progress) != 3 {
		t.Errorf("expected 3 progress entries, got %d", len(progress))
	}
}

func TestProgressListEmpty(t *testing.T) {
	dir := t.TempDir()

	progress, err := ListProgress(dir, 999)
	if err != nil {
		t.Fatalf("ListProgress on empty dir: %v", err)
	}
	if len(progress) != 0 {
		t.Errorf("expected 0 progress entries, got %d", len(progress))
	}
}

// defaultRalphConfig returns a test config with sensible defaults.
func defaultRalphConfig() config.RalphConfig {
	return config.RalphConfig{
		Enabled:                   true,
		MaxIterationsPerPR:        20,
		TurnBudgetPerIteration:    20,
		CooldownBetweenIterations: 2 * time.Minute,
		MaxCostPerPRUSD:           5.00,
		NudgeThresholdPct:         0.25,
		SummaryModel:              "",
		AutoRalphOnYolo:            true,
	}
}

func TestSchedulerShouldSpawnFirstIteration(t *testing.T) {
	cfg := defaultRalphConfig()
	s := NewScheduler(nil, cfg)

	ok, reason := s.ShouldSpawn("repo/pulls/42", "", time.Time{}, 0, 0)
	if !ok {
		t.Fatalf("first iteration should spawn, got: %s", reason)
	}
}

func TestSchedulerShouldSpawnCooldownNotElapsed(t *testing.T) {
	cfg := defaultRalphConfig()
	cfg.CooldownBetweenIterations = 5 * time.Minute
	s := NewScheduler(nil, cfg)

	lastTime := time.Now().Add(-1 * time.Minute) // 1 minute ago
	ok, reason := s.ShouldSpawn("repo/pulls/42", "completed", lastTime, 1, 0)
	if ok {
		t.Fatal("should not spawn before cooldown")
	}
	if reason == "" {
		t.Fatal("should have a reason")
	}
}

func TestSchedulerShouldSpawnCooldownElapsed(t *testing.T) {
	cfg := defaultRalphConfig()
	cfg.CooldownBetweenIterations = 1 * time.Minute
	s := NewScheduler(nil, cfg)

	lastTime := time.Now().Add(-2 * time.Minute) // 2 minutes ago
	ok, reason := s.ShouldSpawn("repo/pulls/42", "completed", lastTime, 1, 0)
	if !ok {
		t.Fatalf("should spawn after cooldown, got: %s", reason)
	}
}

func TestSchedulerShouldSpawnIterationCapExceeded(t *testing.T) {
	cfg := defaultRalphConfig()
	cfg.MaxIterationsPerPR = 3
	s := NewScheduler(nil, cfg)

	ok, reason := s.ShouldSpawn("repo/pulls/42", "completed", time.Time{}, 3, 0)
	if ok {
		t.Fatal("should not spawn when iteration cap exceeded")
	}
	if reason == "" {
		t.Fatal("should have a reason")
	}
}

func TestSchedulerShouldSpawnBudgetExceeded(t *testing.T) {
	cfg := defaultRalphConfig()
	cfg.MaxCostPerPRUSD = 5.00
	s := NewScheduler(nil, cfg)

	ok, _ := s.ShouldSpawn("repo/pulls/42", "completed", time.Time{}, 2, 6.00)
	if ok {
		t.Fatal("should not spawn when budget exceeded")
	}
}

func TestSchedulerMarkActive(t *testing.T) {
	cfg := defaultRalphConfig()
	s := NewScheduler(nil, cfg)

	if s.IsActive("repo/pulls/42") {
		t.Fatal("should not be active initially")
	}

	s.MarkActive("repo/pulls/42")
	if !s.IsActive("repo/pulls/42") {
		t.Fatal("should be active after marking")
	}

	// Should not spawn when active
	ok, _ := s.ShouldSpawn("repo/pulls/42", "", time.Time{}, 0, 0)
	if ok {
		t.Fatal("should not spawn when iteration is active")
	}

	s.MarkInactive("repo/pulls/42")
	if s.IsActive("repo/pulls/42") {
		t.Fatal("should not be active after marking inactive")
	}
}

func TestSchedulerStartStop(t *testing.T) {
	cfg := defaultRalphConfig()
	cfg.CooldownBetweenIterations = 10 * time.Second
	s := NewScheduler(nil, cfg)

	s.Start()
	// Let it tick once
	time.Sleep(100 * time.Millisecond)
	s.Stop()
}

// helper
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
