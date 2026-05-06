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
	ScaffoldLabel = "scaffold"
)

// CheckAndBlock inspects the repository file count. If it is below the threshold
// and the current issue is not itself the scaffold issue, it either creates a
// scaffold issue or labels the current issue blocked.
// If adminClient is non-nil, it is used to add the bot as collaborator and apply
// labels (requires repo-owner-level permissions that the bot token may not have).
// Returns (blocked, error). A non-nil error does not imply blocked.
func CheckAndBlock(ctx context.Context, client *forgejo.Client, repo string, issueNumber int, adminClient *forgejo.Client) (bool, error) {
	if client == nil {
		return false, nil
	}

	// Use adminClient for write operations (label add, collab add).
	// The bot token may not have repo-level permissions yet.
	writeClient := client
	if adminClient != nil {
		writeClient = adminClient
	}

	files, err := client.ListRepoFiles(ctx, repo, "")
	if err != nil {
		slog.Warn("scaffold: failed to list repo files", "error", err, "repo", repo)
	}

	// If repo has zero files OR lacks go.mod/README.md, treat as empty.
	hasGoMod := false
	hasReadme := false
	for _, f := range files {
		if f == "go.mod" {
			hasGoMod = true
		}
		if strings.EqualFold(f, "README.md") {
			hasReadme = true
		}
	}
	if len(files) > 0 && hasGoMod && hasReadme {
		return false, nil
	}

	// Ensure bot has write access to the repo (needed for push, labels, PRs).
	// Do this FIRST so subsequent label operations can use either token.
	if adminClient != nil {
		if err := adminClient.AddCollaborator(ctx, repo, "fordjent-bot", "admin"); err != nil {
			slog.Debug("scaffold: could not add bot as collaborator (may already have access)", "error", err, "repo", repo)
		} else {
			slog.Info("scaffold: added fordjent-bot as admin collaborator", "repo", repo)
		}
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
		_ = writeClient.AddIssueLabels(ctx, repo, iss.Number, []string{ScaffoldLabel, "blocked"})
		scaffoldIssue = iss
		slog.Info("scaffold: created scaffold issue", "repo", repo, "issue", iss.Number)
	}

	// Label the current issue blocked so the agent doesn't start on it yet.
	if issueNumber > 0 && (scaffoldIssue == nil || scaffoldIssue.Number != issueNumber) {
		if err := writeClient.AddIssueLabels(ctx, repo, issueNumber, []string{"blocked"}); err != nil {
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
