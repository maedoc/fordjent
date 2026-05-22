// Package scaffold detects empty repositories and ensures a scaffold issue exists
// before other issues are processed. This prevents parallel workstreams from all
// independently creating project files and conflicting.
//
// The scaffold is language-agnostic: it detects the project language from existing
// files (e.g., go.mod → Go, requirements.txt → Python) and generates appropriate
// scaffolding. For truly empty repos, it creates a minimal README.md + .gitignore.
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

	// Ensure required labels exist before any label operations
	if err := writeClient.EnsureLabels(ctx, repo); err != nil {
		slog.Warn("scaffold: failed to ensure labels exist", "error", err, "repo", repo)
	}

	files, err := client.ListRepoFiles(ctx, repo, "")
	if err != nil {
		// Empty repos (no branches yet) return 400 "sha not found [main]".
		// This is expected — treat as empty repo.
		if strings.Contains(err.Error(), "sha not found") || strings.Contains(err.Error(), "400") {
			slog.Info("scaffold: repo has no branches yet, treating as empty", "repo", repo)
		} else {
			slog.Warn("scaffold: failed to list repo files", "error", err, "repo", repo)
		}
	}

	// Detect project language from existing files.
	projectLang := detectProjectLang(files)
	slog.Info("scaffold: detected project language", "repo", repo, "language", projectLang)

	// If repo has enough files and a project manifest, treat as populated.
	if isRepoPopulated(files, projectLang) {
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
		// Create the scaffold issue with language-appropriate guidance.
		title, body := scaffoldIssueContent(projectLang, len(files))
		iss, err := client.CreateIssue(ctx, repo, title, body)
		if err != nil {
			slog.Warn("scaffold: failed to create scaffold issue", "error", err, "repo", repo)
			return false, nil
		}
		_ = writeClient.AddIssueLabels(ctx, repo, iss.Number, []string{ScaffoldLabel})
		scaffoldIssue = iss
		slog.Info("scaffold: created scaffold issue", "repo", repo, "issue", iss.Number, "language", projectLang)
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

// detectProjectLang examines repo files to determine the project language.
func detectProjectLang(files []string) string {
	fileSet := make(map[string]bool)
	for _, f := range files {
		fileSet[f] = true
	}

	// Check for language-specific manifests in priority order.
	switch {
	case fileSet["go.mod"]:
		return "go"
	case fileSet["go.sum"]:
		return "go"
	case fileSet["pyproject.toml"]:
		return "python"
	case fileSet["requirements.txt"]:
		return "python"
	case fileSet["setup.py"] || fileSet["setup.cfg"]:
		return "python"
	case fileSet["Pipfile"]:
		return "python"
	case fileSet["Cargo.toml"]:
		return "rust"
	case fileSet["package.json"]:
		return "javascript"
	case fileSet["pom.xml"] || fileSet["build.gradle"] || fileSet["build.gradle.kts"]:
		return "java"
	case fileSet["Gemfile"]:
		return "ruby"
	case fileSet["composer.json"]:
		return "php"
	}

	// Check file extensions for hints.
	pyCount := 0
	goCount := 0
	for _, f := range files {
		ext := lastExt(f)
		switch ext {
		case ".py":
			pyCount++
		case ".go":
			goCount++
		}
	}
	if pyCount > goCount && pyCount > 0 {
		return "python"
	}
	if goCount > 0 {
		return "go"
	}

	return "unknown"
}

// lastExt returns the file extension (e.g., ".py" from "dir/file.py").
func lastExt(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i:]
		}
		if path[i] == '/' {
			break
		}
	}
	return ""
}

