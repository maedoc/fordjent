// Package scheduler manages issue dependencies and label transitions.
// It parses "Depends on: #N" declarations from issue bodies and manages
// label states (blocked → ready → in_progress) as PRs are merged.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/fordjent/fordjent/internal/tool"
)

var dependsOnRegex = regexp.MustCompile(`(?i)#(\d+)`)
var dependsOnKeywordRegex = regexp.MustCompile(`(?i)depends\s+on`)

// Scheduler wraps a Forgejo client and provides dependency management.
type Scheduler struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New creates a Scheduler from a ForgejoAdapter.
func New(adapter *tool.ForgejoAdapter) *Scheduler {
	return &Scheduler{
		BaseURL: adapter.BaseURL,
		Token:   adapter.Token,
		HTTP:    adapter.Client,
	}
}

// Issue mirrors the minimal Forgejo issue representation.
type Issue struct {
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Labels  []Label  `json:"labels"`
	State   string   `json:"state"`
}

// Label is a Forgejo label.
type Label struct {
	Name string `json:"name"`
}

// OnPRMerged is called whenever a PR is merged. It scans open issues for
// any that declared a dependency on the merged PR number and removes their
// 'blocked' label, adding a 'ready' label instead. It also posts a comment.
func (s *Scheduler) OnPRMerged(ctx context.Context, repo string, mergedPRNumber int) error {
	// 1. List all open issues in the repo
	issues, err := s.listOpenIssues(ctx, repo)
	if err != nil {
		return fmt.Errorf("list open issues: %w", err)
	}

	mergedDep := fmt.Sprintf("#%d", mergedPRNumber)
	var unblocked []int

	for _, issue := range issues {
		deps := parseDependsOn(issue.Body)
		if len(deps) == 0 {
			continue
		}

		// Check if this issue depended on the merged PR
		dependsOnMerged := false
		for _, d := range deps {
			if d == mergedPRNumber {
				dependsOnMerged = true
				break
			}
		}
		if !dependsOnMerged {
			continue
		}

		// Check if ALL declared dependencies are satisfied.
		// For now, a "satisfied" dependency means the PR is merged.
		// In a future iteration we could track PR state in a local table.
		allSatisfied := true
		for _, depNum := range deps {
			isMerged, err := s.isPRMerged(ctx, repo, depNum)
			if err != nil {
				slog.Warn("scheduler: failed to check PR state, assuming not merged", "error", err, "pr", depNum)
				allSatisfied = false
				break
			}
			if !isMerged {
				allSatisfied = false
				break
			}
		}
		if !allSatisfied {
			continue
		}

		// Remove 'blocked' label if present
		if s.hasLabel(issue.Labels, "blocked") {
			if err := s.removeLabel(ctx, repo, issue.Number, "blocked"); err != nil {
				slog.Warn("scheduler: failed to remove blocked label", "error", err, "issue", issue.Number)
			}
		}

		// Add 'ready' label if not present
		if !s.hasLabel(issue.Labels, "ready") {
			if err := s.addLabel(ctx, repo, issue.Number, "ready"); err != nil {
				slog.Warn("scheduler: failed to add ready label", "error", err, "issue", issue.Number)
			}
		}

		// Post a comment
		comment := fmt.Sprintf("Dependency %s is now merged. This issue is unblocked and ready to work on!", mergedDep)
		if err := s.postComment(ctx, repo, issue.Number, comment); err != nil {
			slog.Warn("scheduler: failed to post unblock comment", "error", err, "issue", issue.Number)
		}

		unblocked = append(unblocked, issue.Number)
	}

	if len(unblocked) > 0 {
		slog.Info("scheduler: unblocked issues after PR merge", "repo", repo, "merged_pr", mergedPRNumber, "issues", unblocked)
	}
	return nil
}

// hasLabel checks if a label exists in a list.
func (s *Scheduler) hasLabel(labels []Label, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// parseDependsOn extracts PR/issue numbers from a body string.
func parseDependsOn(body string) []int {
	if !dependsOnKeywordRegex.MatchString(body) {
		return nil
	}
	matches := dependsOnRegex.FindAllStringSubmatch(body, -1)
	var nums []int
	for _, m := range matches {
		if len(m) >= 2 {
			n, err := strconv.Atoi(m[1])
			if err == nil {
				nums = append(nums, n)
			}
		}
	}
	return nums
}

// listOpenIssues returns all open issues in a repo.
func (s *Scheduler) listOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/issues?state=open", escaped)
	body, err := s.doGet(ctx, apiPath)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(body), &issues); err != nil {
		return nil, fmt.Errorf("unmarshal issues: %w", err)
	}
	return issues, nil
}

// isPRMerged checks whether a PR is merged by querying its state.
func (s *Scheduler) isPRMerged(ctx context.Context, repo string, number int) (bool, error) {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/pulls/%d", escaped, number)
	body, err := s.doGet(ctx, apiPath)
	if err != nil {
		return false, err
	}
	var pr struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		return false, fmt.Errorf("unmarshal PR: %w", err)
	}
	return pr.Merged, nil
}

// removeLabel removes a label from an issue.
func (s *Scheduler) removeLabel(ctx context.Context, repo string, issueNum int, label string) error {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/issues/%d/labels/%s", escaped, issueNum, url.PathEscape(label))
	return s.doDelete(ctx, apiPath)
}

// addLabel adds a label to an issue.
func (s *Scheduler) addLabel(ctx context.Context, repo string, issueNum int, label string) error {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/issues/%d/labels", escaped, issueNum)
	return s.doPost(ctx, apiPath, []string{label})
}

// postComment posts a comment on an issue or pull request.
func (s *Scheduler) postComment(ctx context.Context, repo string, issueNum int, body string) error {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/issues/%d/comments", escaped, issueNum)
	return s.doPost(ctx, apiPath, map[string]string{"body": body})
}

// doGet performs an authenticated GET request.
func (s *Scheduler) doGet(ctx context.Context, apiPath string) (string, error) {
	fullURL := s.BaseURL + apiPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+s.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	return string(data), nil
}

// doPost performs an authenticated POST request.
func (s *Scheduler) doPost(ctx context.Context, apiPath string, body interface{}) error {
	fullURL := s.BaseURL + apiPath
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+s.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// doDelete performs an authenticated DELETE request.
func (s *Scheduler) doDelete(ctx context.Context, apiPath string) error {
	fullURL := s.BaseURL + apiPath
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fullURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+s.Token)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	return nil
}

func escapeRepoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
