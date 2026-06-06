## Why

The `openspec-integration` change is ~90% implemented but the review (June 2026) identified correctness and test-coverage gaps in the parallel task validation and spec lifecycle handlers. Currently, `validateParallelTasks` silently downgrades tasks without traceability, the regex heuristic is imprecise and recompiled on every call, and there's no integration test proving the downgrade path works end-to-end. These gaps could cause silent misclassification of parallel tasks in production.

## What Changes

- Add a `*(auto-downgraded from parallel)*` annotation in issue bodies when `validateParallelTasks` downgrades a task, so PM and operators can distinguish auto-downgraded issues from originally serial ones
- Move `extractPathPrefixes` regex to a package-level `var` to avoid recompilation on every call
- Add a whitelist of known Go project prefixes (`cmd/`, `internal/`, `pkg/`, `api/`) to reduce false positives from the path extraction heuristic
- Add integration test `TestHandleSpecPRMerged_ParallelOverlap_DowngradedToSerial` that exercises the full `handleSpecPRMerged` → `validateParallelTasks` → issue creation path with overlapping task descriptions
- Optionally add `fordjent/downgraded-parallel` label for Forgejo UI visibility

## Capabilities

### New Capabilities

- `parallel-validation-hardening`: Hardens the parallel task file-disjointness validator with traceability annotations, reduced false positives, and integration test coverage

### Modified Capabilities

- `speccycle`: The `SpecPRManager generates implementer issues on spec merge` scenario now requires downgrade traceability in issue bodies

## Impact

- `internal/session/manager.go` — `validateParallelTasks`, `extractPathPrefixes`, `handleSpecPRMerged` issuing body construction
- `internal/session/specpr_test.go` — new integration test
- No API changes, no breaking changes, no new dependencies
