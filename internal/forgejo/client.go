package forgejo

import (
	"bytes"
	"context"
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
	baseURL string
	token   string
	client  *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
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
	req.Header.Set("Authorization", "token "+c.token)

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
