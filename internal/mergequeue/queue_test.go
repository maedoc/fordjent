package mergequeue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fordjent/fordjent/internal/tool"
)

func TestCheckGate_NoConflicts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		q := r.URL.RawQuery
		switch {
		case strings.Contains(path, "/compare/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]string{
					{"filename": "internal/command/config.go"},
				},
			})
		case path == "/api/v1/repos/fjadmin/gogit/pulls" && strings.Contains(q, "state=open"):
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number": 10,
					"state":  "open",
					"head":   map[string]string{"ref": "feature/other", "sha": "abc123"},
					"base":   map[string]string{"ref": "main", "sha": "def456"},
				},
			})
		case path == "/api/v1/repos/fjadmin/gogit/pulls/10/files":
			json.NewEncoder(w).Encode([]map[string]string{
				{"filename": "internal/command/other.go"},
			})
		default:
			t.Fatalf("unexpected request: %s?%s", path, q)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	mq := NewClient(adapter)

	blocked, msg, err := mq.CheckGate(context.Background(), "fjadmin/gogit", "feature/config", "main")
	if err != nil {
		t.Fatalf("CheckGate error: %v", err)
	}
	if blocked {
		t.Fatalf("expected no conflict, got blocked: %s", msg)
	}
}

func TestCheckGate_WithConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		q := r.URL.RawQuery
		switch {
		case strings.Contains(path, "/compare/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]string{
					{"filename": "cmd/gogit/main.go"},
				},
			})
		case path == "/api/v1/repos/fjadmin/gogit/pulls" && strings.Contains(q, "state=open"):
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number": 10,
					"state":  "open",
					"head":   map[string]string{"ref": "feature/status", "sha": "abc123"},
					"base":   map[string]string{"ref": "main", "sha": "def456"},
				},
			})
		case path == "/api/v1/repos/fjadmin/gogit/pulls/10/files":
			json.NewEncoder(w).Encode([]map[string]string{
				{"filename": "cmd/gogit/main.go"},
			})
		default:
			t.Fatalf("unexpected request: %s?%s", path, q)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	mq := NewClient(adapter)

	blocked, msg, err := mq.CheckGate(context.Background(), "fjadmin/gogit", "feature/config", "main")
	if err != nil {
		t.Fatalf("CheckGate error: %v", err)
	}
	if !blocked {
		t.Fatal("expected blocked due to file overlap, but was allowed")
	}
	if !strings.Contains(msg, "10") {
		t.Fatalf("expected message to reference PR #10, got: %s", msg)
	}
	if !strings.Contains(msg, "cmd/gogit/main.go") {
		t.Fatalf("expected message to reference conflicting file, got: %s", msg)
	}
	fmt.Printf("✅ Block message: %s\n", msg)
}

func TestCheckGate_SelfBranch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		q := r.URL.RawQuery
		switch {
		case strings.Contains(path, "/compare/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]string{
					{"filename": "internal/command/config.go"},
				},
			})
		case path == "/api/v1/repos/fjadmin/gogit/pulls" && strings.Contains(q, "state=open"):
			// Our own branch already has an open PR
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number": 10,
					"state":  "open",
					"head":   map[string]string{"ref": "feature/config", "sha": "abc123"},
					"base":   map[string]string{"ref": "main", "sha": "def456"},
				},
			})
		default:
			t.Fatalf("unexpected request: %s?%s", path, q)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	mq := NewClient(adapter)

	blocked, msg, err := mq.CheckGate(context.Background(), "fjadmin/gogit", "feature/config", "main")
	if err != nil {
		t.Fatalf("CheckGate error: %v", err)
	}
	if blocked {
		t.Fatalf("should not block when the existing PR is from the same branch, got: %s", msg)
	}
}
