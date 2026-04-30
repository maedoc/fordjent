package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetIssue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v1/repos/org/repo/issues/42" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "token test-token" {
			t.Errorf("missing auth header: %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"number": 42,
			"title":  "Bug report",
			"body":   "Something is broken",
			"state":  "open",
			"user":   map[string]interface{}{"id": 1, "login": "alice"},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	issue, err := client.GetIssue(context.Background(), "org/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issue.Number != 42 {
		t.Errorf("expected issue 42, got %d", issue.Number)
	}
	if issue.Title != "Bug report" {
		t.Errorf("expected 'Bug report', got %s", issue.Title)
	}
	if issue.User.Login != "alice" {
		t.Errorf("expected sender alice, got %s", issue.User.Login)
	}
}

func TestGetIssueNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message": "issue not found"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetIssue(context.Background(), "org/repo", 999)
	if err == nil {
		t.Error("expected error for 404")
	}
}

func TestListComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v1/repos/org/repo/issues/42/comments" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"id":   1,
				"body": "First comment",
				"user": map[string]interface{}{"login": "alice"},
			},
			{
				"id":   2,
				"body": "Second comment",
				"user": map[string]interface{}{"login": "bob"},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	comments, err := client.ListComments(context.Background(), "org/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
	if comments[0].User != "alice" {
		t.Errorf("expected alice, got %s", comments[0].User)
	}
	if comments[1].Body != "Second comment" {
		t.Errorf("expected 'Second comment', got %s", comments[1].Body)
	}
}

func TestAddReactionToIssue(t *testing.T) {
	var receivedMethod, receivedPath string
	var receivedBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.EscapedPath()
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"content": "eyes"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	err := client.AddReaction(context.Background(), "org/repo", 42, 0, "eyes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", receivedMethod)
	}
	if receivedPath != "/api/v1/repos/org/repo/issues/42/reactions" {
		t.Errorf("unexpected path: %s", receivedPath)
	}
	if receivedBody["content"] != "eyes" {
		t.Errorf("expected reaction 'eyes', got %s", receivedBody["content"])
	}
}

func TestAddReactionToComment(t *testing.T) {
	var receivedPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"content": "+1"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	err := client.AddReaction(context.Background(), "org/repo", 42, 100, "+1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedPath != "/api/v1/repos/org/repo/issues/comments/100/reactions" {
		t.Errorf("unexpected path: %s", receivedPath)
	}
}

func TestURLPathEscaping(t *testing.T) {
	// Verify that repository names with special chars are escaped
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.EscapedPath()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"number": 1, "title": "test", "body": "", "state": "open",
			"user": map[string]interface{}{"id": 1, "login": "test"},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetIssue(context.Background(), "org/repo-with/slashes", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// escapeRepoPath("org/repo-with/slashes") = "org/repo-with/slashes"
	expected := "/api/v1/repos/org/repo-with/slashes/issues/1"
	if receivedPath != expected {
		t.Errorf("expected path %s, got %s", expected, receivedPath)
	}
}
