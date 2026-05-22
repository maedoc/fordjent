package scaffold

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		w.Write([]byte(`{"id":1,"number":1,"title":"[scaffold] Add project scaffold"}`))
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

func TestCheckAndBlock_PopulatedRepo(t *testing.T) {
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
		t.Error("populated repo with go.mod and README.md should not block")
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