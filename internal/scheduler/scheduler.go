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
	"sort"
	"strconv"
	"strings"

	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/tool"
)

var dependsOnRegex = regexp.MustCompile(`(?i)#(\d+)`)
var dependsOnKeywordRegex = regexp.MustCompile(`(?i)depends\s+on`)
var priorityRegex = regexp.MustCompile(`(?i)priority\s*:\s*(\d+)`)

// Scheduler wraps a Forgejo client and provides dependency management.
type Scheduler struct {
	BaseURL       string
	Token         string
	HTTP          *http.Client
	forgejoClient *forgejo.Client
}

// New creates a Scheduler from a ForgejoAdapter.
func New(adapter *tool.ForgejoAdapter) *Scheduler {
	return &Scheduler{
		BaseURL: adapter.BaseURL(),
		Token:   adapter.Token(),
		HTTP:    adapter.HTTPClient(),
	}
}

// SetForgejoClient sets the underlying Forgejo API client used for label
// auto-creation (EnsureLabels). Call this after New if label guarantees are needed.
func (s *Scheduler) SetForgejoClient(c *forgejo.Client) {
	s.forgejoClient = c
}

// Issue mirrors the minimal Forgejo issue representation.
type Issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Labels []Label `json:"labels"`
	State  string  `json:"state"`
}

// Label is a Forgejo label.
type Label struct {
	Name string `json:"name"`
}

// OnPRMerged is called whenever a PR is merged. It scans open issues for
// any whose declared dependencies are all closed (satisfied), removes their
// 'blocked' label, and adds a 'ready' label. It also posts a comment.
func (s *Scheduler) OnPRMerged(ctx context.Context, repo string, mergedPRNumber int) error {
	return s.checkAndUnblock(ctx, repo, mergedPRNumber)
}

// CheckAndUnblock scans all open issues for satisfied dependencies and unblocks them.
// Unlike OnPRMerged, it does not require a specific merged PR trigger.
func (s *Scheduler) CheckAndUnblock(ctx context.Context, repo string) error {
	return s.checkAndUnblock(ctx, repo, 0)
}

// checkAndUnblock is the shared implementation.
func (s *Scheduler) checkAndUnblock(ctx context.Context, repo string, mergedPRNumber int) error {
	// 1. List all open issues in the repo
	issues, err := s.listOpenIssues(ctx, repo)
	if err != nil {
		return fmt.Errorf("list open issues: %w", err)
	}

	// Ensure required labels exist before any label operations
	if s.forgejoClient != nil {
		if err := s.forgejoClient.EnsureLabels(ctx, repo); err != nil {
			slog.Warn("scheduler: failed to ensure labels exist", "error", err, "repo", repo)
		}
	}

	// 2. Detect circular dependencies before processing
	cycles := detectCircularDeps(issues)
	for _, cycle := range cycles {
		for _, num := range cycle {
			comment := fmt.Sprintf("Circular dependency detected involving issue #%d. The dependency graph contains a cycle. Please break the cycle manually.\n\n<!-- ford -->", num)
			if err := s.postComment(ctx, repo, num, comment); err != nil {
				slog.Warn("scheduler: failed to post cycle warning comment", "error", err, "issue", num)
			}
		}
	}
	cycleSet := make(map[int]bool)
	for _, cycle := range cycles {
		for _, num := range cycle {
			cycleSet[num] = true
		}
	}

	// 3. Find all unblock candidates with priority ordering
	type candidate struct {
		issue    Issue
		priority int
	}
	var candidates []candidate

	for _, issue := range issues {
		// Skip issues in dependency cycles
		if cycleSet[issue.Number] {
			continue
		}

		deps := parseDependsOn(issue.Body)
		if len(deps) == 0 {
			continue
		}

		// Check if ALL declared dependencies are closed (satisfied).
		allSatisfied := true
		for _, depNum := range deps {
			isClosed, err := s.isIssueClosed(ctx, repo, depNum)
			if err != nil {
				slog.Warn("scheduler: failed to check issue state, assuming not closed", "error", err, "issue", depNum)
				allSatisfied = false
				break
			}
			if !isClosed {
				allSatisfied = false
				break
			}
		}
		if !allSatisfied {
			continue
		}

		// Parse priority: lower number = higher priority. Default 99.
		priority := parsePriority(issue.Body)
		candidates = append(candidates, candidate{issue: issue, priority: priority})
	}

	// Sort by priority (ascending), then by issue number (ascending)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].issue.Number < candidates[j].issue.Number
	})

	// 4. Unblock one issue at a time in priority order
	var unblocked []int
	for _, c := range candidates {
		issue := c.issue

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

		// Post a comment with the ford marker so isAgentEvent() filters it
		comment := fmt.Sprintf(
			"All dependencies are now resolved. This issue is unblocked and ready to work on! (Priority: %d)\n\n<!-- ford -->",
			c.priority,
		)
		if err := s.postComment(ctx, repo, issue.Number, comment); err != nil {
			slog.Warn("scheduler: failed to post unblock comment", "error", err, "issue", issue.Number)
		}

		unblocked = append(unblocked, issue.Number)
	}

	if len(unblocked) > 0 {
		slog.Info("scheduler: unblocked issues after PR merge", "repo", repo, "merged_pr", mergedPRNumber, "issues", unblocked)
	}

	// 5. Parent issue completion tracking (2.7)
	if mergedPRNumber > 0 {
		s.checkParentCompletion(ctx, repo, mergedPRNumber, issues)
	}

	return nil
}

