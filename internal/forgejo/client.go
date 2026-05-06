package forgejo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Client is a Forgejo API client.
type Client struct {
	baseURL   string
	token     string
	user      string // Basic auth username
	password  string // Basic auth password
	client    *http.Client
}

// NewClient creates a client with token authentication.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientWithBasicAuth creates a client with basic authentication.
func NewClientWithBasicAuth(baseURL, user, password string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		user:     user,
		password: password,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Issue represents a Forgejo issue.
type Issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	State  string  `json:"state"`
	User   User    `json:"user"`
	Labels []Label `json:"labels"`
}

// Comment represents a Forgejo issue/PR comment.
type Comment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
	User string `json:"user"`
}

// User represents a Forgejo user.
type User struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
}

func (u User) String() string { return u.Login }

// PullRequest represents a Forgejo pull request with branch info.
type PullRequest struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	State        string `json:"state"`
	Mergeable    bool   `json:"mergeable"`
	Merged       bool   `json:"merged"` // Forgejo sends merged=true even when state=closed
	HasConflicts bool   `json:"has_conflits"` // NOTE: Forgejo API field may vary — treat as advisory
	Head         struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// Label represents a Forgejo issue/PR label.
type Label struct {
	Name string `json:"name"`
}

// RepoFile represents a file in a repository tree.
type RepoFile struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// GetPR retrieves a pull request by number, including head branch info.
func (c *Client) GetPR(ctx context.Context, repo string, number int) (*PullRequest, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "pulls", fmt.Sprintf("%d", number))
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var pr PullRequest
	if err := json.Unmarshal([]byte(result), &pr); err != nil {
		return nil, fmt.Errorf("decode PR: %w", err)
	}
	return &pr, nil
}

// MergePR merges a pull request using the given merge style ("merge", "rebase-merge", etc.).
func (c *Client) MergePR(ctx context.Context, repo string, number int, style string) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "pulls", fmt.Sprintf("%d", number), "merge")
	_, err := c.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"Do": style})
	return err
}

// doRequest is a shared helper for Forgejo API calls.
func (c *Client) doRequest(ctx context.Context, method, apiPath string, body interface{}) (string, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	fullURL := c.baseURL + apiPath
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	// Auth: token takes precedence, then basic auth
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	} else if c.user != "" && c.password != "" {
		req.SetBasicAuth(c.user, c.password)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}
	return string(respBody), nil
}

// escapeRepoPath escapes each segment of an "owner/repo" path while preserving
// the slash separator. Using url.PathEscape on the whole string encodes the slash,
// breaking Gitea/Forgejo's two-segment path routing.
func escapeRepoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return path.Join(parts...)
}

// GetIssue retrieves an issue by number.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues", fmt.Sprintf("%d", number))
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}

	var issue Issue
	if err := json.Unmarshal([]byte(result), &issue); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &issue, nil
}

