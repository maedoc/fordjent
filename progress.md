# Progress: openspec-integration Implementation

## Task: Speccycle Tests (tasks 1.7-1.8)
**Status: Complete**

- [x] 1.7 Write `internal/speccycle/manager_test.go` with comprehensive unit tests
- [x] 1.8 `go vet` + `go test` both pass

### Test coverage (32 tests):
- 3× CreateChange (normal, duplicate, empty name)
- 4× ListChanges (normal, archive excluded, empty dir, no dir)
- 4× ParseTasks (normal, empty file, no file, prose-only)
- 5× MarkTaskComplete (normal, idempotent, out-of-range, zero index, multi-mark)
- 4× ArchiveChange (normal + spec sync, target exists, not found, no specs)
- 1× ChangeExists
- 3× ReadChangeFile (normal, escape detection, not found)
- 4× ReadSpecFile (merged, change fallback, merged priority, not found)
- 2× isSpecFilePath / extractChangeName (comprehensive path cases)
- 2× SpecPRManager.IsSpecPR (spec PR detection, error handling)

## Task: Prompt & Tool Registration Updates (tasks 2.2-2.5, 4.4, 5.1, 5.2, 2.1_partial)
**Status: Complete**

### Changes to `internal/session/agent.go`

**buildRoleRegistry()** — tool registration per role:
- PM: Added write_file (restricted to openspec/), OpenSpecProposeTool, OpenSpecGetTasksTool, OpenSpecArchiveChangeTool
- Reviewer: Merged MergePRTool with repoDir param, OpenSpecReadSpecTool, OpenSpecGetTasksTool
- Implementer: Merged MergePRTool with repoDir param, OpenSpecGetTasksTool, OpenSpecReadSpecTool, OpenSpecMarkTaskTool

**buildSystemPrompt()** — prompt sections added:
- PM: OpenSpec Spec Creation (10-step flow: propose → write_file spec files → git → PR/yolo)
- PM: Spec PR Review Mode (review comments → update files → push to same branch)
- PM: Spec Archive flow (verify tasks → archive_change → git commit → label complete)
- Reviewer: Spec-Driven Review (7-step flow: read_spec → check requirements → verify → round cap)
- Implementer: Spec-Driven Implementation (7-step flow: get_tasks → read_spec → implement → verify → mark_task)

### Changes to `internal/tool/local_tools.go`
- Added `allowedPrefixes` field and `SetAllowedPrefixes()` method to writeFileTool
- Added path prefix restriction in Execute(): when allowedPrefixes set, paths outside prefixes are rejected

### Changes to `internal/tool/forgejo_tools.go`
- Updated NewMergePRTool to accept `repoDir string` parameter
- Added `checkSpecRef()` helper method

### Verification
- `go build ./...` — passes
- `go vet ./internal/session/... ./internal/tool/...` — passes