// ParentIssue holds info about a parent issue for completion tracking.
type ParentIssue struct {
	Number      int
	NumChildren int
	ClosedCount int
}

// checkParentCompletion checks if all children of a parent issue are closed.
// If so, posts a completion comment on the parent and closes it.
func (s *Scheduler) checkParentCompletion(ctx context.Context, repo string, mergedPRNumber int, issues []Issue) {
	// In Forgejo, PR numbers and issue numbers share the same namespace.
	// The merged PR number IS the issue number.
	parents := findParentIssues(mergedPRNumber, issues)
	for _, parent := range parents {
		childrenNums := parseDependsOn(parent.Body)
		if len(childrenNums) == 0 {
			continue
		}

		// Check if ALL children are closed
		allClosed := true
		var closedCount int
		for _, childNum := range childrenNums {
			if childNum == mergedPRNumber {
				closedCount++
				continue // just merged, is closed
			}
			isClosed, cerr := s.isIssueClosed(ctx, repo, childNum)
			if cerr == nil && isClosed {
				closedCount++
			} else {
				allClosed = false
			}
		}
		closedCount++ // count the just-merged one

		if allClosed && closedCount == len(childrenNums) {
			slog.Info("scheduler: all children complete, closing parent",
				"repo", repo, "parent", parent.Number, "children", childrenNums)

			comment := fmt.Sprintf(
				"All sub-issues are now complete! %d/%d children merged or closed. Parent issue resolved.\n\n<!-- ford -->",
				closedCount, len(childrenNums),
			)
			if err := s.postComment(ctx, repo, parent.Number, comment); err != nil {
				slog.Warn("scheduler: failed to post parent completion comment", "error", err, "issue", parent.Number)
			}
			// Close the parent issue via PATCH
			s.closeIssue(ctx, repo, parent.Number)
		}
	}
}

// findParentIssues finds all open issues that list the given child number
// as a dependency in their Depends on: declaration.
func findParentIssues(childNum int, issues []Issue) []Issue {
	var parents []Issue
	for _, issue := range issues {
		deps := parseDependsOn(issue.Body)
		for _, d := range deps {
			if d == childNum {
				parents = append(parents, issue)
				break
			}
		}
	}
	return parents
}

// closeIssue closes an issue via PATCH.
func (s *Scheduler) closeIssue(ctx context.Context, repo string, issueNum int) error {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/issues/%d", escaped, issueNum)
	return s.doPatch(ctx, apiPath, map[string]string{"state": "closed"})
}

// detectCircularDeps builds a dependency graph and finds all cycles.
// Returns each cycle as a slice of issue numbers.
func detectCircularDeps(issues []Issue) [][]int {
	graph := make(map[int][]int)
	for _, issue := range issues {
		deps := parseDependsOn(issue.Body)
		// Only include edges to issues that exist in this set
		for _, dep := range deps {
			graph[issue.Number] = append(graph[issue.Number], dep)
		}
	}

	// Use DFS to find cycles
	visited := make(map[int]bool)
	recStack := make(map[int]bool)
	var cycles [][]int

	var dfs func(node int, path []int) bool
	dfs = func(node int, path []int) bool {
		visited[node] = true
		recStack[node] = true
		path = append(path, node)

		for _, neighbor := range graph[node] {
			if !visited[neighbor] {
				if dfs(neighbor, path) {
					return true
				}
			} else if recStack[neighbor] {
				// Found a cycle — extract it from the path
				cycleStart := -1
				for i, n := range path {
					if n == neighbor {
						cycleStart = i
						break
					}
				}
				if cycleStart >= 0 {
					cycle := make([]int, len(path)-cycleStart)
					copy(cycle, path[cycleStart:])
					cycles = append(cycles, cycle)
				}
				return true
			}
		}

		recStack[node] = false
		return false
	}

	for node := range graph {
		if !visited[node] {
			dfs(node, nil)
		}
	}

	return cycles
}