// isRepoPopulated checks if a repo has enough files to be considered scaffolded.
func isRepoPopulated(files []string, projectLang string) bool {
	if len(files) == 0 {
		return false
	}

	// Must have a README.
	hasReadme := false
	for _, f := range files {
		if strings.EqualFold(f, "README.md") || strings.EqualFold(f, "README") || strings.EqualFold(f, "README.txt") {
			hasReadme = true
			break
		}
	}

	// Must have a project manifest appropriate for the language.
	hasManifest := false
	fileSet := make(map[string]bool)
	for _, f := range files {
		fileSet[f] = true
	}

	switch projectLang {
	case "go":
		hasManifest = fileSet["go.mod"]
	case "python":
		hasManifest = fileSet["requirements.txt"] || fileSet["pyproject.toml"] ||
			fileSet["setup.py"] || fileSet["setup.cfg"] || fileSet["Pipfile"]
	case "rust":
		hasManifest = fileSet["Cargo.toml"]
	case "javascript":
		hasManifest = fileSet["package.json"]
	case "java":
		hasManifest = fileSet["pom.xml"] || fileSet["build.gradle"] || fileSet["build.gradle.kts"]
	case "ruby":
		hasManifest = fileSet["Gemfile"]
	case "php":
		hasManifest = fileSet["composer.json"]
	default:
		// Unknown language: if there are 3+ files and a README, consider it populated.
		return len(files) >= 3 && hasReadme
	}

	return hasReadme && hasManifest
}

// scaffoldIssueContent returns the title and body for a scaffold issue.
func scaffoldIssueContent(projectLang string, fileCount int) (string, string) {
	var title, body string

	switch projectLang {
	case "go":
		title = "[scaffold] Set up Go project structure"
		body = fmt.Sprintf(
			"This repository has only %d file(s) and needs a Go project scaffold. "+
				"Please create the following files:\n\n"+
				"- `go.mod` — Go module definition\n"+
				"- `README.md` — Project documentation\n"+
				"- `.gitignore` — Go-specific ignore patterns\n\n"+
				"Other open issues have been labeled `blocked` until the scaffold is in place.",
			fileCount)

	case "python":
		title = "[scaffold] Set up Python project structure"
		body = fmt.Sprintf(
			"This repository has only %d file(s) and needs a Python project scaffold. "+
				"Please create the following files:\n\n"+
				"- `requirements.txt` or `pyproject.toml` — Python dependencies\n"+
				"- `README.md` — Project documentation\n"+
				"- `.gitignore` — Python-specific ignore patterns\n\n"+
				"If the project uses Snakemake, also create:\n"+
				"- `Snakefile` — Workflow definition\n\n"+
				"Other open issues have been labeled `blocked` until the scaffold is in place.",
			fileCount)

	case "rust":
		title = "[scaffold] Set up Rust project structure"
		body = fmt.Sprintf(
			"This repository has only %d file(s) and needs a Rust project scaffold. "+
				"Please create the following files:\n\n"+
				"- `Cargo.toml` — Rust package manifest\n"+
				"- `README.md` — Project documentation\n"+
				"- `.gitignore` — Rust-specific ignore patterns\n\n"+
				"Other open issues have been labeled `blocked` until the scaffold is in place.",
			fileCount)

	case "javascript":
		title = "[scaffold] Set up JavaScript/Node project structure"
		body = fmt.Sprintf(
			"This repository has only %d file(s) and needs a JavaScript project scaffold. "+
				"Please create the following files:\n\n"+
				"- `package.json` — Node package manifest\n"+
				"- `README.md` — Project documentation\n"+
				"- `.gitignore` — Node-specific ignore patterns\n\n"+
				"Other open issues have been labeled `blocked` until the scaffold is in place.",
			fileCount)

	default:
		title = "[scaffold] Set up project structure"
		body = fmt.Sprintf(
			"This repository has only %d file(s) and needs a basic project scaffold. "+
				"Please create the following files:\n\n"+
				"- `README.md` — Project documentation\n"+
				"- `.gitignore` — Appropriate ignore patterns\n\n"+
				"Look at other open issues in this repository for hints about the project's "+
				"language and framework. If the issues mention Python, Snakemake, or BIDS, "+
				"create a Python project structure (requirements.txt, etc.). "+
				"If they mention Go, create a Go project structure (go.mod, etc.).\n\n"+
				"Other open issues have been labeled `blocked` until the scaffold is in place.",
			fileCount)
	}

	return title, body
}
