package ralph

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Guard enforces spec immutability during ralph sessions.
// It checks file paths and commit diffs for spec modifications.
type Guard struct {
	repoDir string
}

// NewGuard creates a Guard for the given repository directory.
func NewGuard(repoDir string) *Guard {
	return &Guard{repoDir: repoDir}
}

// IsSpecPath returns true if the path points to a spec file
// (openspec/**/spec.md). The path is resolved and checked against
// the spec pattern to prevent traversal attacks.
func (g *Guard) IsSpecPath(p string) bool {
	// Normalize the path
	clean := filepath.ToSlash(filepath.Clean(p))

	// Check for spec pattern: openspec/**/spec.md
	// This covers paths like:
	//   openspec/changes/my-feature/spec.md
	//   openspec/specs/auth-core/spec.md
	if strings.Contains(clean, "openspec/") && strings.HasSuffix(clean, "/spec.md") {
		return true
	}

	// Also check resolved path if possible
	absPath := filepath.Join(g.repoDir, clean)
	resolved, err := filepath.EvalSymlinks(absPath)
	if err == nil && resolved != absPath {
		relClean := filepath.ToSlash(filepath.Clean(resolved))
		if strings.Contains(relClean, "openspec/") && strings.HasSuffix(relClean, "/spec.md") {
			return true
		}
	}

	return false
}

// ValidateCommitDiff checks a git diff output for modifications to spec files.
// The diff should be the output of `git diff --cached --name-only` or similar.
// Returns an error listing any spec files found in the diff.
func (g *Guard) ValidateCommitDiff(diff string) error {
	if diff == "" {
		return nil
	}

	var specFiles []string
	lines := strings.Split(diff, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if g.IsSpecPath(line) {
			specFiles = append(specFiles, line)
		}
	}

	if len(specFiles) > 0 {
		return fmt.Errorf("commit touches spec file(s) %s — spec scope/AC are immutable during ralph", strings.Join(specFiles, ", "))
	}

	return nil
}

// IsProgressPath returns true if the path is a ralph progress file
// (.ralph/progress/*.md).
func (g *Guard) IsProgressPath(p string) bool {
	clean := filepath.ToSlash(filepath.Clean(p))
	return strings.HasPrefix(clean, ".ralph/progress/") && strings.HasSuffix(clean, ".md")
}
