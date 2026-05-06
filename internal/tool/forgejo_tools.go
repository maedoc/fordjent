package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/sentinel"
	"github.com/fordjent/fordjent/internal/stalegate"
)

func escapeRepoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return path.Join(parts...)
}

// forgejoCommentTool posts comments on issues/PRs.
type forgejoCommentTool struct {
	adapter *ForgejoAdapter
}

func NewCommentTool(adapter *ForgejoAdapter) *forgejoCommentTool {
	return &forgejoCommentTool{adapter: adapter}
}

func (t *forgejoCommentTool) Name() string { return "forgejo_comment" }

func (t *forgejoCommentTool) Description() string {
	return "Post a comment on an issue or pull request. Use this to respond to users, provide status updates, or share findings."
}

func (t *forgejoCommentTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"issue_number": map[string]interface{}{
				"type":        "integer",
				"description": "Issue or PR number",
			},
			"body": map[string]interface{}{
				"type":        "string",
				"description": "Comment body in Markdown",
			},
		},
		"required": []string{"repository", "issue_number", "body"},
	}
}

// agentCommentMarker is appended to all agent comments so the webhook router
// can detect self-originated events and break infinite comment loops.
const agentCommentMarker = "\n\n<!-- ford -->"

func (t *forgejoCommentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository  string `json:"repository"`
		IssueNumber int    `json:"issue_number"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	// Append hidden marker + visible agent signature so webhook router can filter
	// self-generated comments and humans can identify bot work.
	signature := "\n\n---\n*🤖 Created by [Fordjent](https://github.com/fordjent/fordjent) autonomous coding agent*"
	body := params.Body + signature + agentCommentMarker

	apiPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository),
		"issues", fmt.Sprintf("%d", params.IssueNumber), "comments")
	_, err := t.adapter.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"body": body})
	if err != nil {
		return "", err
	}
	return "Comment posted successfully", nil
}

// forgejoCreateIssueTool creates a new issue.
type forgejoCreateIssueTool struct {
	adapter        *ForgejoAdapter
	parentIssueNum int
	maxSubIssues   int
	subIssueCount  int
	mu             sync.Mutex
}

func NewCreateIssueTool(adapter *ForgejoAdapter, parentIssueNum int, maxSubIssues int) *forgejoCreateIssueTool {
	return &forgejoCreateIssueTool{
		adapter:      adapter,
		parentIssueNum: parentIssueNum,
		maxSubIssues:   maxSubIssues,
	}
}

func (t *forgejoCreateIssueTool) Name() string { return "forgejo_create_issue" }

func (t *forgejoCreateIssueTool) Description() string {
	return "Create a new issue in the repository. Use this to break down large tasks into smaller tracked issues. Before creating, call forgejo_list_issues to verify a similar issue does not already exist. Title should be concise; body should describe the specific sub-task or requirement. When created from another issue (parent), the new issue is automatically tagged 'blocked'."
}

func (t *forgejoCreateIssueTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"title": map[string]interface{}{
				"type":        "string",
				"description": "Issue title",
			},
			"body": map[string]interface{}{
				"type":        "string",
				"description": "Issue body in Markdown describing the sub-task",
			},
		},
		"required": []string{"repository", "title", "body"},
	}
}

func (t *forgejoCreateIssueTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Title      string `json:"title"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	// Max sub-issue enforcement
	t.mu.Lock()
	if t.maxSubIssues > 0 && t.subIssueCount >= t.maxSubIssues {
		t.mu.Unlock()
		return "", fmt.Errorf("maximum sub-issues reached (%d). Do not create more issues for this task.", t.maxSubIssues)
	}
	t.subIssueCount++
	t.mu.Unlock()

	// Deduplication check: query open issues for similar titles
	listPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "issues") + "?state=open&limit=50"
	listResult, listErr := t.adapter.doRequest(ctx, http.MethodGet, listPath, nil)
	if listErr == nil {
		var existingIssues []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
		}
		_ = json.Unmarshal([]byte(listResult), &existingIssues)
		titleLower := strings.ToLower(params.Title)
		for _, ex := range existingIssues {
			existingLower := strings.ToLower(ex.Title)
			// Require substantial overlap (≥50% of words or substring containment)
			if existingLower == titleLower ||
				(strings.Contains(existingLower, titleLower) && len(titleLower) > 8) ||
				(strings.Contains(titleLower, existingLower) && len(existingLower) > 8) {
				return fmt.Sprintf("Similar issue already exists: #%d '%s' — no new issue created. Use the existing issue instead.", ex.Number, ex.Title), nil
			}
		}
	}

	body := params.Body
	if t.parentIssueNum > 0 {
		body += fmt.Sprintf("\n\nDepends on: #%d", t.parentIssueNum)
		body += "\n\n## Context\n"
		body += fmt.Sprintf("This issue depends on parent issue #%d. ", t.parentIssueNum)
		body += "The code this issue works with will be available in the repository once the parent PR is merged. "
		body += "Wait for the 'ready' label before starting implementation.\n"
	}

	apiPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "issues")
	payload := map[string]string{"title": params.Title, "body": body}
	result, err := t.adapter.doRequest(ctx, http.MethodPost, apiPath, payload)
	if err != nil {
		return "", err
	}

	// If this issue has a parent, auto-tag with 'blocked' label
	if t.parentIssueNum > 0 {
		var created struct {
			Number int `json:"number"`
		}
		_ = json.Unmarshal([]byte(result), &created)
		if created.Number > 0 {
			labelPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "issues", fmt.Sprintf("%d", created.Number), "labels")
			_, _ = t.adapter.doRequest(ctx, http.MethodPost, labelPath, map[string]interface{}{"labels": []string{"blocked"}})
		}
	}

	return fmt.Sprintf("Issue created: %s", result), nil
}

