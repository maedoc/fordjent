## Context

The `openspec-integration` change implemented `validateParallelTasks()` in `internal/session/manager.go`. A June 2026 review identified five issues:

1. **Silent downgrade**: When parallel tasks overlap, they're downgraded to serial with no traceability — PM and operators cannot distinguish auto-downgraded issues from originally serial ones
2. **Regex imprecision**: `extractPathPrefixes` matches version strings like `v1.49.1/linux` as path tokens, causing false-positive overlap detection
3. **Regex recompilation**: `regexp.MustCompile` is called inside `extractPathPrefixes` on every invocation instead of at package init
4. **Missing integration test**: No test proves the full `handleSpecPRMerged` → `validateParallelTasks` → issue creation path when overlap is detected

Current code flow:
```
handleSpecPRMerged()
  → SpecPRManager.IsSpecPR()
  → ParseTasksContent()
  → validateParallelTasks(parallelTasks)   // ← downgrades silently
     → extractPathPrefixes(description)    // ← regex recompiled each call
        → pathsOverlap(a, b)
  → createSpecIssue() for each task
```

## Goals / Non-Goals

**Goals:**
- Add traceability annotation to downgraded parallel tasks in issue bodies
- Reduce false positives in path extraction by whitelisting known Go project prefixes
- Eliminate per-call regex compilation
- Add integration test covering the downgrade path through `handleSpecPRMerged`

**Non-Goals:**
- Full `mergequeue.CheckGate` integration at issue-creation time (architecturally impossible without branches)
- Structured file-path metadata in `tasks.md` format (would require a tasks.md schema change)
- Refactoring the fake Forgejo handler pattern in tests (separate concern)

## Decisions

### 1. Downgrade annotation: inline text over label

**Decision**: Add `*(auto-downgraded from parallel)*` in the issue body text, not a Forgejo label.

**Rationale**: Labels require API calls (`AddIssueLabels`, `CreateLabel`) and label lifecycle management. Inline body text is zero API calls, visible in the Forgejo UI, and persistent across label changes. The PM can also read it from the issue body.

**Alternative rejected**: A `fordjent/downgraded-parallel` label. This requires creating the label (with error handling for duplicates), attaching it to the issue, and managing its lifecycle during auto-retry. Adds complexity for minimal UX gain.

### 2. Path extraction: whitelist-then-fallback

**Decision**: `extractPathPrefixes` checks if the first segment matches a known Go project prefix (`cmd/`, `internal/`, `pkg/`, `api/`, `web/`, `docs/`, `configs/`) before accepting a match. If the first segment isn't in the whitelist, the match is discarded.

**Rationale**: The current regex `[a-zA-Z0-9_]+(?:/[a-zA-Z0-9_.-]+)+` matches any slash-separated tokens — including version strings ("v1.49") and prose fragments ("2.5/coder"). A whitelist of Go-conventional top-level directories reduces false positives without requiring the PM to follow a special format.

**Alternative rejected**: Require ≥3 segments for a match (e.g., `internal/forgejo/client.go`). This would miss valid 2-segment paths like `pkg/auth` which IS a meaningful directory prefix.

### 3. Package-level regex var

**Decision**: Move `regexp.MustCompile(...)` to a package-level `var pathLikeRe`.

**Rationale**: Go's `regexp` package documents that `MustCompile` should be called once. The current per-call compilation is not a performance problem (called once per spec merge), but it's the wrong pattern.

### 4. Integration test pattern

**Decision**: Add `TestHandleSpecPRMerged_ParallelOverlap_DowngradedToSerial` in `specpr_test.go` using the existing `specPRFakeForgejo` handler.

**Rationale**: The fake handler is already wired for issue creation, milestone creation, and label operations. The test just needs to use task descriptions containing overlapping paths and verify the resulting issues have serial dependencies.

## Risks / Trade-offs

- [Risk] Whitelist may miss valid non-Go project prefixes (e.g., `src/`, `lib/`) → Mitigation: the whitelist is a `[]string` var that can be extended; fallback is the merge queue blocking at PR creation time
- [Risk] Inline annotation could confuse the LLM agent reading the issue body → Mitigation: the annotation is in italics `*(...)*` which LLMs generally treat as metadata, not instructions
- [Risk] Adding annotation text changes the body format, which existing tests may assert on → Mitigation: existing tests check for `Contains`, not exact match, so annotation text is additive
