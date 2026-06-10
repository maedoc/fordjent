## ADDED Requirements

### Requirement: Store record deletion on shutdown
`shutdownAll` SHALL call `m.store.Delete(key)` for each session in addition to the existing `delete(m.sessions, key)`. This prevents stale `working` records from being restored on the next process start.

#### Scenario: Graceful shutdown cleans store
- **WHEN** `shutdownAll()` is called (process exit, docker stop)
- **THEN** each session's record is deleted from `sessions.db` as well as the in-memory map

#### Scenario: ReapIdle also cleans store
- **WHEN** `reapIdle()` cancels an idle session
- **THEN** the session's store record is deleted (current behavior — already calls `m.store.Delete`)

### Requirement: Merged branch pruning
`cleanupOldWorkDirs` SHALL prune merged feature branches from the repo clone inside each work directory being cleaned up. The pruning SHALL run `git branch --merged main` and delete any branch listed as merged (excluding `main` and the currently checked-out branch).

#### Scenario: Prune merged branches during cleanup
- **WHEN** the hourly cleanup processes a completed session's work directory
- **THEN** `git branch --merged main` is run in the repo directory; merged feature branches are deleted with `git branch -d <name>`

#### Scenario: Active branch not pruned
- **WHEN** a feature branch with unmerged code exists in the work directory
- **THEN** it is not listed by `git branch --merged main` and is not pruned

#### Scenario: Branch pruning failure is non-fatal
- **WHEN** `git branch --merged` fails (corrupt repo, missing git binary)
- **THEN** a warning is logged and cleanup proceeds to the next step

### Requirement: completed_at field on session store
The `sessions` table SHALL have a `completed_at` column (DATETIME, nullable). When a session transitions to `completed` or `failed` state in the lifecycle DB, the `completed_at` field on the corresponding store record SHALL be set to the current UTC timestamp.

#### Scenario: completed_at set on session completion
- **WHEN** a session completes successfully
- **THEN** the session's store record has `completed_at` set to the completion timestamp

#### Scenario: completed_at is NULL for active sessions
- **WHEN** a session is in `working` state
- **THEN** `completed_at` is NULL

#### Scenario: Existing rows have NULL completed_at
- **WHEN** the migration adds the `completed_at` column to an existing database
- **THEN** all existing rows have NULL (safe default)

### Requirement: Branch pruning on all work directory scans
Branch pruning SHALL run not only in `cleanupOldWorkDirs` (7-day archived sessions) but also independently during a periodic scan of ALL session work directories, not just completed ones. This catches branches from sessions that are still technically "active" in the lifecycle but haven't processed events in hours.

#### Scenario: Prune branches from idle sessions
- **WHEN** the hourly cleanup runs and a session has been idle for >1 hour but is not yet in `completed` state
- **THEN** merged branches in that session's repo clone are still pruned (the session's in-memory state is unaffected — only the git branches in the clone are cleaned)