// forgejoListIssuesTool lists issues in a repository.
type forgejoListIssuesTool struct {
	adapter *ForgejoAdapter
}

func NewListIssuesTool(adapter *ForgejoAdapter) *forgejoListIssuesTool {
	return &forgejoListIssuesTool{adapter: adapter}
}

func (t *forgejoListIssuesTool) Name() string { return "forgejo_list_issues" }

func (t *forgejoListIssuesTool) Description() string {
	return "List issues in a repository. Returns issue numbers, titles, and states."
}

func (t *forgejoListIssuesTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"state": map[string]interface{}{
				"type":        "string",
				"description": "Issue state: open, closed, all",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Max issues to return (default 20)",
			},
		},
		"required": []string{"repository"},
	}
}

func (t *forgejoListIssuesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		State      string `json:"state"`
		Limit      int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if params.State == "" {
		params.State = "open"
	}
	if params.Limit == 0 {
		params.Limit = 20
	}

	apiPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "issues")
	query := url.Values{}
	query.Set("state", params.State)
	query.Set("limit", fmt.Sprintf("%d", params.Limit))
	apiPath += "?" + query.Encode()

	return t.adapter.doRequest(ctx, http.MethodGet, apiPath, nil)
}

// forgejoGetIssueTool retrieves a single issue.
type forgejoGetIssueTool struct {
	adapter *ForgejoAdapter
}

func NewGetIssueTool(adapter *ForgejoAdapter) *forgejoGetIssueTool {
	return &forgejoGetIssueTool{adapter: adapter}
}

func (t *forgejoGetIssueTool) Name() string { return "forgejo_get_issue" }

func (t *forgejoGetIssueTool) Description() string {
	return "Get details of a specific issue or pull request including body, labels, and assignees."
}

func (t *forgejoGetIssueTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"issue_number": map[string]interface{}{
				"type":        "integer",
				"description": "Issue or PR number",
			},
		},
		"required": []string{"repository", "issue_number"},
	}
}

