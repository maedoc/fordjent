## Why

The spec system has no integration test that exercises the full lifecycle (spec PR merge → implementer issue creation → spec tool calls → label transitions → archive). Individual components are unit-tested, but the connectors between them (event dispatch, session creation, tool registration per role, prompt generation) are unverified. Additionally, `extractPathPrefixes` only recognizes Go project directories, leaving Python/JS repos without parallel-task overlap detection. The `spec-implementing` label is never automatically applied, and the `speccycle.Change.Schema` field is defined but never populated.

## What Changes

- Add `TestSpecLifecycleIntegration` in `internal/session/` that exercises the full spec PR merge → implementer issue creation → milestone → label transitions flow end-to-end with a fake Forgejo.
- Add `SpecLifecycleScenario` in `internal/eval/` that seeds a repo with a merged spec and verifies the agent can process a spec-driven implementer issue.
- Make `extractPathPrefixes` language-aware by calling `scaffold.DetectProjectLang()` and choosing directory-prefix whitelists per language.
- Auto-add `spec-implementing` label in `handleSpecPRMerged` after creating implementer issues.
- Populate `speccycle.Change.Schema` from the `.openspec.yaml` file during `ListChanges()`.

## Capabilities

### New Capabilities

- `spec-integration-test`: End-to-end integration test for the spec lifecycle
- `spec-eval-scenario`: Eval harness scenario for spec-driven workflows

### Modified Capabilities

- `speccycle`: `ListChanges` populates `Schema` field from `.openspec.yaml`
- `parallel-validation-hardening`: `extractPathPrefixes` becomes language-aware with per-language whitelists

## Impact

- `internal/session/` — new integration test file
- `internal/eval/` — new scenario and verification function
- `internal/session/manager.go` — `handleSpecPRMerged` adds `spec-implementing` label
- `internal/speccycle/speccycle.go` — `ListChanges` reads `.openspec.yaml`
- `internal/session/manager.go` — `extractPathPrefixes` calls `scaffold.DetectProjectLang`
