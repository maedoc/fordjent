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
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		path := r.URL.EscapedPath()
		switch path {
		case "/api/v1/repos/org/repo/pulls":
			json.NewDecoder(r.Body).Decode(&receivedBody)
			json.NewEncoder(w).Encode(map[string]interface{}{"number": 7})
		case "/api/v1/repos/org/repo/pulls/7/requested_reviewers":
			// Reviewer request — expected side effect of PR creation
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path: %s", path)
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
