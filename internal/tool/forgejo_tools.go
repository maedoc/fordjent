package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strings"
	"time"

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

	// Append hidden marker so webhook router can filter self-generated comments
	body := params.Body + agentCommentMarker

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
}

func NewCreateIssueTool(adapter *ForgejoAdapter, parentIssueNum int) *forgejoCreateIssueTool {
	return &forgejoCreateIssueTool{adapter: adapter, parentIssueNum: parentIssueNum}
}

func (t *forgejoCreateIssueTool) Name() string { return "forgejo_create_issue" }

func (t *forgejoCreateIssueTool) Description() string {
	return "Create a new issue in the repository. Use this to break down large tasks into smaller tracked issues. Before creating, call forgejo_list_issues to verify a similar issue does not already exist. Title should be concise; body should describe the specific sub-task or requirement."
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
	}

	apiPath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "issues")
	payload := map[string]string{"title": params.Title, "body": body}
	result, err := t.adapter.doRequest(ctx, http.MethodPost, apiPath, payload)
	if err != nil {
		return "", err
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
	adapter *ForgejoAdapter
	mq      MergeGate
	repoDir string
}

func NewCreatePRTool(adapter *ForgejoAdapter, mq MergeGate, repoDir string) *forgejoCreatePRTool {
	return &forgejoCreatePRTool{adapter: adapter, mq: mq, repoDir: repoDir}
}

func (t *forgejoCreatePRTool) Name() string { return "forgejo_create_pr" }

func (t *forgejoCreatePRTool) Description() string {
	return "Create a pull request from a head branch to a base branch. Use ONLY for submitting NEW code changes. If you are responding to a review on an existing PR, do NOT call this tool — push to the existing branch instead."
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
					return fmt.Sprintf("Merge-queue block: %s", msg), nil
				}
				break
			}
			mqErr = err
			if strings.Contains(err.Error(), "bad revision") {
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

	// Append hidden marker so webhook router can filter self-generated PRs
	body := params.Body + agentCommentMarker

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
			return fmt.Sprintf("Pull request created: %s", result), nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "bad revision") || strings.Contains(err.Error(), "500") {
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
	adapter *ForgejoAdapter
}

func NewMergePRTool(adapter *ForgejoAdapter) *forgejoMergePRTool {
	return &forgejoMergePRTool{adapter: adapter}
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
		Mergeable    bool `json:"mergeable"`
		HasConflicts bool `json:"has_conflits"`
	}
	_ = json.Unmarshal([]byte(result), &pr)

	if pr.HasConflicts {
		return "", fmt.Errorf("PR #%d has conflicts — please resolve before merging", params.PRNumber)
	}
	if !pr.Mergeable {
		return "", fmt.Errorf("PR #%d is not mergeable yet — check status requirements", params.PRNumber)
	}

	mergePath := path.Join("/api/v1/repos", escapeRepoPath(params.Repository), "pulls", fmt.Sprintf("%d", params.PRNumber), "merge")
	_, err = t.adapter.doRequest(ctx, http.MethodPost, mergePath, map[string]string{"Do": params.Style})
	if err != nil {
		return "", fmt.Errorf("merge PR #%d: %w", params.PRNumber, err)
	}
	return fmt.Sprintf("PR #%d merged successfully using '%s'", params.PRNumber, params.Style), nil
}

// ForgejoAdapter holds shared HTTP client and credentials for Forgejo API tools.
type ForgejoAdapter struct {
	Client  *http.Client
	BaseURL string
	Token   string
}

func NewForgejoAdapter(baseURL, token string) *ForgejoAdapter {
	return &ForgejoAdapter{
		Client:  &http.Client{Timeout: 30 * time.Second},
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
	}
}

// doRequest is a shared helper that handles auth, request construction, and error handling.
func (a *ForgejoAdapter) doRequest(ctx context.Context, method, apiPath string, body interface{}) (string, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	fullURL := a.BaseURL + apiPath
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+a.Token)

	resp, err := a.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}
	return string(respBody), nil
}
