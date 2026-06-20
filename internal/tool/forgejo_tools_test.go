package tool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAdapter(server *httptest.Server) *ForgejoAdapter {
	return NewForgejoAdapter(server.URL, "test-token")
}

func TestCommentToolExecute(t *testing.T) {
	var receivedBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.EscapedPath() != "/api/v1/repos/org/repo/issues/42/comments" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "token test-token" {
			t.Errorf("missing auth header")
		}
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 1})
	}))
	defer server.Close()

	tool := NewCommentTool(newTestAdapter(server))
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"issue_number": 42,
		"body": "Hello world"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Comment posted successfully" {
		t.Errorf("unexpected result: %s", result)
	}
	if !strings.HasPrefix(receivedBody["body"], "Hello world") {
		t.Errorf("expected body starting with 'Hello world', got %s", receivedBody["body"])
	}
}

func TestGetIssueToolExecute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.EscapedPath() != "/api/v1/repos/org/repo/issues/42" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"number": 42, "title": "Test issue", "body": "body text", "state": "open",
		})
	}))
	defer server.Close()

	tool := NewGetIssueTool(newTestAdapter(server))
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"issue_number": 42
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestListIssuesToolDefaultParams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("expected state=open, got %s", r.URL.Query().Get("state"))
		}
		if r.URL.Query().Get("limit") != "20" {
			t.Errorf("expected limit=20, got %s", r.URL.Query().Get("limit"))
		}
		json.NewEncoder(w).Encode([]interface{}{})
	}))
	defer server.Close()

	tool := NewListIssuesTool(newTestAdapter(server))
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repository": "org/repo"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreatePRToolExecute(t *testing.T) {
	var receivedBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch path {
		case "/api/v1/repos/org/repo/pulls":
			if r.Method != http.MethodPost {
				t.Errorf("expected POST for pulls, got %s", r.Method)
			}
			json.NewDecoder(r.Body).Decode(&receivedBody)
			json.NewEncoder(w).Encode(map[string]interface{}{"number": 7, "user": map[string]string{"login": "org"}})
		case "/api/v1/repos/org/repo/pulls/7/requested_reviewers":
			// Reviewer request — expected side effect of PR creation
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/repos/org/repo/collaborators":
			// Collaborators list — PR author is org, so no external reviewers
			json.NewEncoder(w).Encode([]map[string]string{{"login": "org", "permission": "admin"}})
		case "/api/v1/repos/org/repo/labels":
			// List labels — called by AddIssueLabels to validate
			json.NewEncoder(w).Encode([]map[string]string{{"name": "automerge"}})
		case "/api/v1/repos/org/repo/issues/7/labels":
			// Add labels to issue/PR — automerge label
			json.NewEncoder(w).Encode([]map[string]string{{"name": "automerge"}})
		case "/api/v1/repos/org/repo/issues/7":
			// Get issue — called by AddIssueLabels to check existing labels
			json.NewEncoder(w).Encode(map[string]interface{}{"number": 7, "labels": []interface{}{}})
		default:
			t.Errorf("unexpected path: %s %s", r.Method, path)
		}
	}))
	defer server.Close()

	tool := NewCreatePRTool(newTestAdapter(server), nil, "")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"title": "Fix bug",
		"body": "Description",
		"head": "fix-branch",
		"base": "main"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedBody["head"] != "fix-branch" || receivedBody["base"] != "main" {
		t.Errorf("unexpected PR params: %+v", receivedBody)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

// TestSubmitReviewToolExecute_Approve verifies that state=approved posts the
// review and removes an existing changes_requested label.
func TestSubmitReviewToolExecute_Approve(t *testing.T) {
	var reviewBody map[string]string
	var removeLabelCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch p {
		case "/api/v1/repos/org/repo/pulls/7/reviews":
			if r.Method == http.MethodGet {
				// Duplicate-review dedup probe (A2): returns empty list = no existing review.
				json.NewEncoder(w).Encode([]map[string]interface{}{})
				return
			}
			if r.Method != http.MethodPost {
				t.Errorf("expected POST for reviews, got %s", r.Method)
			}
			json.NewDecoder(r.Body).Decode(&reviewBody)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": 99, "state": "approved"})
		case "/api/v1/repos/org/repo/labels":
			// RemoveIssueLabel queries label list to resolve label ID.
			json.NewEncoder(w).Encode([]map[string]interface{}{{"id": 42, "name": "changes_requested"}})
		case "/api/v1/repos/org/repo/issues/7/labels/42":
			if r.Method != http.MethodDelete {
				t.Errorf("expected DELETE for label removal, got %s", r.Method)
			}
			removeLabelCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path: %s %s", r.Method, p)
		}
	}))
	defer server.Close()

	tool := NewSubmitReviewTool(newTestAdapter(server), "djent-qa")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"pr_number": 7,
		"state": "approved",
		"body": "LGTM"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reviewBody["event"] != "approved" {
		t.Errorf("expected event=approved, got %q", reviewBody["event"])
	}
	if !removeLabelCalled {
		t.Error("approved review should remove changes_requested label")
	}
	if !strings.Contains(result, "approved") {
		t.Errorf("result should mention state, got %q", result)
	}
}

