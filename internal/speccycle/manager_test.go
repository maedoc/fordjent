package speccycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/forgejo"
)

// ── Helpers ──────────────────────────────────────────────────────────────

func writeTasks(path, content string, t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(path, content string, t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ── SpecManager ──────────────────────────────────────────────────────────

func TestCreateChange(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	err := sm.CreateChange("test-feature")
	if err != nil {
		t.Fatalf("CreateChange failed: %v", err)
	}

	changeDir := filepath.Join(repoDir, "openspec", "changes", "test-feature")
	if !dirExists(changeDir) {
		t.Errorf("expected change dir %s to exist", changeDir)
	}

	specsDir := filepath.Join(changeDir, "specs")
	if !dirExists(specsDir) {
		t.Errorf("expected specs dir %s to exist", specsDir)
	}
}

func TestCreateChange_AlreadyExists(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	// First creation succeeds
	if err := sm.CreateChange("test-feature"); err != nil {
		t.Fatalf("first CreateChange failed: %v", err)
	}

	// Second creation should error
	err := sm.CreateChange("test-feature")
	if err == nil {
		t.Fatal("expected error for duplicate change, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}

func TestCreateChange_EmptyName(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	err := sm.CreateChange("")
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
}

func TestListChanges(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	// Create multiple changes
	if err := sm.CreateChange("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := sm.CreateChange("beta"); err != nil {
		t.Fatal(err)
	}
	if err := sm.CreateChange("gamma"); err != nil {
		t.Fatal(err)
	}

	// Create an archive dir (should be excluded)
	archiveDir := filepath.Join(repoDir, "openspec", "changes", "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatal(err)
	}

	changes, err := sm.ListChanges()
	if err != nil {
		t.Fatalf("ListChanges failed: %v", err)
	}

	if len(changes) != 3 {
		t.Fatalf("expected 3 changes, got %d", len(changes))
	}

	// Verify names (sorted alphabetically)
	expected := []string{"alpha", "beta", "gamma"}
	for i, c := range changes {
		if c.Name != expected[i] {
			t.Errorf("change[%d].Name = %q, want %q", i, c.Name, expected[i])
		}
		if c.LastModified.IsZero() {
			t.Errorf("change[%d].LastModified is zero", i)
		}
	}
}

func TestListChanges_ArchiveExcluded(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	// Create a change
	if err := sm.CreateChange("active-change"); err != nil {
		t.Fatal(err)
	}

	// Create archive directory manually
	archivePath := filepath.Join(repoDir, "openspec", "changes", "archive", "2026-01-01-old-change")
	if err := os.MkdirAll(archivePath, 0755); err != nil {
		t.Fatal(err)
	}

	changes, err := sm.ListChanges()
	if err != nil {
		t.Fatalf("ListChanges failed: %v", err)
	}

	for _, c := range changes {
		if c.Name == "archive" {
			t.Errorf("archive dir should not appear in changes, got %q", c.Name)
		}
	}

	if len(changes) != 1 {
		t.Fatalf("expected 1 change (archive excluded), got %d: %+v", len(changes), changes)
	}
}

func TestListChanges_EmptyDir(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	changes, err := sm.ListChanges()
	if err != nil {
		t.Fatalf("ListChanges failed: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected empty list, got %d items", len(changes))
	}
}

func TestListChanges_NoChangesDir(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	// Don't create any openspec directory at all
	changes, err := sm.ListChanges()
	if err != nil {
		t.Fatalf("ListChanges on missing dir should not error: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected empty list, got %d", len(changes))
	}
}

// ── ParseTasks ───────────────────────────────────────────────────────────

func TestParseTasks(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	tasksContent := `## 1. Setup

- [ ] Create auth module
- [x] Implement OAuth flow [parallel]
- Some prose text (should be skipped)
- [ ] Write integration tests

More prose.
- [x] Done task
`
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "tasks.md")
	writeFile(tasksPath, tasksContent, t)

	tasks, err := sm.ParseTasks("my-feature")
	if err != nil {
		t.Fatalf("ParseTasks failed: %v", err)
	}

	if len(tasks) != 4 {
		t.Fatalf("expected 4 tasks, got %d", len(tasks))
	}

	// Task 1: not done, not parallel
	if tasks[0].Index != 1 || tasks[0].Description != "Create auth module" || tasks[0].Done || tasks[0].Parallel {
		t.Errorf("task[0] mismatch: %+v", tasks[0])
	}

	// Task 2: done, parallel
	if tasks[1].Index != 2 || tasks[1].Description != "Implement OAuth flow" || !tasks[1].Done || !tasks[1].Parallel {
		t.Errorf("task[1] mismatch: %+v", tasks[1])
	}

	// Task 3: not done, not parallel
	if tasks[2].Index != 3 || tasks[2].Description != "Write integration tests" || tasks[2].Done || tasks[2].Parallel {
		t.Errorf("task[2] mismatch: %+v", tasks[2])
	}

	// Task 4: done, not parallel
	if tasks[3].Index != 4 || tasks[3].Description != "Done task" || !tasks[3].Done || tasks[3].Parallel {
		t.Errorf("task[3] mismatch: %+v", tasks[3])
	}
}

func TestParseTasks_EmptyFile(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("empty"); err != nil {
		t.Fatal(err)
	}

	// Empty file
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "empty", "tasks.md")
	writeFile(tasksPath, "", t)

	tasks, err := sm.ParseTasks("empty")
	if err != nil {
		t.Fatalf("ParseTasks failed: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for empty file, got %d", len(tasks))
	}
}

func TestParseTasks_NoFile(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("empty"); err != nil {
		t.Fatal(err)
	}

	// Don't create tasks.md at all
	tasks, err := sm.ParseTasks("empty")
	if err != nil {
		t.Fatalf("ParseTasks with missing file should not error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for missing file, got %d", len(tasks))
	}
}

func TestParseTasks_OnlyProse(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("prose-only"); err != nil {
		t.Fatal(err)
	}

	tasksContent := `## Just prose

This is a description of what needs to be done.

There are no checkbox items here.

Just text.
`
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "prose-only", "tasks.md")
	writeFile(tasksPath, tasksContent, t)

	tasks, err := sm.ParseTasks("prose-only")
	if err != nil {
		t.Fatalf("ParseTasks failed: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for prose-only file, got %d", len(tasks))
	}
}

// ── MarkTaskComplete ─────────────────────────────────────────────────────

func TestMarkTaskComplete(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	tasksContent := `## 1. Work

- [ ] First task
- [ ] Second task
- [ ] Third task
`
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "tasks.md")
	writeFile(tasksPath, tasksContent, t)

	// Mark task 2 complete
	if err := sm.MarkTaskComplete("my-feature", 2); err != nil {
		t.Fatalf("MarkTaskComplete failed: %v", err)
	}

	// Read back and verify
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	expected := `## 1. Work

- [ ] First task
- [x] Second task
- [ ] Third task
`
	if content != expected {
		t.Errorf("mark task 2:\nexpected:\n%s\ngot:\n%s", expected, content)
	}
}

func TestMarkTaskComplete_Idempotent(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	tasksContent := `- [x] Already done
- [ ] Not done
`
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "tasks.md")
	writeFile(tasksPath, tasksContent, t)

	// Mark already-complete task
	if err := sm.MarkTaskComplete("my-feature", 1); err != nil {
		t.Fatalf("MarkTaskComplete failed: %v", err)
	}

	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	expected := `- [x] Already done
- [ ] Not done
`
	if content != expected {
		t.Errorf("idempotent mark not preserved:\nexpected:\n%s\ngot:\n%s", expected, content)
	}
}

