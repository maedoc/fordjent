# Ralph Loop Implementation Output

## Summary

All 41 of 42 tasks for the OpenSpec change "ralph-loop" have been implemented. Task 7.4 (manual end-to-end validation) is the only remaining task, which requires a live Forgejo instance and is marked as needing manual verification.

## New Package: `internal/ralph/`

| File | Purpose |
|------|---------|
| `internal/ralph/tracker.go` | Tracker struct — 4-A stage tracking (awareness/act/assert/append), ordering validation, turn-based nudging (`ShouldNudge`), `IsComplete()`, `Reset()` |
| `internal/ralph/progress.go` | WriteProgress/ReadProgress/ListProgress — markdown files to `.ralph/progress/pr-{N}-iteration-{M}.md` |
| `internal/ralph/guard.go` | Guard struct — `IsSpecPath()`, `ValidateCommitDiff()`, `IsProgressPath()` |
| `internal/ralph/scheduler.go` | Scheduler struct — ticker-based PR scanner, iteration dispatch, cooldown, active tracking, RalphConfig |
| `internal/ralph/ralph_test.go` | Unit tests: 32 tests covering Tracker (ordering, nudging, completion, reset), Guard (spec detection, commit diff, progress path), Progress (write/read/list), Scheduler (spawn, cooldown, caps, budget, active tracking) |

## New Tools: `internal/tool/ralph_tools.go`

- `ralph_update` — 4-A protocol stage recording (stage + message params, ordering enforcement)
- `ralph_progress` — Progress file writer for ralph iterations
- `IsRalphSession()` — Session key pattern detection (`-ralph-i` suffix)
- `ParseRalphSessionKey()` — Extract repo, PR number, iteration number from session key

## Modified Files

| File | Change |
|------|--------|
| `internal/config/config.go` | Added `RalphConfig` struct with all fields + defaults in Load() |
| `internal/lifecycle/lifecycle.go` | Added `ralph_sessions` table to initSchema(); added RalphRecord struct, RecordRalphIteration(), GetLastRalphIteration(), GetRalphCost(), ListStalledRalphSessions(), CountRalphIterations(), GetLastNRalphIterations() |
| `internal/tool/local_tools.go` | Added `ralphGuardChecker` interface; `writeFileTool.SetRalphGuard()` + spec path check in Execute(); `gitTool.SetRalphGuard()` + staged diff validation before commit |
| `internal/tool/registry.go` | Added `RalphGuardChecker` interface + `SetRalphGuard()` method |
| `internal/session/agent.go` | Ralph session key detection → ralph tools registration in buildRoleRegistry(); ralph mode prompt injection in buildSystemPrompt(); ralph import |
| `internal/forgejo/client.go` | Added `ralph`, `ralph-completed`, `fordjent/failed:ralph-*` labels to EnsureLabels(); added `ListOpenPRsByLabel()` method |
| `internal/webhook/router.go` | Added ralph section to /status handler; `queryRalphSessionsDB()` function |
| `AGENTS.md` | Added comprehensive "Ralph Iterative Refinement Mode" section with activation, 4-A protocol, config, failure modes, debugging tips |
| `openspec/changes/ralph-loop/tasks.md` | 41/42 tasks marked [x] |

## Test Results

```
ok  github.com/fordjent/fordjent/internal/ralph       (32 tests, all pass)
ok  github.com/fordjent/fordjent/internal/session      (all pass, including new ralph tool registration)
ok  github.com/fordjent/fordjent/internal/lifecycle    (all pass)
ok  github.com/fordjent/fordjent/internal/config       (all pass)
ok  github.com/fordjent/fordjent/internal/forgejo      (all pass)
```

## Build Verification

```
go build ./...          → success
go vet ./internal/ralph/... ./internal/tool/... ./internal/session/... → success (0 errors)
```
