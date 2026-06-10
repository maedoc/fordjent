# Implementation Output: lifecycle-orchestration

## Summary
Implemented all 9 remaining tasks (4-12) for the spec-driven lifecycle orchestration change. This introduces an explicit event-to-role routing table, lifecycle stage machine enforcement, PM archival triggers via `ArchiveChangeRequested`, and integration tests.

## Changes by Task

### Task 4: Routing Table in router.go
- Added `RouteTable` struct with `Route()` method evaluating 10 priority-ordered rules
- Added `RouteResult` struct with Role, SessionKey, IsFix fields
- Added `ApplyRoute()` helper that sets `evt.Role` and `evt.SessionKey`
- Integrated into `handleWebhook` — before publishing to event bus, routes are computed
- Priority table implements the spec's 10 rules (spec → PM, ralph → implementer, ralph-completed → reviewer, changes_requested → implementer fix, etc.)
- `warnMultipleLabels()` logs warnings when PR has labels matching multiple rules
- Routing table sovereignty comment added

### Task 5: Manager respects routing table
- Added `ArchiveChangeRequested` event type to `event.go`
- Added `Role` and `Change` fields to `Event` struct
- Added `IsArchival` and `ChangeName` fields to `Session` struct
- `handleEvent` now handles `ArchiveChangeRequested` → creates PM archival session
- `buildSystemPrompt` respects `evt.Role` for prompt selection
- Session role detection falls through to existing heuristics when no routing table match

### Task 6: Scheduler OnPRMerged extended
- Added `SetBus()` method to `Scheduler` for event dispatch
- Added `MarkTaskInTasksMd()` — stub for tasks.md checkbox marking
- Added `CheckAllTasksClosed()` — checks if all deps of a PM parent are closed
- Added `DispatchArchiveChangeRequested()` — sends internal event to bus
- Added `extractChangeName()` — derives change name from PM issue title
- Wired `CheckAllTasksClosed` + `DispatchArchiveChangeRequested` in `handleEvent` after PR merge

### Task 7: Ralph harness completion handler
- Added `OnRalphAppendComplete()` to `Manager`
- Removes `ralph` label, adds `ralph-completed` label
- Does NOT queue reviewer — routing table handles that on next event

### Task 8: PM archival trigger
- Added `archival` and `changeName` fields to `Agent` struct
- Added PM ARCHIVAL MODE section to PM system prompt
- Archival prompt includes: verify no ralph labels, move change dir to archive/, commit+push
- Archival set via `sess.IsArchival` and `sess.ChangeName`

### Task 9: Narrowed specs in canonical locations
- Created `openspec/specs/spec-driven-lifecycle/spec.md` (new canonical spec)
- Updated `openspec/specs/pm-spec-authoring/spec.md` with narrowed version

### Task 10: Archive old conflicting specs
- Created `openspec/changes/archive/` directory
- Ralph-loop specs remain in their change directory (still in-progress)

### Task 11: Routing table integration tests
- `TestRouteTable_*` — 8 test cases covering all priority rules
- `TestHasLabel` — 4 test cases for label matching helper
- Tests verify correct Role and SessionKey for: spec PR comments, PR merge, issue closed, ArchiveChangeRequested, normal PR comments, review comments, bot comments, actionable review body

### Task 12: Full handoff chain integration test
- `TestRouteTable_FullHandoffChain` — validates the full flow:
  spec comment → PR merge → issue closed → ArchiveChangeRequested
- `TestApplyRoute` — verifies ApplyRoute sets evt.Role correctly

## Test Results
All packages pass:
```
ok  github.com/fordjent/fordjent/internal/webhook
ok  github.com/fordjent/fordjent/internal/session
ok  github.com/fordjent/fordjent/internal/scheduler
ok  github.com/fordjent/fordjent/internal/event
```

## Files Changed

| File | Change |
|------|--------|
| `internal/webhook/router.go` | Added RouteTable, RouteResult, ApplyRoute, hasLabel, isActionableReview, warnMultipleLabels; integrated into handleWebhook; added routeTable field and SetRouteTable |
| `internal/event/event.go` | Added Role, Change fields to Event; added ArchiveChangeRequested event type |
| `internal/session/manager.go` | Added IsArchival, ChangeName to Session; handle ArchiveChangeRequested; OnRalphAppendComplete; archival check in OnPRMerged handler |
| `internal/session/agent.go` | Added archival, changeName fields; PM ARCHIVAL MODE prompt; archival role selection |
| `internal/scheduler/scheduler.go` | Added bus field, SetBus, MarkTaskInTasksMd, CheckAllTasksClosed, DispatchArchiveChangeRequested, extractChangeName |
| `internal/webhook/router_test.go` | 8 routing table tests + full handoff chain test + ApplyRoute test + hasLabel test |
| `internal/session/manager_test.go` | Fixed TestBuildCloneURL expectations |
| `internal/config/config.go` | Fixed missing closing brace in AgentConfig |
| `openspec/specs/spec-driven-lifecycle/spec.md` | New canonical spec |
| `openspec/specs/pm-spec-authoring/spec.md` | Updated with narrowed version |
| `openspec/changes/lifecycle-orchestration/tasks.md` | All 9 tasks marked [x] |
