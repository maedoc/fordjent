## 1. Transitive Dependency Cascade

- [x] 1.1 Add `maxCascadeRounds` field to `Scheduler` struct with default value 10; add `MaxCascadeRounds` to config struct in `internal/config/config.go` (spec: transitive-deps)
- [x] 1.2 Refactor `checkAndUnblock` to wrap the unblock-candidates loop in an outer cascade loop: after processing all candidates in one round, re-list open issues and re-scan for newly-satisfiable candidates until zero are unblocked or `maxCascadeRounds` is reached (spec: transitive-deps, requirement: Cascade unblocking)
- [x] 1.3 Add cascade round logging: each round logs round number and count of unblocked issues (spec: transitive-deps, requirement: Cascade round logging)
- [x] 1.4 Update `ReconcileBlocked` to call `checkAndUnblock` (with `mergedPRNumber=0`) instead of its own ad-hoc scan, so it gets cascade logic for free (spec: transitive-deps, requirement: ReconcileBlocked uses cascade logic)
- [x] 1.5 Add test: `TestCascadeDirectChain` — #10 merged → #20 depends on #10 → #30 depends on #20 → both unblocked in one invocation (spec: transitive-deps, scenario: Direct chain)
- [x] 1.6 Add test: `TestCascadeDiamond` — #10 merged → #20 and #30 depend on #10 → #40 depends on #20 and #30 → all three unblocked across two cascade rounds (spec: transitive-deps, scenario: Diamond dependency)
- [x] 1.7 Add test: `TestCascadeMaxRounds` — chain deeper than `maxCascadeRounds` → logs warning, remaining issues stay blocked (spec: transitive-deps, scenario: Cascade bounded by maxCascadeRounds)
- [x] 1.8 Add test: `TestCascadeWithCycle` — cycle in dependency graph → cyclic issues not unblocked, non-cyclic transitive dependents still unblocked (spec: transitive-deps, scenario: Cycle in dependency chain)

## 2. Restart Checkpoint

- [x] 2.1 Add `LastToolName` field to `TurnExecutor` in `internal/agent/turn.go`; update `RecordToolCall` to set it (spec: restart-checkpoint, requirement: Last tool tracking)
- [x] 2.2 Add `writeShutdownCheckpoint` function in `internal/lifecycle/lifecycle.go` or `internal/session/manager.go`: writes `shutdown.json` to sess.WorkDir with `{"state", "last_tool", "timestamp"}`; called from `runSession` on exit (spec: restart-checkpoint, requirement: Session exit checkpoint)
- [x] 2.3 Call `writeShutdownCheckpoint` from all `runSession` exit paths: normal completion, max-turns failure, error failure, context cancellation (spec: restart-checkpoint, scenarios: Session completes normally, hits max turns, cancelled by shutdownAll)
- [x] 2.4 Update `restoreSessions` in `internal/session/manager.go`: check for `shutdown.json` in rec.WorkDir; if `state: "completed"` → skip + delete store record; if `state: "cancelled"` → skip (don't restore); if absent (crash) → restore with verification steering (spec: restart-checkpoint, requirements: Restore skips completed sessions, Crash recovery verification steering)
- [x] 2.5 Add `VerificationSteering` field to `TurnExecutor`; when set, inject steering message on first turn: "The previous session was interrupted. Re-read any files you previously wrote..." (spec: restart-checkpoint, requirement: Crash recovery verification steering)
- [x] 2.6 Add test: `TestShutdownCheckpointWrites` — mock session exit, verify `shutdown.json` contents for completed/failed/cancelled states (spec: restart-checkpoint, scenarios: completes normally, hits max turns, cancelled by shutdownAll)
- [x] 2.7 Add test: `TestRestoreSkipsCompletedSession` — `shutdown.json` with `state: "completed"` → session not restored, store record deleted (spec: restart-checkpoint, scenario: Gracefully completed session not restored)
- [x] 2.8 Add test: `TestRestoreCrashRecoverySteering` — no `shutdown.json` + lifecycle `working` state → session restored with verification steering message on first turn (spec: restart-checkpoint, scenario: Agent verifies after crash)

## 3. Stale Cleanup

- [x] 3.1 Add `completed_at` column to `sessions` table in `internal/session/store.go`: `ALTER TABLE sessions ADD COLUMN completed_at DATETIME DEFAULT NULL`; update `Create` and `Update` methods (spec: stale-cleanup, requirement: completed_at field)
- [x] 3.2 Set `completed_at` when session transitions to completed/failed in lifecycle: add hook in `runSession` exit path that calls `m.store.SetCompletedAt(key, time.Now().UTC())` (spec: stale-cleanup, scenario: completed_at set on session completion)
- [x] 3.3 Add `m.store.Delete(key)` to `shutdownAll` in `internal/session/manager.go` alongside existing `delete(m.sessions, key)` (spec: stale-cleanup, requirement: Store record deletion on shutdown)
- [x] 3.4 Add `pruneMergedBranches` helper in `internal/session/manager.go`: runs `git branch --merged main` in a repo directory, deletes merged feature branches (excluding `main` and current branch); logs errors non-fatally (spec: stale-cleanup, requirement: Merged branch pruning)
- [x] 3.5 Call `pruneMergedBranches` from `cleanupOldWorkDirs` for each work directory being archived (spec: stale-cleanup, scenario: Prune merged branches during cleanup)
- [x] 3.6 Add branch pruning to the hourly scan for ALL session work directories (not just completed ones): if a session has been idle for >1 hour, prune its repo's merged branches (spec: stale-cleanup, requirement: Branch pruning on all work directory scans)
- [x] 3.7 Add test: `TestStoreDeleteOnShutdown` — `shutdownAll` removes records from `sessions.db`; restart does not restore them (spec: stale-cleanup, scenario: Graceful shutdown cleans store)
- [x] 3.8 Add test: `TestPruneMergedBranches` — create merged feature branch in test repo, call prune, verify branch deleted; unmerged branch preserved (spec: stale-cleanup, scenarios: Prune merged branches during cleanup, Active branch not pruned)
- [x] 3.9 Add test: `TestPruneBranchesFailureNonFatal` — corrupt repo directory → prune logs warning, cleanup continues (spec: stale-cleanup, scenario: Branch pruning failure is non-fatal)
- [x] 3.10 Add test: `TestCompletedAtColumn` — migrate existing DB, start session, complete it, verify `completed_at` is set; active session has NULL (spec: stale-cleanup, scenarios: completed_at set on session completion, completed_at is NULL for active sessions)
