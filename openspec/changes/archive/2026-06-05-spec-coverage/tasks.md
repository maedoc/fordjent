## 1. Integration test

- [x] 1.1 Extend `specPRFakeForgejo` with `GetFile` handler that returns `tasks.md` content for spec changes
- [x] 1.2 Add `TestSpecLifecycleIntegration_FullLifecycle` — dispatch PullRequestMerged for spec PR, verify 3 issues created, milestone attached, summary comment posted
- [x] 1.3 Add `TestSpecLifecycleIntegration_PartiallyCompleted` — 4 tasks with 2 done, verify only 2 issues created
- [x] 1.4 Add `TestSpecLifecycleIntegration_NonSpecPR` — verify no side effects for code-only PR
- [x] 1.5 Add tests for spec tool calls against a temp directory: `openspec_get_tasks`, `openspec_read_spec`, `openspec_mark_task`

## 2. Language-aware extractPathPrefixes

- [x] 2.1 Define `langPrefixes` map in `manager.go` with Go, Python, JavaScript, and unknown whitelists
- [x] 2.2 Modify `validateParallelTasks` to accept `lang` parameter and pass it to `extractPathPrefixes`
- [x] 2.3 Modify `extractPathPrefixes` to accept `lang` parameter and select whitelist from `langPrefixes`
- [x] 2.4 Add test: Go repo extracts `pkg/auth` from task description
- [x] 2.5 Add test: Python repo extracts `app/auth` from task description
- [x] 2.6 Add test: JS repo extracts `components/Header.jsx` from task description
- [x] 2.7 Add test: unknown language extracts nothing
- [x] 2.8 Update `handleSpecPRMerged` call site to detect language via `scaffold.DetectProjectLang`

## 3. spec-implementing label

- [x] 3.1 Add `AddIssueLabels(ctx, repo, prNumber, []string{"spec-implementing"})` after issue creation in `handleSpecPRMerged`
- [x] 3.2 Add test: `spec-implementing` label is added after implementer issues are created

## 4. Change.Schema population

- [x] 4.1 In `listChanges`, read `.openspec.yaml` from each change directory and extract `schema` field
- [x] 4.2 If `.openspec.yaml` missing or unparseable, leave `Schema` as empty string
- [x] 4.3 Add test: `ListChanges` returns `Schema` when `.openspec.yaml` exists
- [x] 4.4 Add test: `ListChanges` returns empty `Schema` when `.openspec.yaml` is missing

## 5. Eval scenario

- [x] 5.1 Define `SpecLifecycleScenario` struct in `internal/eval/scenarios.go` with seed files including openspec directory structure
- [x] 5.2 Write `SpecLifecycleVerify` function that checks build, test, and task completion status
- [x] 5.3 Add `TestEvalSpecLifecycle` to `eval_test.go` (N=1, 15 min timeout)
- [x] 5.4 Test the scenario manually with `EVAL_SKIP_SETUP=true` against running services
