// Package sentinel provides typed sentinel errors shared across packages.
// These replace fragile strings.Contains(err.Error(), ...) checks with
// proper errors.Is() comparisons.
package sentinel

import (
	"errors"
	"fmt"
)

// --- Session lifecycle errors ---

// ErrBlocked is returned by the merge-queue or stale-gate when a PR
// creation is blocked by conflicting open work.
var ErrBlocked = errors.New("blocked by merge queue")

// ErrMaxTurnsReached is returned when a session exhausts its turn budget.
var ErrMaxTurnsReached = errors.New("max turns reached")

// ErrSessionTimeout is returned when a session exceeds its time limit.
var ErrSessionTimeout = errors.New("session timed out")

// --- Git / PR creation errors ---

// ErrBadRevision is returned when Forgejo reports a missing or invalid
// git revision (branch not found, bad ref, etc.).
var ErrBadRevision = errors.New("bad revision")

// ErrBranchNotFound is returned when a pushed branch is not visible on
// the remote after multiple retries.
var ErrBranchNotFound = errors.New("branch not found on remote")

// ErrStaleBranch is returned by stalegate when a feature branch is
// behind the base and auto-rebase failed.
var ErrStaleBranch = errors.New("stale branch")

// ErrVerifyFailed is returned when go build or go test fails before
// PR creation or after commit.
var ErrVerifyFailed = errors.New("verify gate failed")

// --- API errors ---

// ErrAlreadyExists is returned when a resource (label, issue, etc.)
// already exists.
var ErrAlreadyExists = errors.New("resource already exists")

// ErrAPIClient is returned for 4xx client errors (400, 401, 403, 422).
type ErrAPIClient struct {
	StatusCode int
	Body       string
}

func (e *ErrAPIClient) Error() string {
	return fmt.Sprintf("API client error %d: %s", e.StatusCode, e.Body)
}

// ErrAPIServer is returned for 5xx server errors (500, 502, 503, 529).
type ErrAPIServer struct {
	StatusCode int
	Body       string
}

func (e *ErrAPIServer) Error() string {
	return fmt.Sprintf("API server error %d: %s", e.StatusCode, e.Body)
}

// ErrNoRemoteRef is returned when git can't find a remote ref
// (e.g., empty repo with no main branch yet).
var ErrNoRemoteRef = errors.New("remote ref not found")

// --- Helper predicates ---

// IsRetryable returns true if the error is transient and worth retrying.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var apiServer *ErrAPIServer
	if errors.As(err, &apiServer) {
		return true
	}
	if errors.Is(err, ErrBadRevision) {
		return true // branch indexing lag
	}
	return false
}

// IsClientError returns true if the error is a 4xx API error (not retryable).
func IsClientError(err error) bool {
	var apiClient *ErrAPIClient
	return errors.As(err, &apiClient)
}
