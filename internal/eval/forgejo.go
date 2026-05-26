package eval

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Standard labels for benchmark repos.
var standardLabels = []struct {
	Name  string
	Color string
}{
	{"planning", "0ea5db"},
	{"implementing", "fbca04"},
	{"ready", "c2e07c"},
	{"blocked", "b60205"},
	{"done", "28a745"},
	{"review", "fbca04"},
	{"scaffold", "1d76db"},
	{"automerge", "28a745"},
	{"fordjent/failed:max-turns", "b60205"},
	{"fordjent/failed:error", "b60205"},
	{"needs-role", "b60205"},
	{"in_progress", "fbca04"},
	{"plan-approved", "28a745"},
	{"role:implementer", "207de5"},
	{"role:pm", "a0d5e4"},
	{"role:reviewer", "e9d76f"},
	{"role:tester", "bfd4f2"},
	{"role:devops", "f9d5cc"},
}

// CreateRepo creates a Forgejo repository for a benchmark scenario.
func (h *Harness) CreateRepo(name string) error {
	t := h.t
	t.Helper()

	body := map[string]interface{}{
		"name":          name,
		"description":   "Fordjent eval benchmark: " + name,
		"private":       false,
		"auto_init":     true,
		"default_branch": "main",
	}

	respBody, err := h.doForgejoRequest("POST", "/user/repos", body)
	if err != nil {
		return fmt.Errorf("create repo: %w", err)
	}

	var repo map[string]interface{}
	if err := json.Unmarshal([]byte(respBody), &repo); err != nil {
		return fmt.Errorf("parse repo response: %w", err)
	}

	t.Logf("Created repo: %s", name)

	// Wait for repo to be fully initialized
	time.Sleep(2 * time.Second)

	return nil
}

// SeedFiles creates files in the repository via the Forgejo contents API.
func (h *Harness) SeedFiles(repo string, files map[string]string) error {
	t := h.t
	t.Helper()

	for path, content := range files {
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		body := map[string]interface{}{
			"message": fmt.Sprintf("add %s", path),
			"content": encoded,
		}

		_, err := h.doForgejoRequest("POST",
			fmt.Sprintf("/repos/%s/contents/%s", repo, path), body)
		if err != nil {
			return fmt.Errorf("seed file %s: %w", path, err)
		}
		t.Logf("Seeded file: %s/%s", repo, path)
	}

	// Wait for git to sync
	time.Sleep(1 * time.Second)
	return nil
}

// CreateLabels creates the standard FSM and role labels in the repository.
func (h *Harness) CreateLabels(repo string) error {
	t := h.t
	t.Helper()

	for _, label := range standardLabels {
		body := map[string]interface{}{
			"name":  label.Name,
			"color": label.Color,
		}
		_, err := h.doForgejoRequest("POST",
			fmt.Sprintf("/repos/%s/labels", repo), body)
		if err != nil {
			// Labels may already exist, that's OK
			t.Logf("Label %s (may already exist): %v", label.Name, err)
			continue
		}
	}
	t.Logf("Created %d labels for %s", len(standardLabels), repo)
	return nil
}

// CreateWebhook registers a webhook pointing to Fordjent.
func (h *Harness) CreateWebhook(repo string) error {
	t := h.t
	t.Helper()

	body := map[string]interface{}{
		"type": "forgejo",
		"config": map[string]interface{}{
			"url":          h.FordjentURL + "/acp/v1/events",
			"content_type": "json",
			"secret":       h.WebhookSecret,
		},
		"events": []string{"issues", "issue_comment", "pull_request", "pull_request_review_comment"},
		"active": true,
	}

	_, err := h.doForgejoRequest("POST",
		fmt.Sprintf("/repos/%s/hooks", repo), body)
	if err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}

	t.Logf("Created webhook for %s → %s", repo, h.FordjentURL+"/acp/v1/events")
	return nil
}

