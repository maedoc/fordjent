# spec-fixes-hardening Implementation Result

## Summary
All 19 tasks implemented and verified. No regressions introduced.

## Changed Files
- `internal/speccycle/tasks.go` — `markTaskComplete` uses `strings.Replace` instead of regex; documented concurrent-write limitation
- `internal/speccycle/prmanager.go` — Added doc comment on `IsSpecPR` for single-change behavior
- `internal/tool/openspec_tools.go` — `git add -A` → `git add openspec/` in archive; tightened `specChangeRefRegex`; added rollback logic in mark_task; added `os` import; added `rollbackTasksFile` method
- `internal/session/manager.go` — `handleSpecPRMerged` reads from merge SHA (with fallback); tracks issue-creation successes; `handleSpecLifecycleLabels` logs label errors
- `internal/forgejo/client.go` — Added `MergeCommitSHA` field to `PullRequest` struct
- `internal/speccycle/manager_test.go` — Added `TestMarkTaskComplete_DollarSignInDescription`
- `internal/tool/openspec_tools_test.go` — Added `TestArchiveTool_GitAddScoping`, `TestExtractSpecChangeRef_RejectsFalsePositives`, `TestExtractSpecChangeRef_AcceptsValid`, `TestOpenSpecMarkTaskTool_RollbackTasksFile`, `TestOpenSpecMarkTaskTool_RollbackWithEmptyOriginal`
- `internal/session/specpr_test.go` — Added `TestHandleSpecPRMerged_ReadsFromMergeSHA`, `TestHandleSpecPRMerged_FallsBackToMainWhenNoSHA`, `TestHandleSpecLifecycleLabels_LogsLabelErrors`

## Test Results
- `internal/speccycle`: 31/31 PASS
- `internal/forgejo`: 12/12 PASS
- `internal/tool` (openspec-related): All PASS
- `internal/session` (specPR tests): All PASS
- Pre-existing `TestBashToolSuccess` failure (bwrap not available) — NOT introduced by this change
