## Why

The spec lifecycle code has seven latent bugs and several minor polish issues discovered during static analysis (see `spec-analysis-26-03-05.md`). None are crashing production today, but they represent data-corruption risks, fragile regex behavior, and silent error swallowing that will bite as spec usage scales. Fixing them now — before the integration tests in `spec-coverage` — ensures we write tests against correct code.

## What Changes

- Replace `git add -A` with `git add openspec/` in the archive tool to prevent sweeping unrelated files into archive commits.
- Document that multi-change PRs are unsupported (single-change detection is intentional; the PM creates one change per PR by convention).
- Replace regex-based checkbox replacement in `MarkTaskComplete` with simple `strings.Replace` to avoid `$2`/`$3` expansion fragility.
- Read `tasks.md` from the PR's merge commit SHA instead of `main` to close a race window during concurrent merges.
- Tighten `specChangeRefRegex` to require `Spec:` prefix with kebab-case name to eliminate false positives from prose.
- Document the `MarkTaskComplete` concurrent-write race as an accepted limitation (branch isolation mitigates it).
- Roll back the local `tasks.md` mutation if `git commit` fails in `openspec_mark_task`.
- Add `ChangeExists()` pre-check in the archive tool for clearer error messages.
- Log errors from `RemoveIssueLabel`/`AddIssueLabels` in `handleSpecLifecycleLabels` instead of silently discarding them with `_ =`.
- Count actual issue-creation successes in `handleSpecPRMerged` instead of logging "issues created" when all may have failed.

## Capabilities

### New Capabilities

_None_ — all changes are fixes to existing code.

### Modified Capabilities

- `speccycle`: `MarkTaskComplete` replacement strategy changes from regex to `strings.Replace`; `IsSpecPR` behavior documented as single-change-only; `ArchiveChange` gains `ChangeExists` pre-check; `ReadChangeFile` unchanged.
- `spec-driven-implementation`: `openspec_archive_change` scopes `git add` to `openspec/` only; `openspec_mark_task` gains rollback on commit failure; `specChangeRefRegex` tightened; `handleSpecPRMerged` reads from merge SHA and counts actual successes.
- `spec-driven-review`: `handleSpecLifecycleLabels` logs label-operation errors instead of silently discarding them.

## Impact

- `internal/speccycle/tasks.go` — `markTaskComplete` replacement logic
- `internal/speccycle/prmanager.go` — documentation comment on single-change behavior
- `internal/tool/openspec_tools.go` — `git add -A` → `git add openspec/` in archive tool; `specChangeRefRegex` pattern; rollback logic in mark_task; `ChangeExists` check in archive
- `internal/session/manager.go` — `handleSpecPRMerged` reads from merge SHA; counts successes; `handleSpecLifecycleLabels` logs errors
- No API changes, no config changes, no breaking changes
