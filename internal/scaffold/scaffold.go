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
// and the current issue is not itself the scaffold issue, it posts clarifying
// questions and blocks the issue until the human answers.
//
// After the human replies, a scaffold answer session will:
//  1. Read the human's answers from issue comments
//  2. Create the appropriate scaffold files
//  3. Remove "question" and "blocked" labels
//  4. Close the question issue (it IS the scaffold issue)
//
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

	// Build the question with language hints from existing files and sibling issues.
	questionBody := scaffoldQuestionBody(projectLang, files)

	// If the triggering issue itself is a [scaffold] issue created by a previous
	// scaffold detection, don't re-post questions — just let it proceed.
	if issueNumber > 0 {
		iss, err := client.GetIssue(ctx, repo, issueNumber)
		if err == nil && strings.Contains(strings.ToLower(iss.Title), "scaffold") {
			slog.Info("scaffold: triggering issue is itself a scaffold issue, skipping question",
				"repo", repo, "issue", issueNumber)
			return false, nil
		}
	}

	// Check for an existing scaffold question issue (open, with "question" label).
	openIssues, err := client.ListOpenIssues(ctx, repo)
	if err != nil {
		slog.Warn("scaffold: failed to list open issues", "error", err, "repo", repo)
		return false, nil
	}

	var existingQuestionIssue *forgejo.Issue
	for i := range openIssues {
		iss := &openIssues[i]
		for _, l := range iss.Labels {
			if l.Name == "question" {
				existingQuestionIssue = iss
				break
			}
		}
		if existingQuestionIssue != nil {
			break
		}
	}

	if existingQuestionIssue != nil {
		// A question already exists. If this isn't it, point to it and block.
		if issueNumber > 0 && existingQuestionIssue.Number != issueNumber {
			_ = writeClient.AddIssueLabels(ctx, repo, issueNumber, []string{"blocked"})
			msg := fmt.Sprintf("I have questions about this project's setup on #%d. "+
				"Please answer there, and I'll set up the project scaffold."+
				"\n\nOnce the scaffold is done and the `question` label is removed, "+
				"I can work on this issue.",
				existingQuestionIssue.Number)
			_ = client.PostIssueComment(ctx, repo, issueNumber, msg+"\n\n<!-- ford -->")
			slog.Info("scaffold: blocked issue while question exists",
				"repo", repo, "issue", issueNumber, "question_issue", existingQuestionIssue.Number)
			return true, nil
		}
		// This IS the question issue. Check if human has answered.
		comments, err := client.ListComments(ctx, repo, existingQuestionIssue.Number)
		if err == nil {
			for _, c := range comments {
				// Skip bot comments (marker check)
				if strings.Contains(c.Body, "<!-- ford -->") {
					continue
				}
				// Human has answered! Let the session proceed with scaffold-answer mode.
				slog.Info("scaffold: human answered scaffold question",
					"repo", repo, "issue", existingQuestionIssue.Number)
				return false, nil
			}
		}
		// No human answer yet. Don't block — the issue already exists as question.
		slog.Info("scaffold: waiting for human answer on question",
			"repo", repo, "issue", existingQuestionIssue.Number)
		return false, nil
	}

	// No existing question issue: create one on the triggering issue.
	if issueNumber > 0 {
		_ = writeClient.AddIssueLabels(ctx, repo, issueNumber, []string{"question"})
		_ = client.PostIssueComment(ctx, repo, issueNumber, questionBody+"\n\n<!-- ford -->")
		slog.Info("scaffold: posted scaffold question", "repo", repo, "issue", issueNumber)
		return true, nil
	}

	return false, nil
}

// scaffoldQuestionBody generates a question comment asking about language and framework.
func scaffoldQuestionBody(projectLang string, files []string) string {
	lang := "unknown"
	if projectLang != "" {
		lang = projectLang
	}

	sb := strings.Builder{}
	sb.WriteString("### I need a few details to set up this project correctly\n\n")

	if lang != "unknown" {
		sb.WriteString(fmt.Sprintf("I detected **%s** files. Is this a %s project?\n\n", lang, lang))
	}

	sb.WriteString("**Please reply with:**\n\n")
	sb.WriteString("1. **Language**: Python, Go, Rust, JavaScript, Java, Ruby, PHP, or other?\n")
	sb.WriteString("2. **Framework** (if any): Django, Flask, Snakemake, React, etc.\n")
	sb.WriteString("3. **Project name**: What should I call this project?\n")
	sb.WriteString("4. **Any other requirements**: Testing framework, CI, specific file structure?\n\n")
	sb.WriteString("I'll set up the scaffold once you reply!")

	return sb.String()
}

// DetectProjectLang fetches the repo file list and determines the project language.
func DetectProjectLang(ctx context.Context, forgejoClient *forgejo.Client, repo string) string {
	files, err := forgejoClient.ListRepoFiles(ctx, repo, "")
	if err != nil {
		slog.Warn("scaffold: failed to list repo files for language detection", "error", err)
		return ""
	}
	return detectProjectLang(files)
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
