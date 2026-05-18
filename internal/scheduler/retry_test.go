package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/sentinel"
	"github.com/fordjent/fordjent/internal/tool"
)

func TestRetryDo_SuccessOnFirstAttempt(t *testing.T) {
	calls := 0
	result, err := retryDo(context.Background(), func() (string, error) {
		calls++
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected ok, got %s", result)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestRetryDo_SuccessAfterRetries(t *testing.T) {
	calls := 0
	result, err := retryDo(context.Background(), func() (string, error) {
		calls++
		if calls < 3 {
			return "", &sentinel.ErrAPIServer{StatusCode: 500, Body: "internal error"}
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected ok, got %s", result)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestRetryDo_Exhausted(t *testing.T) {
	calls := 0
	_, err := retryDo(context.Background(), func() (string, error) {
		calls++
		return "", &sentinel.ErrAPIServer{StatusCode: 500, Body: "internal error"}
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ree *RetryExhaustedError
	if !errors.As(err, &ree) {
		t.Fatalf("expected RetryExhaustedError, got %T: %v", err, err)
	}
	if ree.Attempts != retryMaxAttempts {
		t.Fatalf("expected %d attempts, got %d", retryMaxAttempts, ree.Attempts)
	}
	if calls != retryMaxAttempts {
		t.Fatalf("expected %d calls, got %d", retryMaxAttempts, calls)
	}
}

func TestRetryDo_NoRetryOnClientError(t *testing.T) {
	calls := 0
	_, err := retryDo(context.Background(), func() (string, error) {
		calls++
		return "", &sentinel.ErrAPIClient{StatusCode: 404, Body: "not found"}
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiClient *sentinel.ErrAPIClient
	if !errors.As(err, &apiClient) {
		t.Fatalf("expected ErrAPIClient, got %T: %v", err, err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry), got %d", calls)
	}
}

func TestRetryDo_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	_, err := retryDo(ctx, func() (string, error) {
		calls++
		return "", &sentinel.ErrAPIServer{StatusCode: 500, Body: "error"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call before cancel, got %d", calls)
	}
}

func TestRetryDo_ContextDeadlineExceededIsRetryable(t *testing.T) {
	calls := 0
	result, err := retryDo(context.Background(), func() (string, error) {
		calls++
		if calls < 2 {
			return "", context.DeadlineExceeded
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected ok, got %s", result)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestIsIssueClosed_RetriesOnServerError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/api/v1/repos/fjadmin/test/issues/5" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"state":"closed","merged":false}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	closed, err := s.isIssueClosed(context.Background(), "fjadmin/test", 5)
	if err != nil {
		t.Fatalf("isIssueClosed error: %v", err)
	}
	if !closed {
		t.Error("expected issue to be closed after retry")
	}
	if calls < 3 {
		t.Fatalf("expected at least 3 calls (2 failures + 1 success), got %d", calls)
	}
}

func TestIsIssueClosed_ExhaustedRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	_, err := s.isIssueClosed(context.Background(), "fjadmin/test", 5)
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	var ree *RetryExhaustedError
	if !errors.As(err, &ree) {
		t.Fatalf("expected RetryExhaustedError, got %T: %v", err, err)
	}
}

func TestIsIssueClosed_NoRetryOn404(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	adapter := tool.NewForgejoAdapter(server.URL, "test-token")
	s := New(adapter)

	_, err := s.isIssueClosed(context.Background(), "fjadmin/test", 999)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	var apiClient *sentinel.ErrAPIClient
	if !errors.As(err, &apiClient) {
		t.Fatalf("expected ErrAPIClient, got %T: %v", err, err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry on 4xx), got %d", calls)
	}
}

func TestScheduleRecheck(t *testing.T) {
	recheckCalled := false

	s := &Scheduler{
		recheckDelayForTest: 100 * time.Millisecond,
		recheckHook: func() { recheckCalled = true },
	}

	s.scheduleRecheck("fjadmin/test", 10)
	time.Sleep(200 * time.Millisecond)

	if !recheckCalled {
		t.Error("expected deferred re-check to fire")
	}
}