func TestMarkTaskComplete_OutOfRange(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	tasksContent := `- [ ] Task 1
- [ ] Task 2
- [ ] Task 3
`
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "tasks.md")
	writeFile(tasksPath, tasksContent, t)

	err := sm.MarkTaskComplete("my-feature", 99)
	if err == nil {
		t.Fatal("expected error for out-of-range index, got nil")
	}
	if !strings.Contains(err.Error(), "out of range") && !strings.Contains(err.Error(), "only") {
		t.Errorf("expected 'out of range' or 'only' error, got: %v", err)
	}
}

func TestMarkTaskComplete_DollarSignInDescription(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("dollar-test"); err != nil {
		t.Fatal(err)
	}

	tasksContent := "- [ ] Fix $2 pricing bug\n"
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "dollar-test", "tasks.md")
	writeFile(tasksPath, tasksContent, t)

	if err := sm.MarkTaskComplete("dollar-test", 1); err != nil {
		t.Fatalf("MarkTaskComplete failed: %v", err)
	}

	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	expected := "- [x] Fix $2 pricing bug\n"
	if content != expected {
		t.Errorf("dollar sign not preserved:\nexpected: %q\ngot:      %q", expected, content)
	}
}

func TestMarkTaskComplete_IndexZero(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	tasksContent := `- [ ] Task 1
`
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "tasks.md")
	writeFile(tasksPath, tasksContent, t)

	err := sm.MarkTaskComplete("my-feature", 0)
	if err == nil {
		t.Fatal("expected error for index 0, got nil")
	}
}