// CreateIssue creates an issue in the repository and returns the issue number.
func (h *Harness) CreateIssue(repo, title, body string) (int, error) {
	t := h.t
	t.Helper()

	issueBody := map[string]interface{}{
		"title": title,
		"body":  body,
	}

	respBody, err := h.doForgejoRequest("POST",
		fmt.Sprintf("/repos/%s/issues", repo), issueBody)
	if err != nil {
		return 0, fmt.Errorf("create issue: %w", err)
	}

	var issue map[string]interface{}
	if err := json.Unmarshal([]byte(respBody), &issue); err != nil {
		return 0, fmt.Errorf("parse issue response: %w", err)
	}

	issueNum := int(extractFloat64(issue["number"]))
	t.Logf("Created issue #%d: %s", issueNum, title)
	return issueNum, nil
}

// GetIssueState returns the state of an issue ("open" or "closed").
func (h *Harness) GetIssueState(repo string, issueNum int) (string, error) {
	respBody, err := h.doForgejoRequest("GET",
		fmt.Sprintf("/repos/%s/issues/%d", repo, issueNum), nil)
	if err != nil {
		return "", fmt.Errorf("get issue: %w", err)
	}

	var issue map[string]interface{}
	if err := json.Unmarshal([]byte(respBody), &issue); err != nil {
		return "", fmt.Errorf("parse issue: %w", err)
	}

	state, _ := issue["state"].(string)
	return state, nil
}

// GetIssueLabels returns the labels on an issue.
func (h *Harness) GetIssueLabels(repo string, issueNum int) ([]string, error) {
	respBody, err := h.doForgejoRequest("GET",
		fmt.Sprintf("/repos/%s/issues/%d/labels", repo, issueNum), nil)
	if err != nil {
		return nil, fmt.Errorf("get labels: %w", err)
	}

	var labels []map[string]interface{}
	if err := json.Unmarshal([]byte(respBody), &labels); err != nil {
		return nil, fmt.Errorf("parse labels: %w", err)
	}

	names := make([]string, 0, len(labels))
	for _, l := range labels {
		if name, ok := l["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names, nil
}

// GetPRList returns all pull requests for a repository.
func (h *Harness) GetPRList(repo string) ([]map[string]interface{}, error) {
	respBody, err := h.doForgejoRequest("GET",
		fmt.Sprintf("/repos/%s/pulls?state=all", repo), nil)
	if err != nil {
		return nil, fmt.Errorf("get PRs: %w", err)
	}

	var prs []map[string]interface{}
	if err := json.Unmarshal([]byte(respBody), &prs); err != nil {
		return nil, fmt.Errorf("parse PRs: %w", err)
	}
	return prs, nil
}

// CloseIssue closes an issue.
func (h *Harness) CloseIssue(repo string, issueNum int) error {
	body := map[string]interface{}{
		"state": "closed",
	}
	_, err := h.doForgejoRequest("PATCH",
		fmt.Sprintf("/repos/%s/issues/%d", repo, issueNum), body)
	return err
}

// CloseAllIssues closes all open issues in a repository.
func (h *Harness) CloseAllIssues(repo string) error {
	respBody, err := h.doForgejoRequest("GET",
		fmt.Sprintf("/repos/%s/issues?state=open&limit=50", repo), nil)
	if err != nil {
		return fmt.Errorf("list issues: %w", err)
	}

	var issues []map[string]interface{}
	if err := json.Unmarshal([]byte(respBody), &issues); err != nil {
		return fmt.Errorf("parse issues: %w", err)
	}

	for _, issue := range issues {
		num := int(extractFloat64(issue["number"]))
		if err := h.CloseIssue(repo, num); err != nil {
			h.t.Logf("warning: failed to close issue #%d: %v", num, err)
		}
	}
	return nil
}

// DeleteRepo deletes the entire repository.
func (h *Harness) DeleteRepo(repo string) error {
	_, err := h.doForgejoRequest("DELETE", fmt.Sprintf("/repos/%s", repo), nil)
	return err
}