// TestSubmitReviewToolExecute_RequestChanges verifies that
// state=changes_requested posts the review AND adds the label.
func TestSubmitReviewToolExecute_RequestChanges(t *testing.T) {
	var reviewBody map[string]string
	var addLabelCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch p {
		case "/api/v1/repos/org/repo/pulls/7/reviews":
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode([]map[string]interface{}{})
				return
			}
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			json.NewDecoder(r.Body).Decode(&reviewBody)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": 99, "state": "changes_requested"})
		case "/api/v1/repos/org/repo/labels":
			// AddIssueLabels validates id first via ListLabels
			json.NewEncoder(w).Encode([]map[string]interface{}{{"id": 42, "name": "changes_requested"}})
		case "/api/v1/repos/org/repo/issues/7":
			// AddIssueLabels calls GetIssue to check existing labels
			json.NewEncoder(w).Encode(map[string]interface{}{"number": 7, "labels": []interface{}{}})
		case "/api/v1/repos/org/repo/issues/7/labels":
			if r.Method != http.MethodPost {
				t.Errorf("expected POST for label add, got %s", r.Method)
			}
			addLabelCalled = true
			json.NewEncoder(w).Encode([]map[string]string{{"name": "changes_requested"}})
		default:
			t.Errorf("unexpected path: %s %s", r.Method, p)
		}
	}))
	defer server.Close()

	tool := NewSubmitReviewTool(newTestAdapter(server), "djent-qa")
	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"pr_number": 7,
		"state": "changes_requested",
		"body": "Add tests"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reviewBody["event"] != "changes_requested" {
		t.Errorf("expected event=changes_requested, got %q", reviewBody["event"])
	}
	if !addLabelCalled {
		t.Error("changes_requested review should add the changes_requested label")
	}
}

