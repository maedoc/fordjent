package forgejo

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

// GetIssue retrieves an issue by number.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	url := fmt.Sprintf("%s/api/v1/repos/%s/issues/%d", c.baseURL, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var issue Issue
	if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &issue, nil
}

// ListComments lists comments on an issue.
func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	url := fmt.Sprintf("%s/api/v1/repos/%s/issues/%d/comments", c.baseURL, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var rawComments []struct {
		ID   int    `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawComments); err != nil {
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
	var url string
	if commentID > 0 {
		url = fmt.Sprintf("%s/api/v1/repos/%s/issues/comments/%d/reactions", c.baseURL, repo, commentID)
	} else {
		url = fmt.Sprintf("%s/api/v1/repos/%s/issues/%d/reactions", c.baseURL, repo, issueNumber)
	}

	payload := map[string]string{"content": emoji}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
