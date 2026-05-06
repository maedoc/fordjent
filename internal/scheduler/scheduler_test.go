package scheduler

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

func TestOnPRMerged_UnblocksDependentIssue(t *testing.T) {
	var captured []struct {
		Method string
		Path   string
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, struct {
			Method string
			Path   string
		}{Method: r.Method, Path: r.URL.Path})

		switch {
		case r.URL.Path == "/api/v1/repos/fjadmin/gogit/issues":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number": 15,
					"title":  "Add clone",
					"body":   "Depends on: #10. Blocked.",
					"state":  "open",
					"labels": []map[string]interface{}{
						{"name": "blocked"},
					},
				},
				{
					"number": 16,
					"title":  "Add status",
					"body":   "No deps here.",
					"state":  "open",
					"labels": []map[string]interface{}{},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/issues/10"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state": "closed",
			})
		case r.URL.Path == "/api/v1/repos/fjadmin/gogit/issues/15/labels/blocked":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/labels"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	err := s.OnPRMerged(context.Background(), "fjadmin/gogit", 10)
	if err != nil {
		t.Fatalf("OnPRMerged error: %v", err)
	}

	var foundRemoveBlocked, foundAddReady, foundComment bool
	for _, c := range captured {
		if c.Method == "DELETE" && strings.Contains(c.Path, "15/labels/blocked") {
			foundRemoveBlocked = true
		}
		if c.Method == "POST" && strings.Contains(c.Path, "15/labels") && !strings.Contains(c.Path, "blocked") {
			foundAddReady = true
		}
		if c.Method == "POST" && strings.Contains(c.Path, "15/comments") {
			foundComment = true
		}
	}

	if !foundRemoveBlocked {
		t.Error("expected DELETE blocked label for issue #15")
	}
	if !foundAddReady {
		t.Error("expected POST ready label for issue #15")
	}
	if !foundComment {
		t.Error("expected POST comment for issue #15")
	}

	fmt.Printf("✅ Scheduler unblocked issue #15: remove_blocked=%v add_ready=%v comment=%v\n",
		foundRemoveBlocked, foundAddReady, foundComment)
}

func TestParseDependsOn(t *testing.T) {
	cases := []struct {
		body string
		want []int
	}{
		{"Depends on: #15", []int{15}},
		{"depends on: #15, #16", []int{15, 16}},
		{"DEPENDS ON #15, #16, #17", []int{15, 16, 17}},
		{"Depends on: #15\nSee also #16", []int{15}},
		{"No deps here.", nil},
	}

	for _, tc := range cases {
		got := parseDependsOn(tc.body)
		if len(got) != len(tc.want) {
			t.Errorf("parseDependsOn(%q) = %v, want %v", tc.body, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseDependsOn(%q)[%d] = %d, want %d", tc.body, i, got[i], tc.want[i])
			}
		}
	}
	fmt.Println("✅ parseDependsOn tests passed")
}
