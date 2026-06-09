## Context

Fordjent's reliability layer has three gaps that compound under realistic usage:

1. **Scheduler**: `checkAndUnblock` (in `internal/scheduler/scheduler.go`) does a single pass over all open issues after a PR merge. If issue #20 depends on #15, and #15 was just unblocked (its own dependency #10 was satisfied by the merge), #20 remains blocked until the next PR merge event or the 2-hour `ReconcileBlocked` ticker. The scheduler already builds the full dependency graph in `detectCircularDeps` but doesn't use it for iterative unblocking.

2. **Restart**: `restoreSessions` (in `internal/session/manager.go`, line 234) re-spawns goroutines for sessions that were `working` when the process died. It reads `memory.jsonl` to inject prior context, but has no way to distinguish a cleanly-completed session from a crash. A 12B model that sees "I wrote main.go" in its memory may skip re-reading the file, even if the write was interrupted mid-execution.

3. **Cleanup**: `shutdownAll` (line 1595) only does `sess.Cancel()` + `delete(m.sessions, key)` — it doesn't delete from `sessions.db`, so stale records survive across restarts. `cleanupOldWorkDirs` (line 1524) archives and deletes work directories for sessions older than 7 days but never prunes merged feature branches. A long-running instance accumulates branches proportional to session count.

Current data flow: PR merge → `OnPRMerged` → `checkAndUnblock` (single pass) → label changes → webhook → `handleEvent`. Each step is event-driven with no cascade between steps.

## Goals / Non-Goals

**Goals:**
- Single PR merge cascades through the full dependency graph in one pass (no waiting for next event)
- Agents on restart verify previously-written files instead of trusting stale memory
- Merged feature branches pruned during periodic cleanup; session store records deleted on graceful shutdown
- All three features tested with edge cases (cycles, partial writes, missing dirs)

**Non-Goals:**
- Auto-merge after all dependencies resolve (that's the automerge system, not the scheduler)
- Summarization-based compaction of memory.jsonl (truncate-only compaction remains)
- OS-level sandboxing for bash scope enforcement (regex + symlink resolution remains the approach)
- Persistent event queue for exactly-once processing across restarts

## Decisions

### D1: Iterative cascade in checkAndUnblock (not new graph traversal API)

**Choice**: Add a loop to `checkAndUnblock` that re-scans remaining candidates after each unblock, continuing until no more candidates can be satisfied.

**Alternative**: Build a topological sort of the dependency graph and unblock in order. This would be more efficient (O(V+E) vs O(V²)) but the existing `checkAndUnblock` logic is complex (it handles priority ordering, label manipulation, native dependencies API, retry exhaustion). Wrapping the existing logic in a loop is simpler and less risky than replacing it.

**Rationale**: V is typically 10-30 issues per repo. O(V²) is acceptable. The loop is naturally bounded by V (each iteration unblocks at least one issue, or the loop terminates). Cycle detection already runs before the loop, so infinite loops are impossible.

### D2: shutdown.json checkpoint (not in sessions.db)

**Choice**: Write a `shutdown.json` file to the session's work directory on exit, recording `{"state": "completed|failed|cancelled", "last_tool": "...", "timestamp": "..."}`.

**Alternative**: Add a `shutdown_state` column to `sessions.db`. But the session store is deliberately minimal (7 columns) and adding shutdown state mixes lifecycle concerns into the store layer. The lifecycle DB already tracks state transitions with timestamps — the `shutdown.json` is complementary (it captures the *last tool call* for steering purposes, which the lifecycle DB doesn't track).

**Rationale**: `shutdown.json` lives next to `memory.jsonl` in the work directory, making it easy for `restoreSessions` to check. On crash, the file is missing (couldn't be written) or has `state: "cancelled"` (context was cancelled). On clean completion, it has `state: "completed"`.

### D3: Verification steering on crash recovery (not automatic re-execution)

**Choice**: On restore, if `shutdown.json` is missing or has `state: "cancelled"`, inject a user-role steering message: "The previous session was interrupted. Re-read any files you previously wrote to verify they are correct before continuing."

**Alternative**: Automatically re-execute the last tool call. This is dangerous (side effects like `git push`, `forgejo_create_pr` must not be auto-repeated) and complex (need to track which tool calls are idempotent).

**Rationale**: The 12B model responds well to explicit steering messages (proven in bug-reproduce steering experiments). A "verify before proceeding" message is safe and effective.

### D4: Merged branch pruning in cleanupOldWorkDirs (not in OnPRMerged)

**Choice**: Add branch pruning to the existing hourly `cleanupOldWorkDirs` ticker, not to the PR merge event handler.

**Alternative**: Prune branches immediately when a PR merges. But the repo clone is inside a session's work directory — multiple sessions may have different clones with different branch states. The merge happens via Forgejo's API, not locally. Pruning on merge would require finding all relevant work directories and running git commands in each.

**Rationale**: Periodic cleanup is simpler and less error-prone. Branches are harmless until pruned. `git branch --merged main` is safe and idempotent. Run it in the work directory during the hourly cleanup pass.

### D5: store.Delete in shutdownAll (not just in-memory cleanup)

**Choice**: Add `m.store.Delete(key)` to `shutdownAll` alongside the existing `delete(m.sessions, key)`.

**Rationale**: Without this, `restoreSessions` on next start will find stale `working` records and try to re-activate sessions that were cleanly shut down. The lifecycle DB already has the final state, but `restoreSessions` checks the lifecycle DB — so the stale store records just create unnecessary work. Deleting them is the right cleanup.

## Risks / Trade-offs

**[Cascade loop could be slow with 100+ issues]** → The loop is bounded by V (each iteration unblocks ≥1 issue). Rust mitigation: add a `maxCascadeRounds` cap (default 10) so pathological repos don't block the event handler.

**[shutdown.json write could fail on disk-full or permission errors]** → Log a warning and continue. The absence of `shutdown.json` is treated as "crash" (cautious), which is the safer default.

**[Branch pruning could delete an agent's active branch]** → Only prune branches that are `--merged main` (fully merged). Active feature branches with unmerged code are never pruned. The `git branch --merged` check is safe by definition.

**[store.Delete in shutdownAll removes audit trail]** → The lifecycle DB retains all state transitions. The session store is operational (for session routing), not archival. Memory.jsonl is archived before workDir deletion. No data loss.

## Migration Plan

1. `completed_at` column on `sessions.db` is additive — add with `ALTER TABLE sessions ADD COLUMN completed_at DATETIME DEFAULT NULL`. Existing rows have NULL, which is fine (cleanupOldWorkDirs already uses lifecycle state, not completed_at).
2. Deploy, monitor cascade loop depth via slog (`"cascade: round N unblocked M issues"`).
3. No rollback needed — all changes are additive. If cascade causes issues, the `maxCascadeRounds` config can be set to 1 to restore single-pass behavior.

## Open Questions

- Should `maxCascadeRounds` be configurable in `fordjent.local.yaml` or a compile-time constant? Leaning toward config for operational flexibility.
