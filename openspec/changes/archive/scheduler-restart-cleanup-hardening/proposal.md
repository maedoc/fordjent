## Why

Three gaps in Fordjent's reliability layer cause delayed dependency resolution, wasted tokens on restart, and unbounded disk growth. Currently, the scheduler only checks direct dependencies per merge event (transitive chains wait for the next event or 2-hour reconciliation tick). On restart, agents may trust stale memory.jsonl entries for partially-completed tool calls. Feature branches and work directories accumulate indefinitely because cleanup only archives 7-day-old completed sessions and never prunes merged branches.

## What Changes

- **Transitive dependency cascade**: After unblocking an issue in `checkAndUnblock`, immediately re-scan remaining candidates to see if the newly-unblocked issue satisfies further dependents. Loop until no more candidates can be unblocked in a single pass.
- **Restart checkpoint**: Write `shutdown.json` to the work directory on session exit. On restore, skip cleanly-completed sessions and inject a verification steering message for crash-recovered sessions ("Re-read any files you previously wrote to verify they are correct").
- **Aggressive cleanup**: Delete session records from `sessions.db` in `shutdownAll`. Add merged-branch pruning to `cleanupOldWorkDirs`. Add a per-session `completed_at` field to the store for more precise cleanup heuristics.

## Capabilities

### New Capabilities
- `transitive-deps`: Iterative unblocking in the scheduler so a single PR merge cascades through the full dependency graph in one pass
- `restart-checkpoint`: Session exit checkpoint and crash-recovery steering to prevent stale-memory hallucinations after restart
- `stale-cleanup`: Merged branch pruning, store record cleanup on shutdown, and `completed_at` tracking for work directory lifecycle

### Modified Capabilities

## Impact

- `internal/scheduler/scheduler.go` — `checkAndUnblock` gains iterative cascade loop; `ReconcileBlocked` reuses the same logic
- `internal/scheduler/scheduler_test.go` — new test cases for transitive chains (A→B→C), self-limiting loops, cycle + transitive interaction
- `internal/session/manager.go` — `shutdownAll` deletes store records; `restoreSessions` checks `shutdown.json` and injects verification steering; `cleanupOldWorkDirs` prunes merged branches
- `internal/session/store.go` — add `completed_at` column; migration for existing DBs
- `internal/lifecycle/lifecycle.go` — `runSession` writes `shutdown.json` on exit
