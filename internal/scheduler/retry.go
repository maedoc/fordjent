package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/fordjent/fordjent/internal/sentinel"
)

const (
	retryMaxAttempts = 3
	retryBaseDelay   = 500 * time.Millisecond
	retryMaxDelay    = 2 * time.Second
	recheckDelay     = 5 * time.Minute
)

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if sentinel.IsRetryable(err) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if sentinel.IsClientError(err) {
		return false
	}
	return true
}

func backoff(attempt int) time.Duration {
	delay := retryBaseDelay * time.Duration(1<<attempt)
	if delay > retryMaxDelay || delay <= 0 {
		delay = retryMaxDelay
	}
	jitter := time.Duration(rand.Int63n(int64(delay)/2)) - delay/4
	return delay + jitter
}

func retryDo[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var lastErr error
	var zero T

	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}

		lastErr = err

		if !isRetryable(err) {
			return zero, err
		}

		if attempt < retryMaxAttempts-1 {
			delay := backoff(attempt)
			slog.Warn("scheduler: retrying API call",
				"attempt", attempt+1,
				"max_attempts", retryMaxAttempts,
				"delay", delay,
				"error", err,
			)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return zero, ctx.Err()
			case <-timer.C:
			}
		}
	}

	return zero, &RetryExhaustedError{Attempts: retryMaxAttempts, LastErr: lastErr}
}

type RetryExhaustedError struct {
	Attempts int
	LastErr  error
}

func (e *RetryExhaustedError) Error() string {
	if e.LastErr != nil {
		return fmt.Sprintf("scheduler API call failed after %d attempts: %v", e.Attempts, e.LastErr)
	}
	return fmt.Sprintf("scheduler API call failed after %d attempts", e.Attempts)
}

func (e *RetryExhaustedError) Unwrap() error { return e.LastErr }

func (s *Scheduler) scheduleRecheck(repo string, mergedPRNumber int) {
	delay := recheckDelay
	if s.recheckDelayForTest > 0 {
		delay = s.recheckDelayForTest
	}
	slog.Info("scheduler: scheduling deferred re-check", "repo", repo, "merged_pr", mergedPRNumber, "delay", delay)
	time.AfterFunc(delay, func() {
		if s.recheckHook != nil {
			s.recheckHook()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.checkAndUnblock(ctx, repo, mergedPRNumber); err != nil {
			slog.Warn("scheduler: deferred re-check failed", "error", err, "repo", repo)
		}
	})
}
