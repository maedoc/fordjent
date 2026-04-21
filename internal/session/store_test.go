package session

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStore_CRUD(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Second)
	rec := &SessionRecord{
		SessionKey:  "org/repo/issues/42",
		Repository:  "org/repo",
		IssueNumber: 42,
		PRNumber:    0,
		WorkDir:     "/tmp/work/42",
		RepoDir:     "/tmp/work/42/repo",
		CreatedAt:   now,
		LastActive:  now,
	}

	if err := store.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(rec.SessionKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SessionKey != rec.SessionKey {
		t.Errorf("session key: got %q, want %q", got.SessionKey, rec.SessionKey)
	}
	if got.Repository != rec.Repository {
		t.Errorf("repository: got %q, want %q", got.Repository, rec.Repository)
	}
	if got.IssueNumber != rec.IssueNumber {
		t.Errorf("issue_number: got %d, want %d", got.IssueNumber, rec.IssueNumber)
	}
	if got.PRNumber != rec.PRNumber {
		t.Errorf("pr_number: got %d, want %d", got.PRNumber, rec.PRNumber)
	}
	if got.WorkDir != rec.WorkDir {
		t.Errorf("work_dir: got %q, want %q", got.WorkDir, rec.WorkDir)
	}
	if got.RepoDir != rec.RepoDir {
		t.Errorf("repo_dir: got %q, want %q", got.RepoDir, rec.RepoDir)
	}
	if !got.CreatedAt.Equal(rec.CreatedAt) {
		t.Errorf("created_at: got %v, want %v", got.CreatedAt, rec.CreatedAt)
	}
	if !got.LastActive.Equal(rec.LastActive) {
		t.Errorf("last_active: got %v, want %v", got.LastActive, rec.LastActive)
	}

	newTime := now.Add(1 * time.Hour)
	if err := store.UpdateLastActive(rec.SessionKey, newTime); err != nil {
		t.Fatalf("update last_active: %v", err)
	}

	got, err = store.Get(rec.SessionKey)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if !got.LastActive.Equal(newTime.UTC().Truncate(time.Second)) {
		t.Errorf("last_active after update: got %v, want %v", got.LastActive, newTime.UTC().Truncate(time.Second))
	}

	if err := store.Delete(rec.SessionKey); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err = store.Get(rec.SessionKey)
	if err == nil {
		t.Fatal("expected error after delete, got nil")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestStore_ListAll(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	base := time.Now().UTC().Truncate(time.Second)
	recs := []SessionRecord{
		{SessionKey: "key-a", Repository: "org/a", IssueNumber: 1, WorkDir: "/tmp/a", RepoDir: "/tmp/a/repo", CreatedAt: base, LastActive: base},
		{SessionKey: "key-b", Repository: "org/b", IssueNumber: 2, WorkDir: "/tmp/b", RepoDir: "/tmp/b/repo", CreatedAt: base, LastActive: base.Add(2 * time.Hour)},
		{SessionKey: "key-c", Repository: "org/c", IssueNumber: 3, WorkDir: "/tmp/c", RepoDir: "/tmp/c/repo", CreatedAt: base, LastActive: base.Add(1 * time.Hour)},
	}

	for i := range recs {
		if err := store.Create(&recs[i]); err != nil {
			t.Fatalf("create record %d: %v", i, err)
		}
	}

	list, err := store.ListAll()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 records, got %d", len(list))
	}

	// Ordered by last_active DESC: key-b (base+2h), key-c (base+1h), key-a (base)
	expectedOrder := []string{"key-b", "key-c", "key-a"}
	for i, want := range expectedOrder {
		if list[i].SessionKey != want {
			t.Errorf("position %d: got %q, want %q", i, list[i].SessionKey, want)
		}
	}
}

func TestStore_DeleteOlderThan(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	base := time.Now().UTC().Truncate(time.Second)
	recs := []SessionRecord{
		{SessionKey: "old-1", Repository: "org/o1", IssueNumber: 1, WorkDir: "/tmp/o1", RepoDir: "/tmp/o1/repo", CreatedAt: base.Add(-2 * time.Hour), LastActive: base.Add(-2 * time.Hour)},
		{SessionKey: "old-2", Repository: "org/o2", IssueNumber: 2, WorkDir: "/tmp/o2", RepoDir: "/tmp/o2/repo", CreatedAt: base.Add(-1 * time.Hour), LastActive: base.Add(-1 * time.Hour)},
		{SessionKey: "new-1", Repository: "org/n1", IssueNumber: 3, WorkDir: "/tmp/n1", RepoDir: "/tmp/n1/repo", CreatedAt: base, LastActive: base},
	}

	for i := range recs {
		if err := store.Create(&recs[i]); err != nil {
			t.Fatalf("create record %d: %v", i, err)
		}
	}

	cutoff := base.Add(-30 * time.Minute)
	deleted, err := store.DeleteOlderThan(cutoff)
	if err != nil {
		t.Fatalf("delete older than: %v", err)
	}
	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}

	list, err := store.ListAll()
	if err != nil {
		t.Fatalf("list all after delete: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 record remaining, got %d", len(list))
	}
	if list[0].SessionKey != "new-1" {
		t.Errorf("expected remaining key new-1, got %q", list[0].SessionKey)
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	base := time.Now().UTC().Truncate(time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rec := SessionRecord{
				SessionKey:  fmt.Sprintf("concurrent-key-%d", n),
				Repository:  "org/repo",
				IssueNumber: n,
				WorkDir:     "/tmp/w",
				RepoDir:     "/tmp/w/repo",
				CreatedAt:   base,
				LastActive:  base,
			}
			if err := store.Create(&rec); err != nil {
				t.Errorf("create goroutine %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	list, err := store.ListAll()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(list) != 10 {
		t.Fatalf("expected 10 records, got %d", len(list))
	}

	seen := make(map[string]bool)
	for _, r := range list {
		seen[r.SessionKey] = true
	}
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("concurrent-key-%d", i)
		if !seen[key] {
			t.Errorf("missing key %q", key)
		}
	}
}
