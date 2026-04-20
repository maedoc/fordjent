package tool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		if r.URL.EscapedPath() != "/api/v1/repos/org%2Frepo/issues/42/comments" {
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
	if receivedBody["body"] != "Hello world" {
		t.Errorf("expected body 'Hello world', got %s", receivedBody["body"])
	}
}

func TestGetIssueToolExecute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.EscapedPath() != "/api/v1/repos/org%2Frepo/issues/42" {
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
		if r.URL.EscapedPath() != "/api/v1/repos/org%2Frepo/pulls" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		json.NewDecoder(r.Body).Decode(&receivedBody)
		json.NewEncoder(w).Encode(map[string]interface{}{"number": 7})
	}))
	defer server.Close()

	tool := NewCreatePRTool(newTestAdapter(server))
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
	if receivedPath != "/api/v1/repos/org%2Frepo/issues/42/reactions" {
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
	if receivedPath != "/api/v1/repos/org%2Frepo/issues/comments/100/reactions" {
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