func (t *forgejoGetIssueTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository  string `json:"repository"`
		IssueNumber int    `json:"issue_number"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	apiPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository),
		"issues", fmt.Sprintf("%d", params.IssueNumber))
	return t.adapter.doRequest(ctx, http.MethodGet, apiPath, nil)
}

// MergeGate is implemented by the merge-queue system. It checks whether a PR
// would conflict with already open PRs before creation.
type MergeGate interface {
	CheckGate(ctx context.Context, repo, headBranch, baseBranch string) (blocked bool, message string, err error)
}

// forgejoCreatePRTool creates a pull request.
type forgejoCreatePRTool struct {
	adapter        *ForgejoAdapter
	mq             MergeGate
	repoDir        string
	parentIssueNum int
}

func NewCreatePRTool(adapter *ForgejoAdapter, mq MergeGate, repoDir string) *forgejoCreatePRTool {
	return &forgejoCreatePRTool{adapter: adapter, mq: mq, repoDir: repoDir}
}

func (t *forgejoCreatePRTool) SetParentIssueNum(n int) {
	t.parentIssueNum = n
}

func (t *forgejoCreatePRTool) Name() string { return "forgejo_create_pr" }

func (t *forgejoCreatePRTool) Description() string {
	return "Create a pull request from a head branch to a base branch. Use ONLY for submitting NEW code changes. If you are responding to a review on an existing PR, do NOT call this tool — push to the existing branch instead. IMPORTANT: This tool will verify that go build and go test pass before creating the PR. If tests fail, the PR will be blocked and you must fix the failures. The PR description body MUST include three sections: 1) '## Changes' listing every file modified and a one-line summary of what changed, 2) '## Testing' describing how the changes were verified (unit tests, manual checks, build steps), 3) '## Related' containing 'Closes #{issue number}' to link the PR to its originating issue and 'Depends on: #{parent}' if this work depends on another issue/PR."
}

func (t *forgejoCreatePRTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"title": map[string]interface{}{
				"type":        "string",
				"description": "PR title",
			},
			"body": map[string]interface{}{
				"type":        "string",
				"description": "PR description in Markdown",
			},
			"head": map[string]interface{}{
				"type":        "string",
				"description": "Head branch name (source)",
			},
			"base": map[string]interface{}{
				"type":        "string",
				"description": "Base branch name (target)",
			},
		},
		"required": []string{"repository", "title", "head", "base"},
	}
}

func (t *forgejoCreatePRTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Title      string `json:"title"`
		Body       string `json:"body"`
		Head       string `json:"head"`
		Base       string `json:"base"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	// Stale gate: if origin/main has moved ahead of this branch, block
	if t.repoDir != "" {
		stale, msg, err := stalegate.IsStale(t.repoDir, params.Base)
		if err == nil && stale {
			slog.Warn("create_pr: stale branch blocked", "branch", params.Head, "msg", msg)
			return "Stale branch: " + msg, nil
		}
	}

	// Verify branch exists on remote before gating / PR creation.
	// Forgejo can lag behind the push, so retry up to 5× with 1s sleep.
	if t.repoDir != "" {
		found := false
		for i := 0; i < 5; i++ {
			out, err := exec.CommandContext(ctx, "git", "-C", t.repoDir, "ls-remote", "origin", params.Head).CombinedOutput()
			if err == nil && strings.Contains(string(out), params.Head) {
				found = true
				break
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if !found {
			slog.Warn("create_pr: branch not found on remote", "branch", params.Head)
			return "", fmt.Errorf("branch %q not found on remote after 5 retries — did you push?", params.Head)
		}
	}

	// Merge-queue file gate: if any open PR touches the same files, block
	// Retry on transient "bad revision" errors (branch indexing lag) with exponential backoff.
	if t.mq != nil {
		var mqErr error
		for attempt := 0; attempt < 3; attempt++ {
			blocked, msg, err := t.mq.CheckGate(ctx, params.Repository, params.Head, params.Base)
			if err == nil {
				if blocked {
					// Clean up dangling branch so the repo doesn't accumulate stale branches
					slog.Warn("create_pr: merge queue blocked", "branch", params.Head, "msg", msg)
					if t.repoDir != "" {
						cmd := exec.CommandContext(ctx, "git", "-C", t.repoDir, "push", "--delete", "origin", params.Head)
						if out, err := cmd.CombinedOutput(); err != nil {
							slog.Warn("mergequeue: failed to clean up blocked branch", "branch", params.Head, "error", err, "output", string(out))
						}
					}
					return "", fmt.Errorf("%w: %s", sentinel.ErrBlocked, msg)
				}
				break
			}
			mqErr = err
			if errors.Is(err, sentinel.ErrBadRevision) || sentinel.IsRetryable(err) {
				delay := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(delay):
					continue
				}
			}
			return "", err // Non-retryable error
		}
		if mqErr != nil {
			return "", mqErr
		}
	}

	// Verify gate: enforce compilation and tests pass before PR creation.
	// The agent must produce working code before claiming "done".
	if t.repoDir != "" {
		buildCmd := exec.CommandContext(ctx, "go", "build", "./...")
		buildCmd.Dir = t.repoDir
		buildOut, buildErr := buildCmd.CombinedOutput()
		if buildErr != nil {
			return "", fmt.Errorf("go build failed — fix compilation errors before creating PR:\n%s", string(buildOut))
		}

		testCmd := exec.CommandContext(ctx, "go", "test", "./...", "-count=1")
		testCmd.Dir = t.repoDir
		testOut, testErr := testCmd.CombinedOutput()
		if testErr != nil {
			return "", fmt.Errorf("go test failed — all tests must pass before creating PR:\n%s", string(testOut))
		}

		lintCmd := exec.CommandContext(ctx, "golangci-lint", "run", "./...")
		lintCmd.Dir = t.repoDir
		if lintOut, lintErr := lintCmd.CombinedOutput(); lintErr != nil {
			// golangci-lint may not be installed — only fail if it IS installed and finds issues
			if !strings.Contains(lintErr.Error(), "executable file not found") {
				return "", fmt.Errorf("golangci-lint failed — fix lint errors before creating PR:\n%s", string(lintOut))
			}
		}

		slog.Info("verify gate passed", "repo_dir", t.repoDir)
	}

	// Append hidden marker + visible agent signature so webhook router can filter
	// self-generated PRs and humans can identify bot work.
	signature := "\n\n---\n*🤖 Created by [Fordjent](https://github.com/fordjent/fordjent) autonomous coding agent*"
	body := params.Body
	if t.parentIssueNum > 0 {
		body += fmt.Sprintf("\n\nCloses #%d", t.parentIssueNum)
	}
	body += signature + agentCommentMarker

	apiPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "pulls")
	payload := map[string]string{
		"title": params.Title,
		"body":  body,
		"head":  params.Head,
		"base":  params.Base,
	}

	// Retry PR creation on transient errors (bad revision, 500)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result, err := t.adapter.doRequest(ctx, http.MethodPost, apiPath, payload)
		if err == nil {
			slog.Info("create_pr: PR created successfully", "repo", params.Repository, "head", params.Head, "base", params.Base)
			return fmt.Sprintf("Pull request created: %s", result), nil
		}
		slog.Warn("create_pr: API call failed", "attempt", attempt+1, "error", err, "head", params.Head)
		lastErr = err
		if errors.Is(err, sentinel.ErrBadRevision) || sentinel.IsRetryable(err) {
			delay := time.Duration(1<<attempt) * time.Second
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
				continue
			}
		}
		return "", err
	}
	return "", lastErr
}