func TestMarkTaskComplete_MultipleMarks(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	tasksContent := `- [ ] Task 1
- [ ] Task 2
- [ ] Task 3
`
	tasksPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "tasks.md")
	writeFile(tasksPath, tasksContent, t)

	// Mark 1, then 3, then 2 (out of order)
	if err := sm.MarkTaskComplete("my-feature", 1); err != nil {
		t.Fatal(err)
	}
	if err := sm.MarkTaskComplete("my-feature", 3); err != nil {
		t.Fatal(err)
	}
	if err := sm.MarkTaskComplete("my-feature", 2); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	expected := `- [x] Task 1
- [x] Task 2
- [x] Task 3
`
	if content != expected {
		t.Errorf("multiple marks:\nexpected:\n%s\ngot:\n%s", expected, content)
	}
}

// ── ArchiveChange ────────────────────────────────────────────────────────

func TestArchiveChange(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	// Create a delta spec file
	specContent := "## ADDED Requirements\n\n### Requirement: Test\nSome content\n"
	specPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "specs", "auth-core", "spec.md")
	writeFile(specPath, specContent, t)

	// Archive the change
	if err := sm.ArchiveChange("my-feature"); err != nil {
		t.Fatalf("ArchiveChange failed: %v", err)
	}

	// Verify the source change directory no longer exists
	srcDir := filepath.Join(repoDir, "openspec", "changes", "my-feature")
	if dirExists(srcDir) {
		t.Errorf("source change directory should be gone after archive: %s", srcDir)
	}

	// Verify the archive directory exists
	today := time.Now().UTC().Format("2006-01-02")
	archivePath := filepath.Join(repoDir, "openspec", "changes", "archive", today+"-my-feature")
	if !dirExists(archivePath) {
		t.Errorf("archive dir should exist: %s", archivePath)
	}

	// Verify the spec was synced to openspec/specs/auth-core/spec.md
	syncedSpec := filepath.Join(repoDir, "openspec", "specs", "auth-core", "spec.md")
	if !fileExists(syncedSpec) {
		t.Errorf("synced spec should exist: %s", syncedSpec)
	}
	data, err := os.ReadFile(syncedSpec)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != specContent {
		t.Errorf("synced spec content mismatch:\ngot:\n%s\nwant:\n%s", string(data), specContent)
	}
}

func TestArchiveChange_TargetExists(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	// Create the archive target preemptively (simulate collision)
	today := time.Now().UTC().Format("2006-01-02")
	archiveTarget := filepath.Join(repoDir, "openspec", "changes", "archive", today+"-my-feature")
	if err := os.MkdirAll(archiveTarget, 0755); err != nil {
		t.Fatal(err)
	}

	err := sm.ArchiveChange("my-feature")
	if err == nil {
		t.Fatal("expected error when archive target exists, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}

	// Source should still be intact
	srcDir := filepath.Join(repoDir, "openspec", "changes", "my-feature")
	if !dirExists(srcDir) {
		t.Errorf("source change should still exist after failed archive: %s", srcDir)
	}
}

func TestArchiveChange_NotFound(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	err := sm.ArchiveChange("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent change, got nil")
	}
}

func TestArchiveChange_NoSpecs(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("no-specs"); err != nil {
		t.Fatal(err)
	}

	// Create a non-spec file (should not cause issues)
	readme := filepath.Join(repoDir, "openspec", "changes", "no-specs", "README.md")
	writeFile(readme, "Some readme", t)

	if err := sm.ArchiveChange("no-specs"); err != nil {
		t.Fatalf("ArchiveChange failed: %v", err)
	}

	// Verify no spec was synced
	specDir := filepath.Join(repoDir, "openspec", "specs")
	if dirExists(specDir) {
		entries, _ := os.ReadDir(specDir)
		if len(entries) > 0 {
			t.Errorf("expected no specs to be synced, found %d entries", len(entries))
		}
	}
}

// ── ChangeExists ─────────────────────────────────────────────────────────

func TestChangeExists(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if ChangeExists(repoDir, "my-feature") {
		t.Error("expected false before creation")
	}

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	if !ChangeExists(repoDir, "my-feature") {
		t.Error("expected true after creation")
	}

	if ChangeExists(repoDir, "nonexistent") {
		t.Error("expected false for nonexistent")
	}
}

// ── ReadChangeFile ───────────────────────────────────────────────────────

func TestReadChangeFile(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	content := "# Proposal\n\nImplement the feature\n"
	proposalPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "proposal.md")
	writeFile(proposalPath, content, t)

	got, err := ReadChangeFile(repoDir, "my-feature", "proposal.md")
	if err != nil {
		t.Fatalf("ReadChangeFile failed: %v", err)
	}
	if got != content {
		t.Errorf("ReadChangeFile:\ngot:\n%s\nwant:\n%s", got, content)
	}
}

