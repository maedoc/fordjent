package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/fordjent/fordjent/internal/sentinel"
)

// RetryPolicy defines when and how to retry LLM requests.
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

// DefaultRetryPolicy returns a sensible default retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries: 3,
		BaseDelay:  2 * time.Second,
		MaxDelay:   30 * time.Second,
	}
}

// IsRetryable returns true if the error should trigger a retry.
func (r RetryPolicy) IsRetryable(err error, statusCode int) bool {
	if err == nil {
		return false
	}

	// Check typed sentinel errors first
	if sentinel.IsRetryable(err) {
		return true
	}

	// Explicit HTTP status codes
	if statusCode == http.StatusTooManyRequests || // 429
		statusCode == http.StatusServiceUnavailable || // 503
		statusCode == http.StatusBadGateway || // 502
		statusCode == 529 { // Cloudflare/Overloaded
		return true
	}

	// Client errors are NOT retryable
	if statusCode >= 400 && statusCode < 500 && statusCode != http.StatusTooManyRequests {
		return false
	}

	// Context deadline / timeout errors are retryable
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return false
}

// Backoff returns the delay for the given attempt (0-indexed) with jitter.
func (r RetryPolicy) Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := r.BaseDelay * time.Duration(1<<attempt) // 2^attempt
	if delay > r.MaxDelay || delay <= 0 {
		delay = r.MaxDelay
	}
	// Add jitter: ±25% of delay
	jitter := time.Duration(rand.Int63n(int64(delay)/2)) - delay/4
	return delay + jitter
}

// RetryError aggregates all attempts into a single error.
type RetryError struct {
	Attempts int
	Errors   []error
	LastErr  error
}

func (e *RetryError) Error() string {
	if e.LastErr != nil {
		return fmt.Sprintf("LLM request failed after %d attempts: %v", e.Attempts, e.LastErr)
	}
	return fmt.Sprintf("LLM request failed after %d attempts", e.Attempts)
}

// Retry executes the given function with exponential backoff.
// It returns the result of the successful call or a *RetryError.
func (r RetryPolicy) Retry(ctx context.Context, fn func() error) error {
	var retryErr = &RetryError{}

	for attempt := 0; attempt <= r.MaxRetries; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}

		retryErr.Attempts = attempt + 1
		retryErr.Errors = append(retryErr.Errors, err)
		retryErr.LastErr = err

		// Don't retry on the last attempt
		if attempt >= r.MaxRetries {
			break
		}

		// Check if error is retryable
		var httpErr *HTTPError
		statusCode := 0
		if errors.As(err, &httpErr) {
			statusCode = httpErr.StatusCode
		}
		if !r.IsRetryable(err, statusCode) {
			slog.Debug("non-retryable error, aborting", "error", err, "status", statusCode)
			return err // Return as-is, not wrapped in RetryError
		}

		delay := r.Backoff(attempt)
		slog.Warn("retrying LLM request",
			"attempt", attempt+1,
			"max_retries", r.MaxRetries,
			"delay", delay,
			"error", err,
		)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	return retryErr
}

// HTTPError wraps an HTTP status code with an error.
type HTTPError struct {
	StatusCode int
	Body       string
	Err        error
}

func (e *HTTPError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("HTTP %d: %s (underlying: %v)", e.StatusCode, e.Body, e.Err)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func (e *HTTPError) Unwrap() error { return e.Err }