// forgejoSearchCodeTool searches code in a repository.
type forgejoSearchCodeTool struct {
	adapter *ForgejoAdapter
}

func NewSearchCodeTool(adapter *ForgejoAdapter) *forgejoSearchCodeTool {
	return &forgejoSearchCodeTool{adapter: adapter}
}

func (t *forgejoSearchCodeTool) Name() string { return "forgejo_search_code" }

func (t *forgejoSearchCodeTool) Description() string {
	return "Search for code in a repository. Returns matching file paths and line numbers."
}

func (t *forgejoSearchCodeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Search query",
			},
		},
		"required": []string{"repository", "query"},
	}
}

func (t *forgejoSearchCodeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Query      string `json:"query"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	apiPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "code", "search")
	query := url.Values{}
	query.Set("q", params.Query)
	apiPath += "?" + query.Encode()

	return t.adapter.doRequest(ctx, http.MethodGet, apiPath, nil)
}

// forgejoAddReactionTool adds an emoji reaction.
type forgejoAddReactionTool struct {
	adapter *ForgejoAdapter
}

func NewAddReactionTool(adapter *ForgejoAdapter) *forgejoAddReactionTool {
	return &forgejoAddReactionTool{adapter: adapter}
}

func (t *forgejoAddReactionTool) Name() string { return "forgejo_add_reaction" }

func (t *forgejoAddReactionTool) Description() string {
	return "Add an emoji reaction to an issue, pull request, or comment. Supported: +1, -1, laugh, hooray, confused, heart, rocket, eyes."
}

func (t *forgejoAddReactionTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"issue_number": map[string]interface{}{
				"type":        "integer",
				"description": "Issue or PR number",
			},
			"comment_id": map[string]interface{}{
				"type":        "integer",
				"description": "Comment ID (0 for issue/PR itself)",
			},
			"reaction": map[string]interface{}{
				"type":        "string",
				"description": "Emoji reaction name (e.g., eyes, +1, rocket)",
			},
		},
		"required": []string{"repository", "issue_number", "reaction"},
	}
}

func (t *forgejoAddReactionTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository  string `json:"repository"`
		IssueNumber int    `json:"issue_number"`
		CommentID   int    `json:"comment_id"`
		Reaction    string `json:"reaction"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	var apiPath string
	if params.CommentID > 0 {
		apiPath = path.Join("/api/v1/repos", escapeRepoPath(params.Repository),
			"issues", "comments", fmt.Sprintf("%d", params.CommentID), "reactions")
	} else {
		apiPath = path.Join("/api/v1/repos", escapeRepoPath(params.Repository),
			"issues", fmt.Sprintf("%d", params.IssueNumber), "reactions")
	}

	_, err := t.adapter.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"content": params.Reaction})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Reaction '%s' added", params.Reaction), nil
}

