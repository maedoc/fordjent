# Tasks: Spec-Driven Lifecycle Orchestration

## Pre-requisites
- [x] Narrow `pm-spec-authoring` spec (remove activation triggers) — see `proposals/pm-spec-authoring-narrowed.md`
- [x] Narrow `ralph-scheduler` spec (remove activation triggers) — see `proposals/ralph-scheduler-narrowed.md`
- [x] Narrow `ralph-protocol` spec (remove activation triggers) — see `proposals/ralph-protocol-narrowed.md`

## Implementation Tasks

- [x] Update `internal/webhook/router.go` to use explicit routing table
  - Route spec PR comments (`spec-proposed`/`spec-approved` labels) → PM
  - Route `ralph`-labeled PR comments → Ralph scheduler (spawn iteration)
  - Route `ralph-completed` PR comments → reviewer
  - Route `changes_requested` review comments → implementer `-fix` session
  - Add routing table sovereignty comment in code

- [x] Update `internal/session/manager.go` `handleEvent` to respect routing table
  - Remove implicit activation logic
  - All session creation flows through explicit route lookup
  - Handle `ArchiveChangeRequested` event → PM archival session

- [x] Update `internal/scheduler/scheduler.go` OnPRMerged
  - Mark task checkbox in `tasks.md` on merge
  - Check if all task issues for change are closed
  - If all closed: dispatch `ArchiveChangeRequested` event

- [x] Update Ralph harness completion handler
  - On `ac_met: true`: remove `ralph` label, add `ralph-completed` label
  - Do NOT queue reviewer directly — let scheduler routing table handle it
  - Record `ac_met` in iteration record only

- [x] Update PM archival trigger
  - PM session responds to `ArchiveChangeRequested` event (or `[pm]` archival issue)
  - Verify no `ralph` label on any PR for this change (natural consequence check)
  - Call `openspec_archive_change` + commit

- [x] Update narrowed specs in canonical locations
  - Replace `openspec/specs/pm-spec-authoring/spec.md` with narrowed version
  - Replace `openspec/changes/ralph-loop/specs/ralph-scheduler/spec.md` with narrowed version
  - Replace `openspec/changes/ralph-loop/specs/ralph-protocol/spec.md` with narrowed version
  - Install `openspec/specs/spec-driven-lifecycle/spec.md` (new canonical spec)

- [x] Archive old conflicting specs
  - Move old `pm-spec-authoring` spec content to `openspec/changes/archive/...` if it differs from narrowed
  - Ensure `ralph-loop` change retains its `.openspec.yaml`, proposal.md, design.md, tasks.md

- [x] Add integration test for routing table
  - Fake Forgejo with PRs carrying different labels
  - Assert correct session keys and roles for each event type

- [x] Add integration test for full handoff chain
  - Spec PR merge → scheduler creates task issues → implementer creates PR → Ralph activates → Ralph completes → reviewer merges → scheduler triggers archival
