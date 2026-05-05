// Package sentinel provides sentinel errors shared across packages.
package sentinel

import "errors"

// ErrBlocked is returned by the merge-queue or stale-gate when a PR
// creation is blocked by conflicting open work.
var ErrBlocked = errors.New("blocked by merge queue")

// ErrMaxTurnsReached is returned when a session exhausts its turn budget.
// Also defined in internal/agent to avoid import cycles; both point to the
// same value in normal operation. Kept here for cross-package use.
var ErrMaxTurnsReached = errors.New("max turns reached")
