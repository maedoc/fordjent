package scaffold

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fordjent/fordjent/internal/forgejo"
)

func TestCheckAndBlock_EmptyRepo(t *testing.T) {
	// Simulates a repo with no branches (sha not found)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/test/repo/git/trees/main", func(w http.ResponseWriter, r *http.Request) {
		// Empty repo — no main branch
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"sha not found [main]"}`))
	})
	mux.HandleFunc("/api/v1/repos/test/repo/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v1/repos/test/repo/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1,"number":1,"title":"[scaffold] Set up project structure"}`))
	})
	mux.HandleFunc("/api/v1/repos/test/repo/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v1/repos/test/repo/issues/2/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v1/repos/test/repo/issues/2/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := forgejo.NewClient(srv.URL, "test-token")

	blocked, err := CheckAndBlock(context.Background(), client, "test/repo", 2, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !blocked {
		t.Error("empty repo should block the issue")
	}
}

func TestCheckAndBlock_PopulatedGoRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/test/repo/git/trees/main", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"tree":[{"path":"go.mod","type":"blob"},{"path":"README.md","type":"blob"}]}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := forgejo.NewClient(srv.URL, "test-token")

	blocked, err := CheckAndBlock(context.Background(), client, "test/repo", 3, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked {
		t.Error("populated Go repo with go.mod and README.md should not block")
	}
}

func TestCheckAndBlock_PopulatedPythonRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/test/repo/git/trees/main", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"tree":[{"path":"requirements.txt","type":"blob"},{"path":"README.md","type":"blob"},{"path":"Snakefile","type":"blob"}]}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := forgejo.NewClient(srv.URL, "test-token")

	blocked, err := CheckAndBlock(context.Background(), client, "test/repo", 3, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked {
		t.Error("populated Python repo with requirements.txt and README.md should not block")
	}
}

func TestCheckAndBlock_NilClient(t *testing.T) {
	blocked, err := CheckAndBlock(context.Background(), nil, "test/repo", 1, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked {
		t.Error("nil client should not block")
	}
}

func TestDetectProjectLang(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		expected string
	}{
		{"go_mod", []string{"go.mod", "README.md", "main.go"}, "go"},
		{"python_requirements", []string{"requirements.txt", "README.md", "app.py"}, "python"},
		{"python_pyproject", []string{"pyproject.toml", "README.md"}, "python"},
		{"rust", []string{"Cargo.toml", "README.md"}, "rust"},
		{"javascript", []string{"package.json", "README.md"}, "javascript"},
		{"java_maven", []string{"pom.xml", "README.md"}, "java"},
		{"ruby", []string{"Gemfile", "README.md"}, "ruby"},
		{"php", []string{"composer.json", "README.md"}, "php"},
		{"py_files_no_manifest", []string{"README.md", "app.py", "utils.py"}, "python"},
		{"go_files_no_manifest", []string{"README.md", "main.go"}, "go"},
		{"empty", []string{}, "unknown"},
		{"only_readme", []string{"README.md"}, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectProjectLang(tt.files)
			if got != tt.expected {
				t.Errorf("detectProjectLang(%v) = %q, want %q", tt.files, got, tt.expected)
			}
		})
	}
}

func TestIsRepoPopulated(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		lang    string
		populated bool
	}{
		{"go_with_mod_and_readme", []string{"go.mod", "README.md"}, "go", true},
		{"go_with_mod_no_readme", []string{"go.mod"}, "go", false},
		{"python_with_req_and_readme", []string{"requirements.txt", "README.md"}, "python", true},
		{"python_with_pyproject_and_readme", []string{"pyproject.toml", "README.md"}, "python", true},
		{"python_with_only_readme", []string{"README.md"}, "python", false},
		{"unknown_with_3_files_and_readme", []string{"README.md", "config.yaml", "data.csv"}, "unknown", true},
		{"unknown_with_2_files_and_readme", []string{"README.md", "config.yaml"}, "unknown", false},
		{"empty", []string{}, "unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRepoPopulated(tt.files, tt.lang)
			if got != tt.populated {
				t.Errorf("isRepoPopulated(%v, %q) = %v, want %v", tt.files, tt.lang, got, tt.populated)
			}
		})
	}
}

func TestScaffoldIssueContent(t *testing.T) {
	tests := []struct {
		lang       string
		wantGo     bool
		wantPython bool
		wantGeneric bool
	}{
		{"go", true, false, false},
		{"python", false, true, false},
		{"rust", false, false, false},
		{"unknown", false, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.lang, func(t *testing.T) {
			title, body := scaffoldIssueContent(tt.lang, 1)
			if tt.wantGo && !contains(title, "Go") {
				t.Errorf("expected Go title for lang=%q, got %q", tt.lang, title)
			}
			if tt.wantPython && !contains(body, "requirements.txt") {
				t.Errorf("expected Python scaffolding in body for lang=%q, got %q", tt.lang, body)
			}
			if tt.wantGeneric && !contains(body, "Look at other open issues") {
				t.Errorf("expected generic scaffolding hint for lang=%q, got %q", tt.lang, body)
			}
		})
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}