// ListComments lists comments on an issue.
func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues", fmt.Sprintf("%d", number), "comments")
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}

	var rawComments []struct {
		ID   int    `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(result), &rawComments); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	comments := make([]Comment, len(rawComments))
	for i, rc := range rawComments {
		comments[i] = Comment{
			ID:   rc.ID,
			Body: rc.Body,
			User: rc.User.Login,
		}
	}
	return comments, nil
}

// AddReaction adds an emoji reaction to an issue/PR or comment.
func (c *Client) AddReaction(ctx context.Context, repo string, issueNumber, commentID int, emoji string) error {
	var apiPath string
	if commentID > 0 {
		apiPath = path.Join("/api/v1/repos", escapeRepoPath(repo),
			"issues", "comments", fmt.Sprintf("%d", commentID), "reactions")
	} else {
		apiPath = path.Join("/api/v1/repos", escapeRepoPath(repo),
			"issues", fmt.Sprintf("%d", issueNumber), "reactions")
	}

	_, err := c.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"content": emoji})
	return err
}

// AddIssueLabels adds labels to an issue.
func (c *Client) AddIssueLabels(ctx context.Context, repo string, issueNumber int, labels []string) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues", fmt.Sprintf("%d", issueNumber), "labels")
	_, err := c.doRequest(ctx, http.MethodPost, apiPath, labels)
	return err
}

// RemoveIssueLabel removes a single label from an issue.
func (c *Client) RemoveIssueLabel(ctx context.Context, repo string, issueNumber int, label string) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues", fmt.Sprintf("%d", issueNumber), "labels", url.PathEscape(label))
	_, err := c.doRequest(ctx, http.MethodDelete, apiPath, nil)
	return err
}

// CreateIssue creates a new issue in a repository.
func (c *Client) CreateIssue(ctx context.Context, repo, title, body string) (*Issue, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues")
	result, err := c.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"title": title, "body": body})
	if err != nil {
		return nil, err
	}
	var issue Issue
	if err := json.Unmarshal([]byte(result), &issue); err != nil {
		return nil, fmt.Errorf("decode created issue: %w", err)
	}
	return &issue, nil
}

// ListOpenIssues returns all open issues in a repository.
func (c *Client) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues?state=open")
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(result), &issues); err != nil {
		return nil, fmt.Errorf("decode issues: %w", err)
	}
	return issues, nil
}

// ListRepoFiles returns file paths in a repository tree (shallow, first page).
func (c *Client) ListRepoFiles(ctx context.Context, repo, ref string) ([]string, error) {
	if ref == "" {
		ref = "main"
	}
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "git/trees", ref)
	result, err := c.doRequest(ctx, http.MethodGet, apiPath+"?recursive=1", nil)
	if err != nil {
		return nil, err
	}
	var tree struct {
		Tree []RepoFile `json:"tree"`
	}
	if err := json.Unmarshal([]byte(result), &tree); err != nil {
		return nil, fmt.Errorf("decode tree: %w", err)
	}
	var files []string
	for _, f := range tree.Tree {
		if f.Type == "blob" {
			files = append(files, f.Path)
		}
	}
	return files, nil
}

// PostIssueComment adds a comment to an issue or pull request.
func (c *Client) PostIssueComment(ctx context.Context, repo string, issueNumber int, body string) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues", fmt.Sprintf("%d", issueNumber), "comments")
	_, err := c.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"body": body})
	return err
}

// AddCollaborator adds a user as a collaborator to a repository with the given permission (read, write, admin).
func (c *Client) AddCollaborator(ctx context.Context, repo, username, permission string) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "collaborators", url.PathEscape(username))
	_, err := c.doRequest(ctx, http.MethodPut, apiPath, map[string]string{"permission": permission})
	return err
}

// CreateLabel creates a new label in a repository.
func (c *Client) CreateLabel(ctx context.Context, repo, name, color string) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "labels")
	_, err := c.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"name": name, "color": color})
	return err
}

// EnsureLabels creates labels that Fordjent scheduler/lifecycle/scaffold depend on.
func (c *Client) EnsureLabels(ctx context.Context, repo string) error {
	labels := []struct {
		Name  string
		Color string
	}{
		{"blocked", "d93f0b"},
		{"ready", "0e8a16"},
		{"scaffold", "fbca04"},
		{"fordjent/failed:max-turns", "b60205"},
		{"fordjent/failed:error", "5319e7"},
	}
	for _, l := range labels {
		if err := c.CreateLabel(ctx, repo, l.Name, l.Color); err != nil {
			// Ignore conflict (label already exists)
			if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "already exists") {
				continue
			}
			return fmt.Errorf("create label %q: %w", l.Name, err)
		}
	}
	return nil
}

// === REPOSITORY ===

// Repository represents a Forgejo repository.
type Repository struct {
	ID          int    `json:"id"`
	Name       string `json:"name"`
	FullName   string `json:"full_name"`
	Description string `json:"description"`
	Private    bool   `json:"private"`
	HTMLURL    string `json:"html_url"`
	CloneURL   string `json:"clone_url"`
}

// GetRepository retrieves a repository by owner/repo.
func (c *Client) GetRepository(ctx context.Context, repo string) (*Repository, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo))
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var r Repository
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, fmt.Errorf("decode repository: %w", err)
	}
	return &r, nil
}

// ListUserRepos lists repositories for the authenticated user.
func (c *Client) ListUserRepos(ctx context.Context) ([]Repository, error) {
	result, err := c.doRequest(ctx, http.MethodGet, "/api/v1/user/repos", nil)
	if err != nil {
		return nil, err
	}
	var repos []Repository
	if err := json.Unmarshal([]byte(result), &repos); err != nil {
		return nil, fmt.Errorf("decode repos: %w", err)
	}
	return repos, nil
}

// CreateRepository creates a new repository.
func (c *Client) CreateRepository(ctx context.Context, name, description string, private bool) (*Repository, error) {
	payload := map[string]interface{}{
		"name":        name,
		"description": description,
		"private":     private,
	}
	result, err := c.doRequest(ctx, http.MethodPost, "/api/v1/user/repos", payload)
	if err != nil {
		return nil, err
	}
	var r Repository
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, fmt.Errorf("decode repository: %w", err)
	}
	return &r, nil
}

// === ISSUES (Extended) ===

// ListIssues lists issues in a repository with optional state filter.
func (c *Client) ListIssues(ctx context.Context, repo, state string, limit int) ([]Issue, error) {
	if state == "" {
		state = "open"
	}
	if limit == 0 {
		limit = 20
	}
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues")
	query := url.Values{}
	query.Set("state", state)
	query.Set("limit", fmt.Sprintf("%d", limit))
	result, err := c.doRequest(ctx, http.MethodGet, apiPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(result), &issues); err != nil {
		return nil, fmt.Errorf("decode issues: %w", err)
	}
	return issues, nil
}

// CloseIssue closes an issue.
func (c *Client) CloseIssue(ctx context.Context, repo string, number int) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues", fmt.Sprintf("%d", number))
	_, err := c.doRequest(ctx, http.MethodPatch, apiPath, map[string]string{"state": "closed"})
	return err
}

// ReopenIssue reopens a closed issue.
func (c *Client) ReopenIssue(ctx context.Context, repo string, number int) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "issues", fmt.Sprintf("%d", number))
	_, err := c.doRequest(ctx, http.MethodPatch, apiPath, map[string]string{"state": "open"})
	return err
}

// === PULL REQUESTS (Extended) ===

// PRFile represents a file changed in a pull request.
type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"` // added, modified, removed, renamed
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// ListPRs lists pull requests in a repository.
func (c *Client) ListPRs(ctx context.Context, repo, state string) ([]PullRequest, error) {
	if state == "" {
		state = "open"
	}
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "pulls")
	query := url.Values{}
	query.Set("state", state)
	result, err := c.doRequest(ctx, http.MethodGet, apiPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var prs []PullRequest
	if err := json.Unmarshal([]byte(result), &prs); err != nil {
		return nil, fmt.Errorf("decode PRs: %w", err)
	}
	return prs, nil
}

// GetPRFiles lists files changed in a pull request.
func (c *Client) GetPRFiles(ctx context.Context, repo string, number int) ([]PRFile, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "pulls", fmt.Sprintf("%d", number), "files")
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var files []PRFile
	if err := json.Unmarshal([]byte(result), &files); err != nil {
		return nil, fmt.Errorf("decode PR files: %w", err)
	}
	return files, nil
}