// TestSubmitReviewToolExecute_InvalidState verifies the state validator.
func TestSubmitReviewToolExecute_InvalidState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP call should have been made, got %s %s", r.Method, r.URL.EscapedPath())
	}))
	defer server.Close()

	tool := NewSubmitReviewTool(newTestAdapter(server), "djent-qa")
	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"pr_number": 7,
		"state": "bogus",
		"body": "x"
	}`))
	if err == nil {
		t.Fatal("expected error for invalid state")
	}
	if !strings.Contains(err.Error(), "invalid state") {
		t.Errorf("expected 'invalid state' error, got %v", err)
	}
}

// TestSubmitReviewTool_DedupSuppressesDuplicateApproved is the A2 scenario
// "duplicate-submission guard": when the model calls forgejo_submit_review
// (state=approved) twice in the same session, the second call SHOULD return
// the same success JSON without hitting the Forgejo API. Backing evidence:
// GEMMA-FAILURE-ANALYSIS.md (pulls/14 submitted `approved` 3× in a row).
func TestSubmitReviewTool_DedupSuppressesDuplicateApproved(t *testing.T) {
	reviewsPostCount := 0
	labelsTouchCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case p == "/api/v1/repos/org/repo/pulls/7/reviews" && r.Method == http.MethodGet:
			// First call: report NO review existingdjent-qa review exists.
			if reviewsPostCount == 0 {
				json.NewEncoder(w).Encode([]map[string]interface{}{})
			} else {
				json.NewEncoder(w).Encode([]map[string]interface{}{{
					"id": 99, "state": "APPROVED",
					"user": map[string]interface{}{"login": "djent-qa"},
				}})
			}
		case p == "/api/v1/repos/org/repo/pulls/7/reviews" && r.Method == http.MethodPost:
			reviewsPostCount++
			json.NewEncoder(w).Encode(map[string]interface{}{"id": 99, "state": "approved"})
		case strings.Contains(p, "/issues/7/labels"):
			labelsTouchCount++
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(p, "/labels"):
			json.NewEncoder(w).Encode([]map[string]interface{}{{"id": 1, "name": "changes_requested"}})
		case strings.Contains(p, "/issues/7"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state": "open", "labels": []interface{}{}, "number": 7, "title": "PR",
			})
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	defer server.Close()

	tool := NewSubmitReviewTool(newTestAdapter(server), "djent-qa")

	// First call — submits to Forgejo.
	res1, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo", "pr_number": 7, "state": "approved", "body": "LGTM"
	}`))
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if !strings.Contains(res1, `"duplicate":false`) {
		t.Errorf("first submit result should be duplicate=false; got: %s", res1)
	}
	if reviewsPostCount != 1 {
		t.Errorf("expected exactly 1 POST /reviews on first submit, got %d", reviewsPostCount)
	}
	firstLabelTouches := labelsTouchCount

	// Second call — should return the same success, NOT hit the API, NOT touch labels.
	res2, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo", "pr_number": 7, "state": "approved", "body": "LGTM again"
	}`))
	if err != nil {
		t.Fatalf("duplicate submit: %v", err)
	}
	if !strings.Contains(res2, `"duplicate":true`) {
		t.Errorf("duplicate submit result should be duplicate=true; got: %s", res2)
	}
	if reviewsPostCount != 1 {
		t.Errorf("expected NO additional POST /reviews on duplicate submit; got total %d", reviewsPostCount)
	}
	if labelsTouchCount != firstLabelTouches {
		t.Errorf("expected NO label side-effects on duplicate submit; labels touched %d more times", labelsTouchCount-firstLabelTouches)
	}
}

// TestSubmitReviewTool_DifferentStatesNotDeduplicated is the A2 scenario
// "different state is not a duplicate": submitted `approved`, then
// `changes_requested` for the same PR → both calls execute normally.
func TestSubmitReviewTool_DifferentStatesNotDeduplicated(t *testing.T) {
	reviewsPostCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case p == "/api/v1/repos/org/repo/pulls/7/reviews" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]interface{}{}) // no existing reviews
		case p == "/api/v1/repos/org/repo/pulls/7/reviews" && r.Method == http.MethodPost:
			reviewsPostCount++
			json.NewEncoder(w).Encode(map[string]interface{}{"id": 100 + reviewsPostCount, "state": "approved"})
		case strings.Contains(p, "/issues/7/labels"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(p, "/labels"):
			json.NewEncoder(w).Encode([]map[string]interface{}{{"id": 1, "name": "changes_requested"}})
		case strings.Contains(p, "/issues/7"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state": "open", "labels": []interface{}{}, "number": 7, "title": "PR",
			})
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	defer server.Close()

	tool := NewSubmitReviewTool(newTestAdapter(server), "djent-qa")

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo", "pr_number": 7, "state": "approved", "body": "LGTM"
	}`)); err != nil {
		t.Fatalf("approved submit: %v", err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo", "pr_number": 7, "state": "changes_requested", "body": "needs work"
	}`)); err != nil {
		t.Fatalf("changes_requested submit: %v", err)
	}
	if reviewsPostCount != 2 {
		t.Errorf("expected both submits to POST to Forgejo (got %d); a different state must NOT be deduplicated", reviewsPostCount)
	}
}

func TestSearchCodeToolEscapesQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "func main()" {
			t.Errorf("expected unescaped query 'func main()', got %s", r.URL.Query().Get("q"))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}})
	}))
	defer server.Close()

	tool := NewSearchCodeTool(newTestAdapter(server))
	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"query": "func main()"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAddReactionToolOnIssue(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.EscapedPath()
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"content": "eyes"})
	}))
	defer server.Close()

	tool := NewAddReactionTool(newTestAdapter(server))
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"issue_number": 42,
		"reaction": "eyes"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedPath != "/api/v1/repos/org/repo/issues/42/reactions" {
		t.Errorf("unexpected path: %s", receivedPath)
	}
	if receivedBody["content"] != "eyes" {
		t.Errorf("expected 'eyes', got %s", receivedBody["content"])
	}
	if result != "Reaction 'eyes' added" {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestAddReactionToolOnComment(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"content": "+1"})
	}))
	defer server.Close()

	tool := NewAddReactionTool(newTestAdapter(server))
	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"issue_number": 42,
		"comment_id": 100,
		"reaction": "+1"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedPath != "/api/v1/repos/org/repo/issues/comments/100/reactions" {
		t.Errorf("unexpected path: %s", receivedPath)
	}
}

func TestToolAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message": "internal error"}`))
	}))
	defer server.Close()

	tool := NewGetIssueTool(newTestAdapter(server))
	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"issue_number": 42
	}`))
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestToolBadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	tool := NewGetIssueTool(newTestAdapter(server))
	_, err := tool.Execute(context.Background(), json.RawMessage(`not json`))
	if err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestGetSubIssuesToolExecute(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.EscapedPath())
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/1"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 1,
				"title":  "[pm] Add feature X",
				"body":   "Decompose work.\n\nDepends on: #2, #3",
				"state":  "open",
			})
		case strings.HasSuffix(r.URL.Path, "/issues/2"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":       2,
				"title":        "Implement part A",
				"body":         "Part A",
				"state":        "closed",
				"merged":       true,
				"pull_request": nil,
			})
		case strings.HasSuffix(r.URL.Path, "/issues/3"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 3,
				"title":  "Implement part B",
				"body":   "Part B",
				"state":  "open",
				"pull_request": map[string]interface{}{
					"url": "http://localhost:3000/api/v1/repos/org/repo/pulls/3",
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	tool := NewGetSubIssuesTool(newTestAdapter(server))
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"parent_issue_number": 1
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "#2") || !strings.Contains(result, "#3") {
		t.Errorf("expected sub-issues #2 and #3 in result, got: %s", result)
	}
	if !strings.Contains(result, "still open") {
		t.Errorf("expected 'still open' message since #3 is open, got: %s", result)
	}
}

func TestGetSubIssuesToolAllComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/1"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 1,
				"title":  "[pm] Add feature X",
				"body":   "Decompose work.\n\nDepends on: #2, #3",
				"state":  "open",
			})
		case strings.HasSuffix(r.URL.Path, "/issues/2"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 2, "title": "Part A", "body": "A", "state": "closed", "merged": true,
			})
		case strings.HasSuffix(r.URL.Path, "/issues/3"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 3, "title": "Part B", "body": "B", "state": "closed", "merged": true,
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	tool := NewGetSubIssuesTool(newTestAdapter(server))
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"parent_issue_number": 1
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "All sub-issues are complete") {
		t.Errorf("expected completion message, got: %s", result)
	}
}

func TestGetSubIssuesToolNoDeps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"number": 1, "title": "No deps", "body": "Just a regular issue", "state": "open",
		})
	}))
	defer server.Close()

	tool := NewGetSubIssuesTool(newTestAdapter(server))
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"parent_issue_number": 1
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "no 'Depends on:' references") {
		t.Errorf("expected no deps message, got: %s", result)
	}
}

func TestPingParentToolExecute(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		receivedPath = r.URL.EscapedPath()
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 99})
	}))
	defer server.Close()

	tool := NewPingParentTool(newTestAdapter(server))
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"parent_issue_number": 5,
		"message": "Should this function return an error or a boolean?"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Ping sent to PM on issue #5" {
		t.Errorf("unexpected result: %s", result)
	}
	if receivedPath != "/api/v1/repos/org/repo/issues/5/comments" {
		t.Errorf("unexpected path: %s", receivedPath)
	}
	body := receivedBody["body"]
	if !strings.Contains(body, "**[Implementer → PM]**") {
		t.Errorf("expected [Implementer → PM] prefix in body, got: %s", body)
	}
	if !strings.Contains(body, "Should this function return an error or a boolean?") {
		t.Errorf("expected message in body, got: %s", body)
	}
	if !strings.Contains(body, "<!-- ford-ping -->") {
		t.Errorf("expected ford-ping marker in body, got: %s", body)
	}
	if strings.Contains(body, "<!-- ford -->") {
		t.Errorf("expected NO ford marker in body (would be filtered by isAgentEvent), got: %s", body)
	}
}

func TestPingParentToolInvalidIssueNumber(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tool := NewPingParentTool(newTestAdapter(server))
	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"repository": "org/repo",
		"parent_issue_number": 0,
		"message": "Hello"
	}`))
	if err == nil {
		t.Error("expected error for invalid issue number")
	}
}

func TestPingParentToolBadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	tool := NewPingParentTool(newTestAdapter(server))
	_, err := tool.Execute(context.Background(), json.RawMessage(`not json`))
	if err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestParseSubIssueDeps(t *testing.T) {
	cases := []struct {
		body string
		want []int
	}{
		{"Depends on: #10", []int{10}},
		{"depends on: #10, #11", []int{10, 11}},
		{"No deps here.", nil},
		{"Depends on: #5\nSome other text", []int{5}},
	}

	for _, tc := range cases {
		got := parseSubIssueDeps(tc.body)
		if len(got) != len(tc.want) {
			t.Errorf("parseSubIssueDeps(%q) = %v, want %v", tc.body, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseSubIssueDeps(%q)[%d] = %d, want %d", tc.body, i, got[i], tc.want[i])
			}
		}
	}
}