func TestReadChangeFile_ChangeNotExist(t *testing.T) {
	// Simulates push-session scenario where openspec/changes/ dir doesn't exist
	repoDir := t.TempDir()

	content, err := ReadChangeFile(repoDir, "nonexistent-change", "proposal.md")
	if err != nil {
		t.Fatalf("expected helpful message for missing change, got error: %v", err)
	}
	if !strings.Contains(content, "not found") {
		t.Fatalf("expected 'not found' hint, got: %s", content)
	}
}

func TestReadChangeFile_EscapesDir(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	_, err := ReadChangeFile(repoDir, "my-feature", "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("expected 'escapes' error, got: %v", err)
	}
}

func TestReadChangeFile_NotFound(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	content, err := ReadChangeFile(repoDir, "my-feature", "nonexistent.md")
	if err != nil {
		t.Fatalf("expected helpful message for nonexistent file, got error: %v", err)
	}
	if !strings.Contains(content, "not found") {
		t.Fatalf("expected 'not found' hint in response, got: %s", content)
	}
}

// ── ReadSpecFile ─────────────────────────────────────────────────────────

func TestReadSpecFile_Merged(t *testing.T) {
	repoDir := t.TempDir()

	content := "# Auth Spec\n\nAuthentication requirements\n"
	specPath := filepath.Join(repoDir, "openspec", "specs", "auth-core", "spec.md")
	writeFile(specPath, content, t)

	got, err := ReadSpecFile(repoDir, "auth-core")
	if err != nil {
		t.Fatalf("ReadSpecFile failed: %v", err)
	}
	if got != content {
		t.Errorf("ReadSpecFile:\ngot:\n%s\nwant:\n%s", got, content)
	}
}

func TestReadSpecFile_ChangeFallback(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	// Write spec ONLY in active change (not merged)
	content := "# OAuth Spec\n\nOAuth requirements\n"
	specPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "specs", "oauth-core", "spec.md")
	writeFile(specPath, content, t)

	// ReadSpecFile should find it via fallback
	got, err := ReadSpecFile(repoDir, "oauth-core")
	if err != nil {
		t.Fatalf("ReadSpecFile fallback failed: %v", err)
	}
	if got != content {
		t.Errorf("ReadSpecFile fallback:\ngot:\n%s\nwant:\n%s", got, content)
	}
}

func TestReadSpecFile_MergedTakesPriority(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	// Write merged spec
	mergedContent := "# Merged Spec\n"
	mergedPath := filepath.Join(repoDir, "openspec", "specs", "auth-core", "spec.md")
	writeFile(mergedPath, mergedContent, t)

	// Write change spec (should NOT be returned)
	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}
	changeContent := "# Change Spec\n"
	changePath := filepath.Join(repoDir, "openspec", "changes", "my-feature", "specs", "auth-core", "spec.md")
	writeFile(changePath, changeContent, t)

	got, err := ReadSpecFile(repoDir, "auth-core")
	if err != nil {
		t.Fatalf("ReadSpecFile failed: %v", err)
	}
	if got != mergedContent {
		t.Errorf("expected merged spec to take priority, got: %s", got)
	}
}

func TestReadSpecFile_NotFound(t *testing.T) {
	repoDir := t.TempDir()

	_, err := ReadSpecFile(repoDir, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent spec, got nil")
	}
}

// ── isSpecFilePath / extractChangeName ────────────────────────────────────

func TestIsSpecFilePath(t *testing.T) {
	tests := []struct {
		path  string
		want  bool
		label string
	}{
		{"openspec/changes/user-auth/proposal.md", true, "proposal in active change"},
		{"openspec/changes/user-auth/specs/auth/spec.md", true, "spec in active change"},
		{"openspec/changes/user-auth/design.md", true, "design in active change"},
		{"openspec/changes/user-auth/tasks.md", true, "tasks in active change"},
		{"openspec/changes/archive/2026-01-01-old/spec.md", false, "archived change"},
		{"openspec/changes/archive/old/spec.md", false, "archived change (no date)"},
		{"cmd/main.go", false, "regular code file"},
		{"openspec/specs/auth/spec.md", false, "merged spec (not in changes/)"},
		{"openspec/changes/", false, "just the changes dir"},
		{"openspec/changes/user-auth/", true, "change directory itself"},
		{"openspec/changes/archive/", false, "archive dir itself"},
	}

	for _, tt := range tests {
		got := isSpecFilePath(tt.path)
		if got != tt.want {
			t.Errorf("isSpecFilePath(%q) = %v, want %v (%s)", tt.path, got, tt.want, tt.label)
		}
	}
}

