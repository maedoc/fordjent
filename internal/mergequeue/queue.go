// Package mergequeue implements a file-gate merge queue for Fordjent.
// Before a PR is created, it checks whether any currently open PR touches
// the same files. If so, creation is blocked until the conflicting PR merges.
package mergequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/fordjent/fordjent/internal/tool"
)

// Client wraps a Forgejo HTTP client and token so the merge queue can
// inspect open PRs independently of any particular Agent session.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient creates a merge-queue client from a ForgejoAdapter.
func NewClient(adapter *tool.ForgejoAdapter) *Client {
	return &Client{
		BaseURL: adapter.BaseURL(),
		Token:   adapter.Token(),
		HTTP:    adapter.HTTPClient(),
	}
}

// ErrBlocked is returned when a PR creation is blocked by the merge queue.
var ErrBlocked = errors.New("blocked by merge queue")

// ChangedFile mirrors the minimal file entry returned by the Forgejo
// /pulls/{index}/files endpoint.
type ChangedFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
}

// PullRequest mirrors the minimal PR representation from Forgejo.
type PullRequest struct {
	Number int    `json:"number"`
	Head   Branch `json:"head"`
	Base   Branch `json:"base"`
	State  string `json:"state"`
}

// Branch holds ref/sha info for a PR head or base.
type Branch struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

// CheckGate inspects all open PRs in a repo and compares their changed
// files with the files that would be changed by the proposed branch.
// If there is overlap, it returns blocked=true and a message explaining why.
func (c *Client) CheckGate(ctx context.Context, repo, headBranch, baseBranch string) (bool, string, error) {
	// 1. Get the list of files this branch would change vs base
	ourFiles, err := c.compareBranchFiles(ctx, repo, baseBranch, headBranch)
	if err != nil {
		// If we can't diff (e.g. branch doesn't exist yet), don't block
		slog.Warn("mergequeue: failed to diff branch, allowing through", "error", err, "branch", headBranch)
		return false, "", nil
	}
	if len(ourFiles) == 0 {
		return false, "", nil
	}

	// 2. List open PRs
	openPRs, err := c.listOpenPRs(ctx, repo)
	if err != nil {
		slog.Warn("mergequeue: failed to list open PRs, allowing through", "error", err)
		return false, "", nil
	}

	// 3. For each open PR, get its changed files and compare
	var conflicts []int
	var conflictFiles []string
	fileSet := make(map[string]struct{}, len(ourFiles))
	for _, f := range ourFiles {
		fileSet[f] = struct{}{}
	}

	for _, pr := range openPRs {
		if pr.Head.Ref == headBranch {
			// Skip self — this branch already has a PR open
			continue
		}
		prFiles, err := c.listPRFiles(ctx, repo, pr.Number)
		if err != nil {
			continue
		}
		for _, f := range prFiles {
			if _, ok := fileSet[f]; ok {
				conflicts = append(conflicts, pr.Number)
				conflictFiles = appendUniq(conflictFiles, f)
			}
		}
	}

	if len(conflicts) > 0 {
		msg := fmt.Sprintf("Existing open PR(s) #%v touch the same file(s): %v. Wait for them to merge first, then rebase and try again.", conflicts, conflictFiles)
		return true, msg, nil
	}

	return false, "", nil
}

// compareBranchFiles returns the files that differ between baseBranch
// and headBranch using the Forgejo compare API.
func (c *Client) compareBranchFiles(ctx context.Context, repo, base, head string) ([]string, error) {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/compare/%s...%s", escaped, base, head)
	body, err := c.doGet(ctx, apiPath)
	if err != nil {
		return nil, err
	}

	var result struct {
		Commits []struct {
			Files []struct {
				Filename string `json:"filename"`
			} `json:"files"`
		} `json:"commits"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		return nil, fmt.Errorf("unmarshal compare: %w", err)
	}

	fileSet := make(map[string]struct{})
	for _, commit := range result.Commits {
		for _, f := range commit.Files {
			fileSet[f.Filename] = struct{}{}
		}
	}
	files := make([]string, 0, len(fileSet))
	for name := range fileSet {
		files = append(files, name)
	}
	return files, nil
}

// listOpenPRs returns all currently open PRs for a repo.
func (c *Client) listOpenPRs(ctx context.Context, repo string) ([]PullRequest, error) {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/pulls?state=open", escaped)
	body, err := c.doGet(ctx, apiPath)
	if err != nil {
		return nil, err
	}

	var prs []PullRequest
	if err := json.Unmarshal([]byte(body), &prs); err != nil {
		return nil, fmt.Errorf("unmarshal PRs: %w", err)
	}
	return prs, nil
}

// listPRFiles returns the list of files changed in a PR.
func (c *Client) listPRFiles(ctx context.Context, repo string, number int) ([]string, error) {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/pulls/%d/files", escaped, number)
	body, err := c.doGet(ctx, apiPath)
	if err != nil {
		return nil, err
	}

	var files []ChangedFile
	if err := json.Unmarshal([]byte(body), &files); err != nil {
		return nil, fmt.Errorf("unmarshal PR files: %w", err)
	}

	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Filename)
	}
	return out, nil
}

func (c *Client) doGet(ctx context.Context, apiPath string) (string, error) {
	fullURL := c.BaseURL + apiPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := readAllLimit(resp.Body, 10<<20)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	return string(data), nil
}

func escapeRepoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func appendUniq(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

func readAllLimit(body io.Reader, limit int) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, int64(limit)))
}