// --- forgejo_merge_pr ---

type forgejoMergePRTool struct {
	adapter             *ForgejoAdapter
	bypassHumanApproval bool // set true for reviewer/devops roles that can merge without external human
}

func NewMergePRTool(adapter *ForgejoAdapter, bypassHumanApproval bool) *forgejoMergePRTool {
	return &forgejoMergePRTool{adapter: adapter, bypassHumanApproval: bypassHumanApproval}
}

func (t *forgejoMergePRTool) Name() string        { return "forgejo_merge_pr" }
func (t *forgejoMergePRTool) Description() string {
	return "Merge an existing pull request. Only call this after pushing fixes to a PR branch and confirming it has no conflicts."
}
func (t *forgejoMergePRTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"pr_number": map[string]interface{}{
				"type":        "integer",
				"description": "Pull request number to merge",
			},
			"style": map[string]interface{}{
				"type":        "string",
				"description": "Merge style: merge, rebase-merge, squash-merge",
			},
		},
		"required": []string{"repository", "pr_number", "style"},
	}
}

func (t *forgejoMergePRTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		PRNumber   int    `json:"pr_number"`
		Style      string `json:"style"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	// Fetch PR details to check mergeable status (advisory, Forgejo may return mergeable).
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "pulls", fmt.Sprintf("%d", params.PRNumber))
	result, err := t.adapter.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return "", fmt.Errorf("get PR details: %w", err)
	}
	var pr struct {
		Merged       bool   `json:"merged"`
		State        string `json:"state"`
		Mergeable    bool   `json:"mergeable"`
		HasConflicts bool   `json:"has_conflits"`
	}
	_ = json.Unmarshal([]byte(result), &pr)

	if pr.Merged || pr.State == "closed" {
		return fmt.Sprintf("PR #%d is already merged/closed — no action needed.", params.PRNumber), nil
	}
	if pr.HasConflicts {
		return "", fmt.Errorf("PR #%d has conflicts — please resolve before merging", params.PRNumber)
	}
	if !pr.Mergeable {
		return "", fmt.Errorf("PR #%d is not mergeable yet — check status requirements", params.PRNumber)
	}

	// Human review gate: at least one non-bot APPROVED review required
	if !t.bypassHumanApproval {
		reviews, err := t.adapter.Client().ListPRReviews(ctx, params.Repository, params.PRNumber)
		if err != nil {
			return "", fmt.Errorf("failed to list reviews for PR #%d: %w", params.PRNumber, err)
		}
		approved := false
		for _, r := range reviews {
			if r.State == "APPROVED" {
				// Skip bot approvals (anything matching fordjent bot login pattern)
				if r.User != nil {
					login := strings.ToLower(r.User.Login)
					if login == "fordjent-bot" || login == "fordjent[bot]" {
						continue
					}
				}
				approved = true
				break
			}
		}
		if !approved {
			return "", fmt.Errorf("PR #%d cannot be merged: no human approval found. Ask a human to review and approve first.", params.PRNumber)
		}
	}

	mergePath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "pulls", fmt.Sprintf("%d", params.PRNumber), "merge")

	// Retry merge on transient 405 (Forgejo may need time to process refs after push)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			slog.Info("merge_pr: retrying after 405", "attempt", attempt+1, "pr", params.PRNumber)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			}
		}
		_, err = t.adapter.doRequest(ctx, http.MethodPost, mergePath, map[string]string{"Do": params.Style})
		if err == nil {
			return fmt.Sprintf("PR #%d merged successfully using '%s'", params.PRNumber, params.Style), nil
		}
		lastErr = err
		var apiErr *sentinel.ErrAPIClient
		if errors.As(err, &apiErr) && (apiErr.StatusCode == 405 || apiErr.StatusCode == 409) {
			continue // 405 = try again later, 409 = conflict (may resolve)
		}
		break // other errors are not retryable
	}
	return "", fmt.Errorf("merge PR #%d after 3 attempts: %w", params.PRNumber, lastErr)
}

// ForgejoAdapter holds shared Forgejo client for API tools.
// It wraps forgejo.Client for compatibility with existing tools.
type ForgejoAdapter struct {
	client *forgejo.Client
	// Legacy fields for backward compatibility with mergequeue/scheduler
	baseURL string
	token   string
}

func NewForgejoAdapter(baseURL, token string) *ForgejoAdapter {
	return &ForgejoAdapter{
		client:  forgejo.NewClient(baseURL, token),
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
	}
}

// NewForgejoAdapterFromClient creates an adapter from an existing client.
func NewForgejoAdapterFromClient(client *forgejo.Client) *ForgejoAdapter {
	return &ForgejoAdapter{client: client}
}

// Client returns the underlying forgejo client.
func (a *ForgejoAdapter) Client() *forgejo.Client {
	return a.client
}

// BaseURL returns the Forgejo base URL (for backward compatibility).
func (a *ForgejoAdapter) BaseURL() string {
	return a.baseURL
}

// Token returns the API token (for backward compatibility).
func (a *ForgejoAdapter) Token() string {
	return a.token
}

// HTTPClient returns an http.Client for direct use (for backward compatibility).
func (a *ForgejoAdapter) HTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// doRequest is a shared helper that delegates to the client.
// Kept for backward compatibility with tools that still use raw API paths.
func (a *ForgejoAdapter) doRequest(ctx context.Context, method, apiPath string, body interface{}) (string, error) {
	return a.client.RawRequest(ctx, method, apiPath, body)
}

// === NEW TOOLS ===

// forgejoListBranchesTool lists branches in a repository.
type forgejoListBranchesTool struct {
	adapter *ForgejoAdapter
}

func NewListBranchesTool(adapter *ForgejoAdapter) *forgejoListBranchesTool {
	return &forgejoListBranchesTool{adapter: adapter}
}

func (t *forgejoListBranchesTool) Name() string { return "forgejo_list_branches" }

func (t *forgejoListBranchesTool) Description() string {
	return "List all branches in a repository. Returns branch names, commit SHAs, and protection status."
}

func (t *forgejoListBranchesTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
		},
		"required": []string{"repository"},
	}
}

func (t *forgejoListBranchesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	branches, err := t.adapter.Client().ListBranches(ctx, params.Repository)
	if err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(branches, "", "  ")
	return string(result), nil
}

// forgejoDeleteBranchTool deletes a branch.
type forgejoDeleteBranchTool struct {
	adapter *ForgejoAdapter
}

func NewDeleteBranchTool(adapter *ForgejoAdapter) *forgejoDeleteBranchTool {
	return &forgejoDeleteBranchTool{adapter: adapter}
}

func (t *forgejoDeleteBranchTool) Name() string { return "forgejo_delete_branch" }

func (t *forgejoDeleteBranchTool) Description() string {
	return "Delete a branch from a repository. Use after merging a PR to clean up feature branches."
}

func (t *forgejoDeleteBranchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"branch": map[string]interface{}{
				"type":        "string",
				"description": "Branch name to delete",
			},
		},
		"required": []string{"repository", "branch"},
	}
}

func (t *forgejoDeleteBranchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if err := t.adapter.Client().DeleteBranch(ctx, params.Repository, params.Branch); err != nil {
		return "", err
	}
	return fmt.Sprintf("Branch '%s' deleted", params.Branch), nil
}

// forgejoListHooksTool lists webhooks.
type forgejoListHooksTool struct {
	adapter *ForgejoAdapter
}

func NewListHooksTool(adapter *ForgejoAdapter) *forgejoListHooksTool {
	return &forgejoListHooksTool{adapter: adapter}
}

func (t *forgejoListHooksTool) Name() string { return "forgejo_list_hooks" }

func (t *forgejoListHooksTool) Description() string {
	return "List webhooks for a repository. Returns hook IDs, types, URLs, and active status."
}

func (t *forgejoListHooksTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
		},
		"required": []string{"repository"},
	}
}

func (t *forgejoListHooksTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	hooks, err := t.adapter.Client().ListWebhooks(ctx, params.Repository)
	if err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(hooks, "", "  ")
	return string(result), nil
}

// forgejoCreateHookTool creates a webhook.
type forgejoCreateHookTool struct {
	adapter *ForgejoAdapter
}

func NewCreateHookTool(adapter *ForgejoAdapter) *forgejoCreateHookTool {
	return &forgejoCreateHookTool{adapter: adapter}
}

func (t *forgejoCreateHookTool) Name() string { return "forgejo_create_hook" }

func (t *forgejoCreateHookTool) Description() string {
	return "Create a webhook for a repository. Events default to push if not specified."
}

func (t *forgejoCreateHookTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"url": map[string]interface{}{
				"type":        "string",
				"description": "Webhook URL",
			},
			"secret": map[string]interface{}{
				"type":        "string",
				"description": "Webhook secret (optional)",
			},
			"events": map[string]interface{}{
				"type":        "array",
				"items":       "string",
				"description": "Event types (e.g., push, pull_request)",
			},
		},
		"required": []string{"repository", "url"},
	}
}

func (t *forgejoCreateHookTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string   `json:"repository"`
		URL        string   `json:"url"`
		Secret     string   `json:"secret"`
		Events     []string `json:"events"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if len(params.Events) == 0 {
		params.Events = []string{"push"}
	}

	hook, err := t.adapter.Client().CreateWebhook(ctx, params.Repository, "forgejo", params.URL, params.Secret, params.Events)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Webhook #%d created: %s", hook.ID, params.URL), nil
}

