// Package scaffold detects empty repositories and ensures a scaffold issue exists
// before other issues are processed. This prevents parallel workstreams from all
// independently creating go.mod / README.md and conflicting.
package scaffold

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fordjent/fordjent/internal/forgejo"
)

const (
	MinFilesForNonEmpty = 3
	ScaffoldLabel       = "scaffold"
)

// CheckAndBlock inspects the repository file count. If it is below the threshold
// and the current issue is not itself the scaffold issue, it either creates a
// scaffold issue or labels the current issue blocked.
// Returns (blocked, error). A non-nil error does not imply blocked.
func CheckAndBlock(ctx context.Context, client *forgejo.Client, repo string, issueNumber int) (bool, error) {
	if client == nil {
		return false, nil
	}

	files, err := client.ListRepoFiles(ctx, repo, "")
	if err != nil {
		slog.Warn("scaffold: failed to list repo files, allowing through", "error", err, "repo", repo)
		return false, nil
	}

	if len(files) >= MinFilesForNonEmpty {
		return false, nil
	}

	// Check whether there is already an open scaffold issue.
	openIssues, err := client.ListOpenIssues(ctx, repo)
	if err != nil {
		slog.Warn("scaffold: failed to list open issues", "error", err, "repo", repo)
		return false, nil
	}

	var scaffoldIssue *forgejo.Issue
	for i := range openIssues {
		iss := &openIssues[i]
		if strings.Contains(strings.ToLower(iss.Title), "scaffold") {
			scaffoldIssue = iss
			break
		}
		// Also check labels if available (Forgejo issue labels are returned inline)
	}

	if scaffoldIssue == nil {
		// Create the scaffold issue
		title := "[scaffold] Add project scaffold (go.mod, README.md, etc.)"
		body := fmt.Sprintf(
			"This repository has only %d file(s) and appears to be missing basic project files. "+
				"Please create `go.mod` and `README.md` (and `.gitignore` if appropriate) before filing feature issues.\n\n"+
				"Other open issues have been labeled `blocked` until the scaffold is in place.",
			len(files))
		iss, err := client.CreateIssue(ctx, repo, title, body)
		if err != nil {
			slog.Warn("scaffold: failed to create scaffold issue", "error", err, "repo", repo)
			return false, nil
		}
		_ = client.AddIssueLabels(ctx, repo, iss.Number, []string{ScaffoldLabel, "blocked"})
		scaffoldIssue = iss
		slog.Info("scaffold: created scaffold issue", "repo", repo, "issue", iss.Number)
	}

	// Label the current issue blocked so the agent doesn't start on it yet.
	if issueNumber > 0 && (scaffoldIssue == nil || scaffoldIssue.Number != issueNumber) {
		if err := client.AddIssueLabels(ctx, repo, issueNumber, []string{"blocked"}); err != nil {
			slog.Warn("scaffold: failed to add blocked label", "error", err, "issue", issueNumber)
		}
		// Post a pointer comment
		msg := fmt.Sprintf("This repository needs a scaffold first. Please wait for #%d to be resolved, then remove the `blocked` label from this issue to continue.", scaffoldIssue.Number)
		if err := client.PostIssueComment(ctx, repo, issueNumber, msg+"\n\n<!-- ford -->"); err != nil {
			slog.Warn("scaffold: failed to post blocked comment", "error", err, "issue", issueNumber)
		}
		return true, nil
	}

	return false, nil
}