// CreatePR creates a pull request.
func (c *Client) CreatePR(ctx context.Context, repo, title, body, head, base string) (*PullRequest, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "pulls")
	payload := map[string]string{
		"title": title,
		"head":  head,
		"base":  base,
	}
	if body != "" {
		payload["body"] = body
	}
	result, err := c.doRequest(ctx, http.MethodPost, apiPath, payload)
	if err != nil {
		return nil, err
	}
	var pr PullRequest
	if err := json.Unmarshal([]byte(result), &pr); err != nil {
		return nil, fmt.Errorf("decode PR: %w", err)
	}
	return &pr, nil
}

// ClosePR closes a pull request without merging.
func (c *Client) ClosePR(ctx context.Context, repo string, number int) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "pulls", fmt.Sprintf("%d", number))
	_, err := c.doRequest(ctx, http.MethodPatch, apiPath, map[string]string{"state": "closed"})
	return err
}

// === BRANCHES ===

// Branch represents a git branch.
type Branch struct {
	Name      string `json:"name"`
	CommitID  string `json:"id"` // full SHA
	Protected bool   `json:"protected"`
}

// ListBranches lists branches in a repository.
func (c *Client) ListBranches(ctx context.Context, repo string) ([]Branch, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "branches")
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	// Raw response has commit.id nested
	var rawBranches []struct {
		Name      string `json:"name"`
		Commit    struct {
			ID string `json:"id"`
		} `json:"commit"`
		Protected bool `json:"protected"`
	}
	if err := json.Unmarshal([]byte(result), &rawBranches); err != nil {
		return nil, fmt.Errorf("decode branches: %w", err)
	}
	branches := make([]Branch, len(rawBranches))
	for i, rb := range rawBranches {
		branches[i] = Branch{
			Name:      rb.Name,
			CommitID:  rb.Commit.ID,
			Protected: rb.Protected,
		}
	}
	return branches, nil
}

