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
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	User   User   `json:"user"`
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

// GetIssue retrieves an issue by number.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	apiPath := path.Join("/api/v1/repos", url.PathEscape(repo), "issues", fmt.Sprintf("%d", number))
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
	apiPath := path.Join("/api/v1/repos", url.PathEscape(repo), "issues", fmt.Sprintf("%d", number), "comments")
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
		apiPath = path.Join("/api/v1/repos", url.PathEscape(repo),
			"issues", "comments", fmt.Sprintf("%d", commentID), "reactions")
	} else {
		apiPath = path.Join("/api/v1/repos", url.PathEscape(repo),
			"issues", fmt.Sprintf("%d", issueNumber), "reactions")
	}

	_, err := c.doRequest(ctx, http.MethodPut, apiPath, map[string]string{"content": emoji})
	return err
}
