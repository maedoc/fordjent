## Context

The spec subsystem has unit-test coverage for each component (speccycle, specpr, tools) but no test that verifies they work together. The parallel validation (`extractPathPrefixes`) only recognizes Go project directories. The `spec-implementing` label is prompt-only — the code never adds it. The `Change.Schema` field is declared but always zero-valued.

This change adds integration tests, eval scenarios, and small production fixes to close these gaps without architectural changes.

## Goals / Non-Goals

**Goals:**
- Verify the spec lifecycle works end-to-end: merge → issues → tools → labels.
- Add a spec-lifecycle eval scenario for automated regression testing.
- Make `extractPathPrefixes` work for Python and JavaScript repos.
- Auto-add `spec-implementing` label when implementer issues are created.
- Populate `Change.Schema` from `.openspec.yaml`.

**Non-Goals:**
- Spec-driven reviewer integration test (too dependent on LLM behavior; deferred).
- `spec-complete` auto-label (requires milestone progress tracking on every PR merge; needs separate design).
- Verification contract enforcement (v2 feature).
- Hard review round cap (cross-session state; invasive).

## Decisions

### D1: Integration test uses `specpr_test.go` fake Forgejo pattern

The existing `specPRFakeForgejo` in `specpr_test.go` is a well-structured `httptest.Server` with configurable responses. The new `TestSpecLifecycleIntegration` will extend this fake with:
- `GetFile` handler that returns `tasks.md` content
- `GetPR` handler that returns a PR with a merge commit SHA and `spec/` branch name
- Label operation assertions

This avoids Docker dependencies and keeps tests fast.

### D2: Eval scenario seeds a merged spec

`SpecLifecycleScenario` will:
1. Create a repo with `go.mod`, `.gitignore`, `README.md`, and `openspec/changes/test-feature/` (proposal.md, tasks.md, specs/test-cap/spec.md)
2. Create an `[implementer]` issue referencing `Spec: test-feature`
3. Verify the implementer writes code, marks the task, and creates a PR

The scenario is designed for the eval harness but can also be used as a manual test by creating the repo and firing the issue.

### D3: Language-aware prefix whitelists

The current `goPathPrefixes` map is replaced with a `langPrefixes` map keyed by language name. `extractPathPrefixes` calls `scaffold.DetectProjectLang(repoDir)` to determine the language, then uses the appropriate whitelist. For "unknown" language, no prefixes are extracted (same as today's behavior for non-Go paths).

```go
var langPrefixes = map[string]map[string]bool{
    "go":         {"cmd": true, "internal": true, "pkg": true, "api": true, "web": true, "docs": true, "configs": true, "scripts": true},
    "python":     {"src": true, "app": true, "tests": true, "migrations": true, "scripts": true, "configs": true, "docs": true},
    "javascript": {"lib": true, "test": true, "public": true, "pages": true, "components": true, "src": true, "scripts": true, "docs": true},
    "unknown":    {},
}
```

The `DetectProjectLang` function is already exported from `scaffold/scaffold.go`. The `validateParallelTasks` function now receives `repoDir` and passes it through.

### D4: `spec-implementing` label added automatically

`handleSpecPRMerged` adds `spec-implementing` to the original PR after creating implementer issues. This is a single `AddIssueLabels` call with the existing client. If the label doesn't exist in Forgejo, the call fails with a logged warning — no crash. The PR that was merged already has `spec-approved`; adding `spec-implementing` provides observability into which specs are in active implementation.

### D5: Populate `Change.Schema` from `.openspec.yaml`

`ListChanges` currently returns `Change{Name, Schema, LastModified}` but `Schema` is always empty. The fix: when listing changes, attempt to read `.openspec.yaml` from each change directory and extract the `schema` field. If the file doesn't exist or can't be parsed, `Schema` remains empty (backward compatible).

## Risks / Trade-offs

- **Eval scenario depends on LLM**: The eval scenario requires a live LLM provider, making it unsuitable for CI without `EVAL_SKIP_SETUP` mode. The integration test (D1) does not require an LLM and is CI-safe.
- **Language detection is filesystem-only**: `DetectProjectLang` scans repo files. If the implementer hasn't written any code yet (empty repo), the language is "unknown" and no overlap detection occurs. Acceptable — the first spec in an empty repo typically doesn't have parallel tasks.
- **`spec-implementing` label must exist**: If the Forgejo instance doesn't have the label, `AddIssueLabels` fails. The bootstrap script must create it. This is a documentation/deploy concern, not a code concern.