// DeleteBranch deletes a branch from a repository.
func (c *Client) DeleteBranch(ctx context.Context, repo, branch string) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "branches", url.PathEscape(branch))
	_, err := c.doRequest(ctx, http.MethodDelete, apiPath, nil)
	return err
}

// === WEBHOOKS ===

// Webhook represents a repository webhook.
type Webhook struct {
	ID     int               `json:"id"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
	Active bool              `json:"active"`
	Events []string          `json:"events"`
}

// ListWebhooks lists webhooks for a repository.
func (c *Client) ListWebhooks(ctx context.Context, repo string) ([]Webhook, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "hooks")
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var hooks []Webhook
	if err := json.Unmarshal([]byte(result), &hooks); err != nil {
		return nil, fmt.Errorf("decode webhooks: %w", err)
	}
	return hooks, nil
}

// CreateWebhook creates a webhook for a repository.
func (c *Client) CreateWebhook(ctx context.Context, repo, hookType, hookURL, secret string, events []string) (*Webhook, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "hooks")
	payload := map[string]interface{}{
		"type": hookType,
		"config": map[string]string{
			"url":          hookURL,
			"content_type": "json",
		},
		"events": events,
		"active": true,
	}
	if secret != "" {
		payload["config"].(map[string]string)["secret"] = secret
	}
	result, err := c.doRequest(ctx, http.MethodPost, apiPath, payload)
	if err != nil {
		return nil, err
	}
	var hook Webhook
	if err := json.Unmarshal([]byte(result), &hook); err != nil {
		return nil, fmt.Errorf("decode webhook: %w", err)
	}
	return &hook, nil
}

// DeleteWebhook deletes a webhook.
func (c *Client) DeleteWebhook(ctx context.Context, repo string, id int) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "hooks", fmt.Sprintf("%d", id))
	_, err := c.doRequest(ctx, http.MethodDelete, apiPath, nil)
	return err
}

// === LABELS ===

// ListLabels lists labels in a repository.
func (c *Client) ListLabels(ctx context.Context, repo string) ([]Label, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "labels")
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var rawLabels []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.Unmarshal([]byte(result), &rawLabels); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	labels := make([]Label, len(rawLabels))
	for i, rl := range rawLabels {
		labels[i] = Label{Name: rl.Name}
	}
	return labels, nil
}

// === FILES ===

// FileContent represents a file in the repository contents API.
type FileContent struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`     // "file" or "dir"
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	SHA      string `json:"sha"`
}

// ListDir lists files in a repository directory.
func (c *Client) ListDir(ctx context.Context, repo, ref, dirPath string) ([]FileContent, error) {
	if ref == "" {
		ref = "main"
	}
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "contents", dirPath)
	query := url.Values{}
	query.Set("ref", ref)
	result, err := c.doRequest(ctx, http.MethodGet, apiPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var files []FileContent
	if err := json.Unmarshal([]byte(result), &files); err != nil {
		// Could be a single file
		var single FileContent
		if err2 := json.Unmarshal([]byte(result), &single); err2 == nil {
			return []FileContent{single}, nil
		}
		return nil, fmt.Errorf("decode file list: %w", err)
	}
	return files, nil
}

