// Package stalegate detects whether a local feature branch is stale compared to
// the remote base branch (e.g. origin/main). It uses git plumbing commands and
// is designed to be called before forgejo_create_pr.
//
// If the branch is stale, IsStale attempts an automatic rebase (git rebase origin/base)
// followed by a force-push. If the rebase succeeds, it returns (false, "", nil) so
// the caller can proceed. If it fails due to conflicts, it returns (true, "message", nil).
package stalegate

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// IsStale checks whether origin/<base> is an ancestor of HEAD.
// If not, it tries to automatically rebase on origin/<base> and push.
// Returns (true, message, nil) if the branch is stale and could not be auto-rebased.
func IsStale(repoDir, base string) (bool, string, error) {
	if base == "" {
		base = "main"
	}

	// Fast path: merge-base without fetching.
	if clean, err := isAncestor(repoDir, base); err == nil && clean {
		return false, "", nil
	}

	// Fetch the remote ref.
	if out, err := exec.Command("git", "-C", repoDir, "fetch", "origin", base).CombinedOutput(); err != nil {
		outStr := strings.TrimSpace(string(out))
		// If the remote has no base branch yet (empty repo), it's not "stale".
		if strings.Contains(outStr, "couldn't find remote ref") {
			slog.Info("stalegate: remote has no base branch yet (empty repo), treating as not stale", "repoDir", repoDir, "base", base)
			return false, "", nil
		}
		slog.Warn("stalegate: git fetch failed", "error", err, "output", outStr, "repoDir", repoDir)
	}

	// Retry merge-base after fetch.
	if clean, err := isAncestor(repoDir, base); err == nil && clean {
		return false, "", nil
	}

	// Still not an ancestor — attempt automatic rebase.
	rebOut, rebErr := exec.Command("git", "-C", repoDir, "rebase", "origin/"+base).CombinedOutput()
	if rebErr == nil {
		// Rebase succeeded — push the rebased branch.
		pushOut, pushErr := exec.Command("git", "-C", repoDir, "push", "-f", "-u", "origin", "HEAD").CombinedOutput()
		if pushErr != nil {
			slog.Warn("stalegate: push after rebase failed", "error", pushErr, "output", strings.TrimSpace(string(pushOut)), "repoDir", repoDir)
			return true, fmt.Sprintf(
				"Auto-rebased successfully, but push failed: %s. Please push manually.",
				strings.TrimSpace(string(pushOut))), nil
		}

		// Verify after push.
		if clean, err := isAncestor(repoDir, base); err != nil {
			return true, "", fmt.Errorf("stalegate: merge-base check after rebase failed: %w", err)
		} else if !clean {
			return true, "Auto-rebase completed but branch is still stale. Please investigate.", nil
		}
		return false, "", nil
	}

	// Rebase failed — likely conflicts.
	rebMsg := strings.TrimSpace(string(rebOut))
	return true, fmt.Sprintf(
		"This branch is stale and automatic rebase failed:\n%s\n\n"+
			"Please resolve the conflicts manually, then run:\n"+
			"  git rebase origin/%s\n  git push -f origin HEAD",
		rebMsg, base), nil
}

func isAncestor(repoDir, base string) (bool, error) {
	cmd := exec.Command("git", "-C", repoDir, "merge-base", "--is-ancestor", "origin/"+base, "HEAD")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
		return false, nil // not an ancestor, not an error
	}
	return false, fmt.Errorf("stalegate: merge-base failed: %w", err)
}