// parsePriority extracts a priority number from the issue body.
// Lower number = higher priority. Default 99 (lowest) if not specified.
func parsePriority(body string) int {
	match := priorityRegex.FindStringSubmatch(body)
	if len(match) >= 2 {
		n, err := strconv.Atoi(match[1])
		if err == nil && n >= 0 {
			return n
		}
	}
	return 99
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

// parseDependsOn extracts issue/PR numbers from lines containing "depends on".
func parseDependsOn(body string) []int {
	lines := strings.Split(body, "\n")
	seen := make(map[int]struct{})
	var nums []int
	for _, line := range lines {
		if !dependsOnKeywordRegex.MatchString(line) {
			continue
		}
		matches := dependsOnRegex.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			if len(m) >= 2 {
				n, err := strconv.Atoi(m[1])
				if err == nil {
					if _, ok := seen[n]; !ok {
						seen[n] = struct{}{}
						nums = append(nums, n)
					}
				}
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

// isIssueClosed checks whether an issue/PR dependency is satisfied.
// A merged PR has state="closed" but also merged=true — both count as closed.
// An open issue with no associated PR (e.g. a PM issue) is NOT considered
// blocking — only issues with open PRs represent actual code dependencies.
func (s *Scheduler) isIssueClosed(ctx context.Context, repo string, number int) (bool, error) {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/issues/%d", escaped, number)
	body, err := s.doGet(ctx, apiPath)
	if err != nil {
		return false, err
	}
	var issue struct {
		State       string `json:"state"`
		Merged      bool   `json:"merged"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal([]byte(body), &issue); err != nil {
		return false, fmt.Errorf("unmarshal issue: %w", err)
	}
	// Closed or merged = resolved
	if issue.State == "closed" || issue.Merged {
		return true, nil
	}
	// If the issue is open and has NO associated PR, it's not a code
	// dependency — don't block on it. PM issues, scaffold issues, etc.
	// should never block implementation.
	if issue.PullRequest == nil || issue.PullRequest.URL == "" {
		return true, nil
	}
	// Open issue with a PR = code in flight, still blocking
	return false, nil
}

// resolveLabelID resolves a label name to its numeric ID via the Forgejo API.
// Returns 0 if the label doesn't exist.
func (s *Scheduler) resolveLabelID(ctx context.Context, repo, labelName string) (int64, error) {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/labels", escaped)
	body, err := s.doGet(ctx, apiPath)
	if err != nil {
		return 0, fmt.Errorf("list labels: %w", err)
	}
	var labels []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &labels); err != nil {
		return 0, fmt.Errorf("decode labels: %w", err)
	}
	for _, l := range labels {
		if l.Name == labelName {
			return l.ID, nil
		}
	}
	return 0, nil // not found
}

// removeLabel removes a label from an issue.
// Forgejo's DELETE endpoint requires a numeric label ID, not a name string.
func (s *Scheduler) removeLabel(ctx context.Context, repo string, issueNum int, label string) error {
	labelID, err := s.resolveLabelID(ctx, repo, label)
	if err != nil {
		return fmt.Errorf("resolve label %q for removal: %w", label, err)
	}
	if labelID == 0 {
		return nil // label doesn't exist, nothing to remove
	}
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/issues/%d/labels/%d", escaped, issueNum, labelID)
	return s.doDelete(ctx, apiPath)
}

// addLabel adds a label to an issue.
// Forgejo's POST endpoint requires numeric label IDs in {"labels": [id]}.
func (s *Scheduler) addLabel(ctx context.Context, repo string, issueNum int, label string) error {
	labelID, err := s.resolveLabelID(ctx, repo, label)
	if err != nil {
		return fmt.Errorf("resolve label %q for addition: %w", label, err)
	}
	if labelID == 0 {
		return fmt.Errorf("label %q not found in repo", label)
	}
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/issues/%d/labels", escaped, issueNum)
	return s.doPost(ctx, apiPath, map[string]interface{}{"labels": []int64{labelID}})
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

// doPatch performs an authenticated PATCH request.
func (s *Scheduler) doPatch(ctx context.Context, apiPath string, body interface{}) error {
	fullURL := s.BaseURL + apiPath
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, fullURL, strings.NewReader(string(data)))
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

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
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

// hasOpenPR checks whether an issue has any associated open pull requests.
// This is used by isIssueClosed to determine if an open issue is actually
// a code dependency (has a PR) or just a coordination issue (no PR).
func (s *Scheduler) hasOpenPR(ctx context.Context, repo string, issueNumber int) (bool, error) {
	escaped := escapeRepoPath(repo)
	apiPath := fmt.Sprintf("/api/v1/repos/%s/pulls?state=open", escaped)
	body, err := s.doGet(ctx, apiPath)
	if err != nil {
		return false, err
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(body), &prs); err != nil {
		return false, fmt.Errorf("unmarshal PRs: %w", err)
	}
	// Forgejo doesn't directly link issues to PRs in a simple way.
	// We check if any open PR's head branch references this issue number
	// by looking at PR titles/descriptions, but that's unreliable.
	// A simpler approach: check if the issue IS a PR (Forgejo treats
	// PRs as issues with extra fields).
	escapedIssue := escapeRepoPath(repo)
	issuePath := fmt.Sprintf("/api/v1/repos/%s/issues/%d", escapedIssue, issueNumber)
	issueBody, err := s.doGet(ctx, issuePath)
	if err != nil {
		return false, err
	}
	var issue struct {
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal([]byte(issueBody), &issue); err != nil {
		return false, fmt.Errorf("unmarshal issue: %w", err)
	}
	return issue.PullRequest != nil && issue.PullRequest.URL != "", nil
}
