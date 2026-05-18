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
		case r.URL.Path == "/api/v1/repos/fjadmin/gogit/labels":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": float64(1), "name": "blocked"},
				{"id": float64(2), "name": "ready"},
			})
		case strings.HasSuffix(r.URL.Path, "/issues/10"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state": "closed",
			})
		case strings.HasSuffix(r.URL.Path, "/15/labels/1") && r.Method == "DELETE":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/15/labels") && r.Method == "POST":
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

	_, err := s.OnPRMerged(context.Background(), "fjadmin/gogit", 10)
	if err != nil {
		t.Fatalf("OnPRMerged error: %v", err)
	}

	var foundRemoveBlocked, foundAddReady, foundComment bool
	for _, c := range captured {
		if c.Method == "DELETE" && strings.Contains(c.Path, "15/labels/") {
			foundRemoveBlocked = true
		}
		if c.Method == "POST" && strings.Contains(c.Path, "15/labels") {
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

func TestIsIssueClosed_PMIssueNotBlocking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/4"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state":       "open",
				"merged":      false,
				"pull_request": nil,
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	closed, err := s.isIssueClosed(context.Background(), "fjadmin/testbed", 4)
	if err != nil {
		t.Fatalf("isIssueClosed error: %v", err)
	}
	if !closed {
		t.Error("PM issue with no PR should be treated as satisfied (not blocking)")
	}
}

func TestIsIssueClosed_OpenPRIsBlocking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/5"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state":  "open",
				"merged": false,
				"pull_request": map[string]interface{}{
					"url": "http://localhost:3000/api/v1/repos/fjadmin/testbed/pulls/3",
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	closed, err := s.isIssueClosed(context.Background(), "fjadmin/testbed", 5)
	if err != nil {
		t.Fatalf("isIssueClosed error: %v", err)
	}
	if closed {
		t.Error("open PR dependency should be treated as NOT satisfied (blocking)")
	}
}

func TestIsIssueClosed_MergedPRIsSatisfied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/5"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state":  "closed",
				"merged": true,
				"pull_request": map[string]interface{}{
					"url": "http://localhost:3000/api/v1/repos/fjadmin/testbed/pulls/3",
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	closed, err := s.isIssueClosed(context.Background(), "fjadmin/testbed", 5)
	if err != nil {
		t.Fatalf("isIssueClosed error: %v", err)
	}
	if !closed {
		t.Error("merged PR dependency should be treated as satisfied")
	}
}

func TestPMReactivate_AllSubIssuesComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/repos/fjadmin/gogit/issues":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number": 5,
					"title":  "[pm] Implement auth system",
					"body":   "Decompose auth work.\n\nDepends on: #10, #11",
					"state":  "open",
					"labels": []map[string]interface{}{
						{"name": "role:pm"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/issues/10"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state":       "closed",
				"merged":      true,
				"pull_request": nil,
			})
		case strings.HasSuffix(r.URL.Path, "/issues/11"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state":       "closed",
				"merged":      true,
				"pull_request": nil,
			})
		case r.URL.Path == "/api/v1/repos/fjadmin/gogit/labels":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": float64(1), "name": "blocked"},
				{"id": float64(2), "name": "ready"},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	issues := []Issue{
		{Number: 5, Title: "[pm] Implement auth system", Body: "Depends on: #10, #11", State: "open", Labels: []Label{{Name: "role:pm"}}},
	}

	results := s.CheckPMReactivation(context.Background(), "fjadmin/gogit", 10, issues)
	if len(results) != 1 {
		t.Fatalf("expected 1 PM reactivation result, got %d", len(results))
	}
	if results[0].ParentIssueNumber != 5 {
		t.Errorf("expected parent issue #5, got #%d", results[0].ParentIssueNumber)
	}
	if results[0].TriggeringIssue != 10 {
		t.Errorf("expected triggering issue #10, got #%d", results[0].TriggeringIssue)
	}
	fmt.Println("✅ PM reactivation emitted when all sub-issues complete")
}

func TestPMReactivate_SomeSubIssuesStillOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/10"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state":       "closed",
				"merged":      true,
				"pull_request": nil,
			})
		case strings.HasSuffix(r.URL.Path, "/issues/11"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state":  "open",
				"merged": false,
				"pull_request": map[string]interface{}{
					"url": "http://localhost:3000/api/v1/repos/fjadmin/gogit/pulls/11",
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	issues := []Issue{
		{Number: 5, Title: "[pm] Implement auth system", Body: "Depends on: #10, #11", State: "open", Labels: []Label{{Name: "role:pm"}}},
	}

	results := s.CheckPMReactivation(context.Background(), "fjadmin/gogit", 10, issues)
	if len(results) != 0 {
		t.Errorf("expected 0 PM reactivation results (sub-issue #11 still open), got %d", len(results))
	}
	fmt.Println("✅ PM reactivation NOT emitted when some sub-issues still open")
}

func TestPMReactivate_NonPMParent(t *testing.T) {
	adapter := tool.NewForgejoAdapter("http://localhost:9999", "test-token")
	s := New(adapter)

	issues := []Issue{
		{Number: 5, Title: "Implement auth system", Body: "Depends on: #10, #11", State: "open", Labels: []Label{}},
	}

	results := s.CheckPMReactivation(context.Background(), "fjadmin/gogit", 10, issues)
	if len(results) != 0 {
		t.Errorf("expected 0 PM reactivation results for non-PM parent, got %d", len(results))
	}
	fmt.Println("✅ PM reactivation NOT emitted for non-PM parent issues")
}

func TestIsPMIssue(t *testing.T) {
	cases := []struct {
		title  string
		labels []Label
		want   bool
	}{
		{"[pm] Do the thing", nil, true},
		{"[decompose] Break it down", nil, true},
		{"[PM] Uppercase tag", nil, true},
		{"Regular issue", nil, false},
		{"Implement feature", []Label{{Name: "role:pm"}}, true},
		{"Implement feature", []Label{{Name: "role:project-manager"}}, true},
		{"Implement feature", []Label{{Name: "role:implementer"}}, false},
	}

	for _, tc := range cases {
		issue := Issue{Title: tc.title, Labels: tc.labels}
		got := isPMIssue(issue)
		if got != tc.want {
			t.Errorf("isPMIssue(%q, %v) = %v, want %v", tc.title, tc.labels, got, tc.want)
		}
	}
	fmt.Println("✅ isPMIssue tests passed")
}