// forgejoDeleteHookTool deletes a webhook.
type forgejoDeleteHookTool struct {
	adapter *ForgejoAdapter
}

func NewDeleteHookTool(adapter *ForgejoAdapter) *forgejoDeleteHookTool {
	return &forgejoDeleteHookTool{adapter: adapter}
}

func (t *forgejoDeleteHookTool) Name() string { return "forgejo_delete_hook" }

func (t *forgejoDeleteHookTool) Description() string {
	return "Delete a webhook from a repository."
}

func (t *forgejoDeleteHookTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"hook_id": map[string]interface{}{
				"type":        "integer",
				"description": "Webhook ID",
			},
		},
		"required": []string{"repository", "hook_id"},
	}
}

func (t *forgejoDeleteHookTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		HookID     int    `json:"hook_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if err := t.adapter.Client().DeleteWebhook(ctx, params.Repository, params.HookID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Webhook #%d deleted", params.HookID), nil
}

// forgejoListFilesTool lists files in a directory.
type forgejoListFilesTool struct {
	adapter *ForgejoAdapter
}

func NewListFilesTool(adapter *ForgejoAdapter) *forgejoListFilesTool {
	return &forgejoListFilesTool{adapter: adapter}
}

func (t *forgejoListFilesTool) Name() string { return "forgejo_list_files" }

func (t *forgejoListFilesTool) Description() string {
	return "List files in a repository directory. Returns file/directory names, types, and paths."
}

func (t *forgejoListFilesTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Directory path (default: root)",
			},
			"ref": map[string]interface{}{
				"type":        "string",
				"description": "Branch/commit ref (default: main)",
			},
		},
		"required": []string{"repository"},
	}
}

func (t *forgejoListFilesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Path       string `json:"path"`
		Ref        string `json:"ref"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	files, err := t.adapter.Client().ListDir(ctx, params.Repository, params.Ref, params.Path)
	if err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(files, "", "  ")
	return string(result), nil
}

