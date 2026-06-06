# spec-tool-enhancements Implementation Result

## Summary
All 14 tasks implemented and verified. No regressions introduced (pre-existing TestBashToolSuccess failure unrelated).

## Changed Files
- `internal/tool/openspec_tools.go` — Added `sessionInfo` field to `openSpecProposeTool`; added `CreateChange` call in Execute; added new `openSpecReadChangeTool` struct with full implementation (Name, Description, Parameters, Execute delegating to `speccycle.ReadChangeFile`); updated `NewOpenSpecProposeTool` signature
- `internal/tool/openspec_tools_test.go` — Updated `TestOpenSpecProposeTool` to pass sessionInfo and verify directory creation; updated `TestOpenSpecProposeTool_EmptyName` and `TestOpenSpecProposeTool_PathSeparator` for new signature; added `TestOpenSpecProposeTool_CreatesDirectory`, `TestOpenSpecProposeTool_DuplicateChange`; added 5 `TestOpenSpecReadChangeTool_*` tests (ReadProposal, ReadNestedSpec, PathTraversalRejected, NonexistentFile, NonexistentChange)
- `internal/session/agent.go` — Updated `NewOpenSpecProposeTool()` call to pass `sessionInfo`; registered `NewOpenSpecReadChangeTool(sessionInfo)` for reviewer, implementer/default roles; added merge-policy check instruction after Spec-Driven Review section

## Test Results
- `internal/tool` (openspec tests): 18/18 PASS
- Pre-existing `TestBashToolSuccess` failure (bwrap not available) — NOT introduced by this change

## Task Breakdown
- Group 1 (openspec_propose auto-creates): 4/4 ✓
- Group 2 (openspec_read_change tool): 8/8 ✓
- Group 3 (reviewer prompt): 2/2 ✓

## Notes
- `ReadChangeFile` already existed in `internal/speccycle/archive.go` with path traversal protection — no new speccycle code was needed
- The reviewer prompt enhancement is prompt-only; `forgejo_merge_pr` tool already has hard gates for policy enforcement
- `internal/session/` tests have a build error from Change B's modification of `extractPathPrefixes` signature (not from this change)
