package stalegate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var gitPath string

func init() {
	var err error
	gitPath, err = exec.LookPath("git")
	if err != nil {
		gitPath = ""
	}
}

func skipIfNoGit(t *testing.T) {
	if gitPath == "" {
		t.Skip("git not found in PATH")
	}
}

func TestIsStale_UpToDate(t *testing.T) {
	skipIfNoGit(t)
	dir := setupRepo(t)
	exec.Command(gitPath, "-C", dir, "checkout", "-b", "feature").Run()

	stale, msg, err := IsStale(dir, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stale {
		t.Fatalf("expected not stale, got stale with msg: %s", msg)
	}
	if msg != "" {
		t.Fatalf("expected empty msg, got: %s", msg)
	}
}

func TestIsStale_AutoRebaseSucceeds(t *testing.T) {
	skipIfNoGit(t)
	dir := setupRepo(t)

	// Create feature branch
	exec.Command(gitPath, "-C", dir, "checkout", "-b", "feature").Run()
	// Add a commit on main AFTER feature branched
	exec.Command(gitPath, "-C", dir, "checkout", "main").Run()
	writeFile(t, dir, "new.go", "package main\n")
	exec.Command(gitPath, "-C", dir, "add", "new.go").Run()
	exec.Command(gitPath, "-C", dir, "-c", "user.email=test@test", "-c", "user.name=Test", "commit", "-m", "main advance").Run()
	// Update the simulated remote ref so origin/main reflects the advanced main
	exec.Command(gitPath, "-C", dir, "update-ref", "refs/remotes/origin/main", "refs/heads/main").Run()
	// Return to feature
	exec.Command(gitPath, "-C", dir, "checkout", "feature").Run()

	// With auto-rebase, this should succeed cleanly and return not stale
	stale, msg, err := IsStale(dir, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stale {
		t.Fatalf("expected auto-rebase to succeed (not stale), got stale with msg: %s", msg)
	}
	// msg should mention auto-rebase or be empty — but push may warn in local-only test,
	// so we just verify not stale.
}

func TestIsStale_AutoRebaseConflicts(t *testing.T) {
	skipIfNoGit(t)
	dir := setupRepo(t)

	// Create feature branch
	exec.Command(gitPath, "-C", dir, "checkout", "-b", "feature").Run()
	// Change main.go on feature
	writeFile(t, dir, "main.go", "package feature\n")
	exec.Command(gitPath, "-C", dir, "add", "main.go").Run()
	exec.Command(gitPath, "-C", dir, "-c", "user.email=test@test", "-c", "user.name=Test", "commit", "-m", "feature change").Run()

	// Return to main and change main.go differently
	exec.Command(gitPath, "-C", dir, "checkout", "main").Run()
	writeFile(t, dir, "main.go", "package mainline\n")
	exec.Command(gitPath, "-C", dir, "add", "main.go").Run()
	exec.Command(gitPath, "-C", dir, "-c", "user.email=test@test", "-c", "user.name=Test", "commit", "-m", "main change").Run()
	// Update remote ref
	exec.Command(gitPath, "-C", dir, "update-ref", "refs/remotes/origin/main", "refs/heads/main").Run()

	// Return to feature
	exec.Command(gitPath, "-C", dir, "checkout", "feature").Run()

	stale, msg, err := IsStale(dir, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stale {
		t.Fatal("expected stale (rebase conflicts), got not stale")
	}
	if !strings.Contains(msg, "rebase failed") && !strings.Contains(strings.ToLower(msg), "conflict") {
		t.Fatalf("expected conflict message, got: %s", msg)
	}
}

func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := exec.Command(gitPath, "-C", dir, "init").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	writeFile(t, dir, "main.go", "package main\n")
	exec.Command(gitPath, "-C", dir, "add", "main.go").Run()
	exec.Command(gitPath, "-C", dir, "-c", "user.email=test@test", "-c", "user.name=Test", "commit", "-m", "initial").Run()

	// Simulate a remote with refs/remotes/origin/main
	exec.Command(gitPath, "-C", dir, "remote", "add", "origin", dir).Run()
	exec.Command(gitPath, "-C", dir, "update-ref", "refs/remotes/origin/main", "refs/heads/main").Run()
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	f := filepath.Join(dir, name)
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