func TestExtractChangeName(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"openspec/changes/user-auth/proposal.md", "user-auth"},
		{"openspec/changes/user-auth/specs/auth/spec.md", "user-auth"},
		{"openspec/changes/user-auth/design.md", "user-auth"},
		{"openspec/changes/bugfix-typo/tasks.md", "bugfix-typo"},
		{"openspec/changes/archive/old/spec.md", ""},
		{"cmd/main.go", ""},
		{"openspec/specs/auth/spec.md", ""},
	}

	for _, tt := range tests {
		got := extractChangeName(tt.path)
		if got != tt.want {
			t.Errorf("extractChangeName(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// ── SpecPRManager ────────────────────────────────────────────────────────

// mockPRFilesClient implements PRFilesLister for testing.
type mockPRFilesClient struct {
	files []forgejo.PRFile
	err   error
}

func (m *mockPRFilesClient) GetPRFiles(_ context.Context, _ string, _ int) ([]forgejo.PRFile, error) {
	return m.files, m.err
}

func TestSpecPRManager_IsSpecPR(t *testing.T) {
	tests := []struct {
		label       string
		files       []forgejo.PRFile
		wantIsSpec  bool
		wantName    string
	}{
		{
			label: "spec proposal",
			files: []forgejo.PRFile{
				{Filename: "cmd/main.go", Status: "modified"},
				{Filename: "openspec/changes/user-auth/proposal.md", Status: "added"},
			},
			wantIsSpec: true,
			wantName:   "user-auth",
		},
		{
			label: "multiple spec files same change",
			files: []forgejo.PRFile{
				{Filename: "openspec/changes/user-auth/proposal.md", Status: "added"},
				{Filename: "openspec/changes/user-auth/design.md", Status: "added"},
				{Filename: "openspec/changes/user-auth/tasks.md", Status: "added"},
			},
			wantIsSpec: true,
			wantName:   "user-auth",
		},
		{
			label: "no spec files",
			files: []forgejo.PRFile{
				{Filename: "cmd/main.go", Status: "modified"},
			},
			wantIsSpec: false,
			wantName:   "",
		},
		{
			label: "empty PR",
			files: []forgejo.PRFile{},
			wantIsSpec: false,
			wantName:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			mock := &mockPRFilesClient{files: tt.files}
			spm := NewSpecPRManager(mock)

			info, err := spm.IsSpecPR(context.Background(), "test/repo", 1)
			if err != nil {
				t.Fatalf("IsSpecPR failed: %v", err)
			}
			if info.IsSpecPR != tt.wantIsSpec {
				t.Errorf("IsSpecPR = %v, want %v", info.IsSpecPR, tt.wantIsSpec)
			}
			if tt.wantIsSpec && info.ChangeName != tt.wantName {
				t.Errorf("ChangeName = %q, want %q", info.ChangeName, tt.wantName)
			}
		})
	}
}

func TestSpecPRManager_Error(t *testing.T) {
	mock := &mockPRFilesClient{
		files: nil,
		err:   os.ErrPermission,
	}
	spm := NewSpecPRManager(mock)

	info, err := spm.IsSpecPR(context.Background(), "test/repo", 1)
	if err == nil {
		t.Fatal("expected error from mock, got nil")
	}
	if info.IsSpecPR {
		t.Error("expected IsSpecPR=false on error")
	}
}

// ── ListChanges Schema ────────────────────────────────────────────────────

func TestListChanges_WithSchema(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	// Create .openspec.yaml with schema
	openSpecYAML := "schema: spec-driven\n"
	yamlPath := filepath.Join(repoDir, "openspec", "changes", "my-feature", ".openspec.yaml")
	writeFile(yamlPath, openSpecYAML, t)

	changes, err := sm.ListChanges()
	if err != nil {
		t.Fatalf("ListChanges failed: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Schema != "spec-driven" {
		t.Errorf("expected Schema 'spec-driven', got %q", changes[0].Schema)
	}
}

func TestListChanges_WithoutSchema(t *testing.T) {
	repoDir := t.TempDir()
	sm := NewSpecManager(repoDir)

	if err := sm.CreateChange("my-feature"); err != nil {
		t.Fatal(err)
	}

	// No .openspec.yaml created

	changes, err := sm.ListChanges()
	if err != nil {
		t.Fatalf("ListChanges failed: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Schema != "" {
		t.Errorf("expected empty Schema, got %q", changes[0].Schema)
	}
}

