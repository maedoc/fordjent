## Context

Fordjent's spec subsystem (`internal/speccycle/`, `internal/tool/openspec_tools.go`, `internal/session/manager.go`) was built incrementally over several feature passes. A static review (documented in `spec-analysis-26-03-05.md`) found seven bugs and several polish items. All are localized fixes with no architectural change — no new packages, no new API surfaces, no config changes.

## Goals / Non-Goals

**Goals:**
- Fix all seven identified bugs before writing integration tests against this code.
- Improve error observability (log instead of silently discard).
- Harden git operations in spec tools to avoid unintended side effects.
- Make the `specChangeRefRegex` robust against false positives from prose.

**Non-Goals:**
- Multi-change PR support (BUG-2 option a) — not a real use case; PM convention is one change per PR.
- Adding new tools or new capabilities.
- Changing the spec lifecycle state machine or label transitions beyond logging.
- Fixing the `MarkTaskComplete` concurrent-write race (BUG-6) — branch isolation mitigates it; document as accepted limitation.

## Decisions

### D1: `strings.Replace` over regex for `MarkTaskComplete`

The current `taskLineRegex.ReplaceAllString(line, "- [x] $2$3")` relies on `$2`/`$3` capture-group expansion, which is fragile if the description contains dollar signs or the regex engine interprets them specially. Since we've already identified the line as a task line and confirmed the 1-based position, we can simply replace the first `- [ ]` with `- [x]` via `strings.Replace(line, "- [ ]", "- [x]", 1)`. This is unambiguous, faster, and immune to capture-group quirks.

### D2: Read from merge SHA instead of `main`

`handleSpecPRMerged` currently reads `tasks.md` from the `main` branch. If another PR merges between the spec PR merge event and this read, the content could differ. The Forgejo `PullRequest` struct already carries the merge commit SHA (or we can fetch it via `GetPR`). Using the SHA as the ref guarantees we read the exact content that was merged.

Implementation: After calling `m.forgejoClient.GetPR()`, use `pr.MergeCommitSHA` as the ref parameter to `GetFile()` instead of `"main"`.

### D3: Rollback on commit failure in `openspec_mark_task`

If `git commit` or `git push` fails after `MarkTaskComplete` mutates `tasks.md` locally, the file is left in a modified state that doesn't match reality (task appears done in local file but was never committed). The fix: on commit failure, revert the checkbox change by re-writing the line with `- [ ]` instead of `- [x]`. Since we know the exact line that was changed, we can reverse it deterministically.

### D4: Tighten `specChangeRefRegex`

The current regex `(?:Spec|spec|Change|change)\s*:\s*...` matches prose. The tightened version requires the explicit marker `Spec:` (capital S) followed by a kebab-case name, with a line-start or newline anchor:

```go
var specChangeRefRegex = regexp.MustCompile(`(?:^|\n)Spec:\s*([a-z][a-z0-9-]+)`)
```

This eliminates false positives like "climate change:", "spec file:", etc.

### D5: `git add openspec/` instead of `git add -A`

Self-explanatory. The archive tool should only stage files in the `openspec/` directory tree, not everything in the repo.

### D6: Document single-change PR behavior

Add a doc comment on `SpecPRManager.IsSpecPR` stating that when a PR contains files from multiple changes, only one change name is returned (arbitrary, map iteration order). The PM convention is one change per PR, so this is acceptable. No code change needed.

### D7: Log label-operation errors

Replace `_ = m.forgejoClient.RemoveIssueLabel(...)` with proper error logging:

```go
if err := m.forgejoClient.RemoveIssueLabel(...); err != nil {
    slog.Warn("spec labels: failed to remove spec-proposed", "error", err, ...)
}
```

Same pattern for `AddIssueLabels`.

### D8: Count actual issue-creation successes

Currently `handleSpecPRMerged` logs "implementer issues created" with the total count even if individual `createSpecIssue` calls failed. Track actual successes and log both attempted and succeeded counts.

## Risks / Trade-offs

- **strings.Replace approach (D1)**: If the `- [ ]` pattern appears in the task description text (not the checkbox), `strings.Replace` would incorrectly modify it. However, since we've already matched the line with `taskLineRegex` and confirmed its position, we know the first `- [ ]` on the line IS the checkbox. No risk.
- **Merge SHA approach (D2)**: Requires the Forgejo PR API to return `MergeCommitSHA`. If the field is empty (race with merge event), fall back to `"main"`.
- **Rollback on failure (D3)**: If the rollback itself fails (e.g., disk error), we log the failure and return the warning string. The local file may be in an inconsistent state, but the git working tree will show a diff that a human can resolve.
- **Tightened regex (D4)**: Any existing issues/PRs with `spec:` (lowercase) or `Change:` references won't match anymore. Since this is new code with no legacy data, there's no backward-compatibility concern.
