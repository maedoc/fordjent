## 1. openspec_propose auto-creates directory

- [x] 1.1 Add `speccycle.NewSpecManager(repoDir).CreateChange(params.ChangeName)` call in `openSpecProposeTool.Execute` before returning instructions
- [x] 1.2 If `CreateChange` fails (already exists), return the error
- [x] 1.3 Add test: `openspec_propose` creates `openspec/changes/<name>/specs/` directory
- [x] 1.4 Add test: `openspec_propose` with duplicate name returns error

## 2. openspec_read_change tool

- [x] 2.1 Define `openSpecReadChangeTool` struct with `sessionInfo` field
- [x] 2.2 Implement `Name()`, `Description()`, `Parameters()`, `Execute()` — delegate to `speccycle.ReadChangeFile()`
- [x] 2.3 Register tool for implementer and reviewer roles in `agent.go`
- [x] 2.4 Add test: read `proposal.md` from change
- [x] 2.5 Add test: read `specs/capability/spec.md` from change
- [x] 2.6 Add test: path traversal rejected
- [x] 2.7 Add test: nonexistent file returns error
- [x] 2.8 Add test: nonexistent change returns error

## 3. Reviewer prompt enhancement

- [x] 3.1 Add merge-policy check instruction after Spec-Driven Review section in reviewer prompt
- [x] 3.2 Instruction: "Before calling forgejo_merge_pr, check if no-auto-merge or required-review policy is set. If so, post review as comment instead."
