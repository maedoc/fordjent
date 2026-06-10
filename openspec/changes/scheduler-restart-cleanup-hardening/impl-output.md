# Implementation Output: scheduler-restart-cleanup-hardening

## Summary

Implemented all 26 tasks for the scheduler-restart-cleanup-hardening OpenSpec change.

### Group 1: Transitive Dependency Cascade (Tasks 1.1-1.8)

**Changes:**
- Added `maxCascadeRounds` field to `Scheduler` struct (default 10) with `SetMaxCascadeRounds()` setter
- Added `MaxCascadeRounds` to `AgentConfig` in config.go (default 10)
- Refactored `checkAndUnblock()` to wrap the candidate scanning + unblocking in an outer cascade loop that re-lists issues each round until zero are unblocked or `maxCascadeRounds` is reached
- Added cascade round logging: `scheduler: cascade round unblocked issues` with round number, count, and issue numbers
- Updated `ReconcileBlocked()` to delegate to `checkAndUnblock(ctx, repo, 0)` so it gets cascade logic for free
- Added cascade tests: `TestCascadeDirectChain`, `TestCascadeDiamond`, `TestCascadeMaxRounds`, `TestCascadeWithCycle`

### Group 2: Restart Checkpoint (Tasks 2.1-2.8)

**Changes:**
- Added `lastToolName` and `verificationSteering` fields to `TurnExecutor`
- Added `LastToolName()`, `SetVerificationSteering()` methods to `TurnExecutor`
- Updated `RecordToolCall()` to set `lastToolName`
- Added `LastToolName()`, `SetVerificationSteering()` methods to `Agent`
- Added `writeShutdownCheckpoint()`, `readShutdownCheckpoint()`, `deleteShutdownCheckpoint()` functions in manager.go
- Added `shutdownCheckpoint` struct with JSON serialization (state, last_tool, timestamp)
- Modified `runSession()` to call `writeShutdownCheckpoint()` from all exit paths: ctx.Done (cancelled/failed), OnSessionBlocked, OnSessionFailedMaxTurns, OnSessionFailedError, OnSessionComplete
- Modified `restoreSessions()` to check `shutdown.json`: completed → skip + delete store record; cancelled → skip + delete store record; failed → continue (with cleanup); absent → crash recovery with `IsCrashRecovery = true`
- Added `IsCrashRecovery` field to `Session` struct
- Wired `IsCrashRecovery` to `agt.SetVerificationSteering(true)` in `runSession()`
- Added verification steering injection in `TurnExecutor.Run()` on first turn
- Modified `shutdownAll()` to write "cancelled" checkpoint and delete store records
- Added tests: `TestShutdownCheckpointWrites`, `TestRestoreSkipsCompletedSession`, `TestRestoreCrashRecoverySteering`

### Group 3: Stale Cleanup (Tasks 3.1-3.10)

**Changes:**
- Added `completed_at` column (nullable TEXT) to `sessions` table with migration for existing DBs
- Added `CompletedAt *time.Time` field to `SessionRecord`
- Updated `Create()`, `Get()`, `ListAll()` to handle `completed_at`
- Added `SetCompletedAt()` method to Store
- Set `completed_at` via `m.store.SetCompletedAt()` on session completion and failure in `runSession()`
- Added `m.store.Delete(key)` in `shutdownAll()` alongside `delete(m.sessions, key)`
- Added `pruneMergedBranches()` helper that runs `git branch --merged main` and deletes merged feature branches (excluding main and current branch)
- Called `pruneMergedBranches()` from `cleanupOldWorkDirs()` for each work directory being archived
- Added branch pruning for ALL session work directories idle >1 hour in hourly cleanup scan
- Added tests: `TestStoreDeleteOnShutdown`, `TestPruneMergedBranches`, `TestPruneBranchesFailureNonFatal`, `TestCompletedAtColumn`

## Test Results

All 4 packages pass:
- `internal/scheduler` — ok
- `internal/lifecycle` — ok
- `internal/session` — ok
- `internal/agent` — ok

`go vet` — clean (no output)
