package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// forgejoCommentTool posts comments on issues/PRs.
type forgejoCommentTool struct {
	client  *http.Client
	baseURL string
	token   string
}

func NewCommentTool(adapter *ForgejoAdapter) *forgejoCommentTool {
	return &forgejoCommentTool{
		client:  adapter.Client,
		baseURL: adapter.BaseURL,
		token:   adapter.Token,
	}
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

func (t *forgejoCommentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Repository  string `json:"repository"`
		IssueNumber int    `json:"issue_number"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/repos/%s/issues/%d/comments",
		t.baseURL, params.Repository, params.IssueNumber)
	payload := map[string]string{"body": params.Body}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return "Comment posted successfully", nil
}

// forgejoListIssuesTool lists issues in a repository.
type forgejoListIssuesTool struct {
	client  *http.Client
	baseURL string
	token   string
}

func NewListIssuesTool(adapter *ForgejoAdapter) *forgejoListIssuesTool {
	return &forgejoListIssuesTool{
		client:  adapter.Client,
		baseURL: adapter.BaseURL,
		token:   adapter.Token,
	}
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

	url := fmt.Sprintf("%s/api/v1/repos/%s/issues?state=%s&limit=%d",
		t.baseURL, params.Repository, params.State, params.Limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return string(respBody), nil
}

// forgejoGetIssueTool retrieves a single issue.
type forgejoGetIssueTool struct {
	client  *http.Client
	baseURL string
	token   string
}

func NewGetIssueTool(adapter *ForgejoAdapter) *forgejoGetIssueTool {
	return &forgejoGetIssueTool{
		client:  adapter.Client,
		baseURL: adapter.BaseURL,
		token:   adapter.Token,
	}
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

	url := fmt.Sprintf("%s/api/v1/repos/%s/issues/%d",
		t.baseURL, params.Repository, params.IssueNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return string(respBody), nil
}

// forgejoCreatePRTool creates a pull request.
type forgejoCreatePRTool struct {
	client            *http.Client
	baseURL           string
	token             string
	protectedBranches []string
}

func NewCreatePRTool(adapter *ForgejoAdapter, protectedBranches []string) *forgejoCreatePRTool {
	return &forgejoCreatePRTool{
		client:            adapter.Client,
		baseURL:           adapter.BaseURL,
		token:             adapter.Token,
		protectedBranches: protectedBranches,
	}
}

func (t *forgejoCreatePRTool) Name() string { return "forgejo_create_pr" }

func (t *forgejoCreatePRTool) Description() string {
	return "Create a pull request from a head branch to a base branch. Use for submitting code changes for review."
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

	url := fmt.Sprintf("%s/api/v1/repos/%s/pulls", t.baseURL, params.Repository)
	payload := map[string]string{
		"title": params.Title,
		"body":  params.Body,
		"head":  params.Head,
		"base":  params.Base,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Sprintf("Pull request created: %s", string(respBody)), nil
}

// forgejoSearchCodeTool searches code in a repository.
type forgejoSearchCodeTool struct {
	client  *http.Client
	baseURL string
	token   string
}

func NewSearchCodeTool(adapter *ForgejoAdapter) *forgejoSearchCodeTool {
	return &forgejoSearchCodeTool{
		client:  adapter.Client,
		baseURL: adapter.BaseURL,
		token:   adapter.Token,
	}
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

	url := fmt.Sprintf("%s/api/v1/repos/%s/code/search?q=%s",
		t.baseURL, params.Repository, params.Query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return string(respBody), nil
}

// forgejoAddReactionTool adds an emoji reaction.
type forgejoAddReactionTool struct {
	client  *http.Client
	baseURL string
	token   string
}

func NewAddReactionTool(adapter *ForgejoAdapter) *forgejoAddReactionTool {
	return &forgejoAddReactionTool{
		client:  adapter.Client,
		baseURL: adapter.BaseURL,
		token:   adapter.Token,
	}
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

	var url string
	if params.CommentID > 0 {
		url = fmt.Sprintf("%s/api/v1/repos/%s/issues/comments/%d/reactions",
			t.baseURL, params.Repository, params.CommentID)
	} else {
		url = fmt.Sprintf("%s/api/v1/repos/%s/issues/%d/reactions",
			t.baseURL, params.Repository, params.IssueNumber)
	}

	payload := map[string]string{"content": params.Reaction}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Sprintf("Reaction '%s' added", params.Reaction), nil
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