// forgejoPRFilesTool lists files changed in a PR.
type forgejoPRFilesTool struct {
	adapter *ForgejoAdapter
}

func NewPRFilesTool(adapter *ForgejoAdapter) *forgejoPRFilesTool {
	return &forgejoPRFilesTool{adapter: adapter}
}

func (t *forgejoPRFilesTool) Name() string { return "forgejo_pr_files" }

func (t *forgejoPRFilesTool) Description() string {
	return "List files changed in a pull request. Returns filenames, status (added/modified/removed), and +/- counts."
}

func (t *forgejoPRFilesTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"pr_number": map[string]interface{}{
				"type":        "integer",
				"description": "Pull request number",
			},
		},
		"required": []string{"repository", "pr_number"},
	}
}

func (t *forgejoPRFilesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		PRNumber   int    `json:"pr_number"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	files, err := t.adapter.Client().GetPRFiles(ctx, params.Repository, params.PRNumber)
	if err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(files, "", "  ")
	return string(result), nil
}

// forgejoListCollabsTool lists collaborators.
type forgejoListCollabsTool struct {
	adapter *ForgejoAdapter
}

func NewListCollabsTool(adapter *ForgejoAdapter) *forgejoListCollabsTool {
	return &forgejoListCollabsTool{adapter: adapter}
}

func (t *forgejoListCollabsTool) Name() string { return "forgejo_list_collabs" }

func (t *forgejoListCollabsTool) Description() string {
	return "List collaborators for a repository."
}

func (t *forgejoListCollabsTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
		},
		"required": []string{"repository"},
	}
}

func (t *forgejoListCollabsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	collabs, err := t.adapter.Client().ListCollaborators(ctx, params.Repository)
	if err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(collabs, "", "  ")
	return string(result), nil
}

// forgejoGetVersionTool returns server version.
type forgejoGetVersionTool struct {
	adapter *ForgejoAdapter
}

func NewGetVersionTool(adapter *ForgejoAdapter) *forgejoGetVersionTool {
	return &forgejoGetVersionTool{adapter: adapter}
}

func (t *forgejoGetVersionTool) Name() string { return "forgejo_version" }

func (t *forgejoGetVersionTool) Description() string {
	return "Get the Forgejo server version."
}

func (t *forgejoGetVersionTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func (t *forgejoGetVersionTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	version, err := t.adapter.Client().GetVersion(ctx)
	if err != nil {
		return "", err
	}
	return version.Version, nil
}

// forgejoGetUserTool returns current user.
type forgejoGetUserTool struct {
	adapter *ForgejoAdapter
}

func NewGetUserTool(adapter *ForgejoAdapter) *forgejoGetUserTool {
	return &forgejoGetUserTool{adapter: adapter}
}

func (t *forgejoGetUserTool) Name() string { return "forgejo_user" }

func (t *forgejoGetUserTool) Description() string {
	return "Get the currently authenticated user."
}

func (t *forgejoGetUserTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func (t *forgejoGetUserTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	user, err := t.adapter.Client().GetCurrentUser(ctx)
	if err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(user, "", "  ")
	return string(result), nil
}

// forgejoCreateTokenTool creates an access token.
type forgejoCreateTokenTool struct {
	adapter *ForgejoAdapter
}

func NewCreateTokenTool(adapter *ForgejoAdapter) *forgejoCreateTokenTool {
	return &forgejoCreateTokenTool{adapter: adapter}
}

func (t *forgejoCreateTokenTool) Name() string { return "forgejo_create_token" }

func (t *forgejoCreateTokenTool) Description() string {
	return "Create a new access token for a user."
}

func (t *forgejoCreateTokenTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"username": map[string]interface{}{
				"type":        "string",
				"description": "Username to create token for",
			},
			"token_name": map[string]interface{}{
				"type":        "string",
				"description": "Name for the new token",
			},
		},
		"required": []string{"username", "token_name"},
	}
}

func (t *forgejoCreateTokenTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Username  string `json:"username"`
		TokenName string `json:"token_name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	token, err := t.adapter.Client().CreateToken(ctx, params.Username, params.TokenName)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Token created: %s", token.Token), nil
}

// forgejoListPRsTool lists pull requests.
type forgejoListPRsTool struct {
	adapter *ForgejoAdapter
}

func NewListPRsTool(adapter *ForgejoAdapter) *forgejoListPRsTool {
	return &forgejoListPRsTool{adapter: adapter}
}

func (t *forgejoListPRsTool) Name() string { return "forgejo_list_prs" }

func (t *forgejoListPRsTool) Description() string {
	return "List pull requests in a repository. Returns PR numbers, titles, states, and branches."
}

func (t *forgejoListPRsTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository": map[string]interface{}{
				"type":        "string",
				"description": "Repository in owner/repo format",
			},
			"state": map[string]interface{}{
				"type":        "string",
				"description": "PR state: open, closed, all",
			},
		},
		"required": []string{"repository"},
	}
}

func (t *forgejoListPRsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	prs, err := t.adapter.Client().ListPRs(ctx, params.Repository, params.State)
	if err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(prs, "", "  ")
	return string(result), nil
}
