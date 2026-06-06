## 1. markTaskComplete fixes

- [x] 1.1 Replace regex `ReplaceAllString` with `strings.Replace(line, "- [ ]", "- [x]", 1)` in `markTaskComplete`
- [x] 1.2 Add test: task description contains `$2` — verify dollar sign preserved after marking complete

## 2. git add scoping

- [x] 2.1 Change `git add -A` to `git add openspec/` in `openSpecArchiveChangeTool.gitAddAll`
- [x] 2.2 Add test: archive tool does NOT stage files outside `openspec/`

## 3. specChangeRefRegex tightening

- [x] 3.1 Change regex to `(?:^|\n)Spec:\s*([a-z][a-z0-9-]+)` in `openspec_tools.go`
- [x] 3.2 Add test: `extractSpecChangeRef` rejects lowercase `spec:`, `Change:`, and prose like "climate change:"
- [x] 3.3 Add test: `extractSpecChangeRef` accepts `Spec: user-auth` and `Spec: openspec/changes/user-auth`

## 4. Merge SHA reading

- [x] 4.1 In `handleSpecPRMerged`, fetch PR object via `GetPR` and use `pr.MergeCommitSHA` as ref for `GetFile`
- [x] 4.2 Add fallback: if `MergeCommitSHA` is empty, use `"main"` and log debug message
- [x] 4.3 Add test: `GetFile` called with merge SHA when available

## 5. Rollback on commit failure

- [x] 5.1 In `openspec_mark_task`, if `git commit` fails, revert the checkbox change (write `- [ ]` back) before returning error
- [x] 5.2 Add test: commit failure triggers rollback; tasks.md reverts to original state
- [x] 5.3 Add test: rollback failure logs warning and returns error string

## 6. Error logging and success counting

- [x] 6.1 Replace `_ = m.forgejoClient.RemoveIssueLabel(...)` with error-logged call in `handleSpecLifecycleLabels`
- [x] 6.2 Replace `_ = m.forgejoClient.AddIssueLabels(...)` with error-logged call in `handleSpecLifecycleLabels`
- [x] 6.3 In `handleSpecPRMerged`, track successful vs failed issue creations; log "N/M issues created (F failed)"
- [x] 6.4 Add test: `handleSpecLifecycleLabels` logs warning on label removal failure

## 7. Documentation

- [x] 7.1 Add doc comment on `IsSpecPR` stating single-change behavior for multi-change PRs
- [x] 7.2 Document `MarkTaskComplete` concurrent-write race as accepted limitation in `tasks.go`