// GetFile retrieves file contents.
func (c *Client) GetFile(ctx context.Context, repo, ref, filePath string) (*FileContent, error) {
	if ref == "" {
		ref = "main"
	}
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "contents", filePath)
	query := url.Values{}
	query.Set("ref", ref)
	result, err := c.doRequest(ctx, http.MethodGet, apiPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var file FileContent
	if err := json.Unmarshal([]byte(result), &file); err != nil {
		return nil, fmt.Errorf("decode file: %w", err)
	}
	return &file, nil
}

// CreateOrUpdateFile creates or updates a file in the repository.
func (c *Client) CreateOrUpdateFile(ctx context.Context, repo, filePath, message, content string, sha string) error {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "contents", filePath)
	payload := map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if sha != "" {
		payload["sha"] = sha
	}
	_, err := c.doRequest(ctx, http.MethodPost, apiPath, payload)
	return err
}

// === COLLABORATORS ===

// Collaborator represents a repository collaborator.
type Collaborator struct {
	Login      string `json:"login"`
	Permission string `json:"permission"`
}

// ListCollaborators lists collaborators for a repository.
func (c *Client) ListCollaborators(ctx context.Context, repo string) ([]Collaborator, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "collaborators")
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var collabs []Collaborator
	if err := json.Unmarshal([]byte(result), &collabs); err != nil {
		return nil, fmt.Errorf("decode collaborators: %w", err)
	}
	return collabs, nil
}

// === CODE SEARCH ===

// SearchResult represents a code search result.
type SearchResult struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	Line     int    `json:"line"`
}

// SearchCode searches for code in a repository.
func (c *Client) SearchCode(ctx context.Context, repo, query string) ([]SearchResult, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "code", "search")
	q := url.Values{}
	q.Set("q", query)
	result, err := c.doRequest(ctx, http.MethodGet, apiPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data []SearchResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(result), &raw); err != nil {
		return nil, fmt.Errorf("decode search: %w", err)
	}
	return raw.Data, nil
}

// === USER & VERSION ===

// GetCurrentUser returns the authenticated user.
func (c *Client) GetCurrentUser(ctx context.Context) (*User, error) {
	result, err := c.doRequest(ctx, http.MethodGet, "/api/v1/user", nil)
	if err != nil {
		return nil, err
	}
	var user User
	if err := json.Unmarshal([]byte(result), &user); err != nil {
		return nil, fmt.Errorf("decode user: %w", err)
	}
	return &user, nil
}

// Version represents Forgejo version info.
type Version struct {
	Version string `json:"version"`
}

// GetVersion returns the Forgejo server version.
func (c *Client) GetVersion(ctx context.Context) (*Version, error) {
	result, err := c.doRequest(ctx, http.MethodGet, "/api/v1/version", nil)
	if err != nil {
		return nil, err
	}
	var v Version
	if err := json.Unmarshal([]byte(result), &v); err != nil {
		return nil, fmt.Errorf("decode version: %w", err)
	}
	return &v, nil
}

// Token represents an access token.
type Token struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// CreateToken creates a new access token for a user.
func (c *Client) CreateToken(ctx context.Context, username, tokenName string) (*Token, error) {
	apiPath := path.Join("/api/v1/users", url.PathEscape(username), "tokens")
	result, err := c.doRequest(ctx, http.MethodPost, apiPath, map[string]string{"name": tokenName})
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal([]byte(result), &t); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	return &t, nil
}

// RawRequest executes a raw API request and returns the response body.
func (c *Client) RawRequest(ctx context.Context, method, apiPath string, body interface{}) (string, error) {
	return c.doRequest(ctx, method, apiPath, body)
}

// Review represents a pull request review.
type Review struct {
	ID     int    `json:"id"`
	User   *User  `json:"user"`
	State  string `json:"state"` // "APPROVED", "REQUEST_CHANGES", etc.
	Body   string `json:"body"`
}

// ListPRReviews returns reviews for a pull request.
func (c *Client) ListPRReviews(ctx context.Context, repo string, number int) ([]Review, error) {
	apiPath := path.Join("/api/v1/repos", escapeRepoPath(repo), "pulls", fmt.Sprintf("%d", number), "reviews")
	result, err := c.doRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var reviews []Review
	if err := json.Unmarshal([]byte(result), &reviews); err != nil {
		return nil, fmt.Errorf("unmarshal reviews: %w", err)
	}
	return reviews, nil
}
