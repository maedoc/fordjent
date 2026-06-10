## ADDED Requirements

### Requirement: Session exit checkpoint
When a session goroutine exits (for any reason — completion, failure, max turns, context cancellation), the system SHALL write a `shutdown.json` file to the session's work directory containing `{"state": "<state>", "last_tool": "<last_tool_name>", "timestamp": "<RFC3339>"}`.

#### Scenario: Session completes normally
- **WHEN** an implementer session runs to completion after creating a PR
- **THEN** `shutdown.json` is written with `state: "completed"` and the name of the last tool executed

#### Scenario: Session hits max turns
- **WHEN** a session exceeds `max_turns_implementer`
- **THEN** `shutdown.json` is written with `state: "failed"` and `last_tool` reflecting the last tool call before termination

#### Scenario: Session cancelled by shutdownAll
- **WHEN** `shutdownAll()` cancels a running session's context
- **THEN** `shutdown.json` is written with `state: "cancelled"` if the goroutine has time to write it; if not (hard kill), the file is absent

#### Scenario: Process crash (no shutdown.json)
- **WHEN** the Fordjent process is killed (OOM, SIGKILL)
- **THEN** `shutdown.json` is not written (the goroutine cannot execute file I/O before termination)

### Requirement: Restore skips completed sessions
`restoreSessions` SHALL check for `shutdown.json` in the work directory. If it exists with `state: "completed"`, the session SHALL be skipped and its store record deleted.

#### Scenario: Gracefully completed session not restored
- **WHEN** a session completed normally and `shutdown.json` has `state: "completed"` and the process restarts
- **THEN** `restoreSessions` logs "skipping completed session" and deletes the store record; no goroutine is spawned

#### Scenario: Failed session restored with steering
- **WHEN** a session failed (max turns) and `shutdown.json` has `state: "failed"`
- **THEN** `restoreSessions` creates the session but does NOT inject verification steering (the lifecycle auto-retry system handles retries)

### Requirement: Crash recovery verification steering
When `restoreSessions` finds a session that was `working` (lifecycle state) and has NO `shutdown.json` (indicating a crash), it SHALL inject a user-role steering message on the first turn: "The previous session was interrupted. Re-read any files you previously wrote to verify they are correct before continuing. Do not assume prior write_file or git operations succeeded."

#### Scenario: Agent verifies after crash
- **WHEN** the process crashes during an active session, restarts, and restores the session
- **THEN** the agent receives a steering message telling it to re-verify previously-written files on its first turn

#### Scenario: Agent does NOT verify after clean completion
- **WHEN** a session completed normally (`shutdown.json` exists with `state: "completed"`)
- **THEN** the session is skipped entirely (not restored)

#### Scenario: Agent does NOT verify after cancellation
- **WHEN** a session was cancelled by `shutdownAll` and `shutdown.json` has `state: "cancelled"`
- **THEN** the session is NOT restored (cancelled sessions should not be auto-resumed)

### Requirement: Last tool tracking
The `TurnExecutor` SHALL track the name of the last tool executed in the current session. This value is written to `shutdown.json` on session exit.

#### Scenario: Last tool recorded
- **WHEN** the agent executes `write_file` then `git commit` then the session ends
- **THEN** `shutdown.json` records `last_tool: "git"`
