## 1. Foundation: `internal/speccycle/` package

- [ ] 1.1 Create `internal/speccycle/manager.go` with `SpecManager` struct and `CreateChange(repoDir, name string) error` — creates `openspec/changes/<name>/` with `.openspec.yaml` scaffold. [spec: speccycle/SpecManager creates OpenSpec change directories]
- [ ] 1.2 Implement `SpecManager.ListChanges(repoDir string) ([]Change, error)` — scans `openspec/changes/`, excludes `archive/`, returns change names with metadata. [spec: speccycle/SpecManager lists active changes]
- [ ] 1.3 Implement `SpecManager.ParseTasks(repoDir, name string) ([]Task, error)` — parses `tasks.md` checkbox lines (`- [ ]` / `- [x]`), extracts `[parallel]` tags, returns structured task list. [spec: speccycle/SpecManager parses tasks from tasks.md]
- [ ] 1.4 Implement `SpecManager.MarkTaskComplete(repoDir, name string, index int) error` — updates specific checkbox in `tasks.md` from `- [ ]` to `- [x]`. Handles idempotency and out-of-range. [spec: speccycle/SpecManager marks tasks complete]
- [ ] 1.5 Implement `SpecManager.ArchiveChange(repoDir, name string) error` — moves `changes/<name>/` to `changes/archive/YYYY-MM-DD-<name>/`, syncs delta specs from `changes/<name>/specs/` to `specs/`. Handles target-already-exists error. [spec: speccycle/SpecManager archives completed changes]
- [ ] 1.6 Implement `SpecPRManager` struct with `IsSpecPR(repo, prNumber int) (bool, string)` — queries Forgejo API for PR file list, detects `openspec/changes/` in paths, returns change name. [spec: speccycle/SpecPRManager detects spec PRs]
- [ ] 1.7 Write `internal/speccycle/manager_test.go` — unit tests for CreateChange, ListChanges, ParseTasks, MarkTaskComplete, ArchiveChange, IsSpecPR. Use temp directories, no external services needed.
- [ ] 1.8 Run `go vet ./internal/speccycle/...` and `go test ./internal/speccyle/...` — all pass.

## 2. PM Spec Authoring Tools

- [ ] 2.1 Add `openspec_propose` tool to tool registry — accepts `repository`, `change_name`, `description`. Returns instructions for the PM to write spec files via `write_file`. (This is a lightweight tool that confirms the change name and tells the PM the artifact structure to create.) [spec: pm-spec-authoring/PM creates spec artifacts for features]
- [ ] 2.2 Gate PM `write_file` permission to allow `openspec/changes/` and `openspec/specs/` paths. Restriction is applied in `session/agent.go` tool execution — PM role can write_code only to those paths, plus scaffold paths. [spec: pm-spec-authoring/PM creates spec artifacts for features]
- [ ] 2.3 Update PM system prompt in `session/agent.go` `buildSystemPrompt()` — add spec creation instructions: artifact structure (proposal, design, specs, tasks), when to skip design.md, how to create spec branch and PR, `[parallel]` tagging for file-disjoint tasks, verification contract format, `Depends on:` references, and yolo vs non-yolo paths. [spec: pm-spec-authoring/*]
- [ ] 2.4 Update PM system prompt with spec PR review cycle — when a human comments on a spec PR, PM reads comments, updates spec files, pushes to same branch, comments back. [spec: pm-spec-authoring/PM refines spec in response to review comments]
- [ ] 2.5 Update PM system prompt with archive instructions — on completion, archive change, sync delta specs, commit, create archive PR (non-yolo) or commit directly (yolo). [spec: pm-spec-authoring/PM closes the spec loop on completion]

## 3. Spec PR Detection and Task Generation

- [ ] 3.1 Add spec PR detection to webhook router — on `pull_request.merged`, check if PR contains `openspec/changes/` files via `SpecPRManager.IsSpecPR`. If yes, extract change name and dispatch `event.SpecPRMerged`. [spec: speccycle/SpecPRManager detects spec PRs]
- [ ] 3.2 Add `event.SpecPRMerged` event type to `internal/event/event.go`. [spec: speccycle/SpecPRManager detects spec PRs]
- [ ] 3.3 Implement `handleSpecPRMerged` in `session/manager.go` — parse `tasks.md` from merged change, create Forgejo issues per task (one `[implementer]` issue each), create milestone, attach issues to milestone. For `[parallel]` tasks: validate file disjointness via `mergequeue.Client`, skip `Depends on:` for verified parallel tasks. [spec: speccycle/SpecPRManager generates implementer issues on spec merge]
- [ ] 3.4 Implement spec lifecycle label management — `spec-proposed` on PR open, `spec-approved` on merge, `spec-implementing` on task issue creation, `spec-complete` on archive. Add/remove labels via Forgejo API. [spec: speccycle/Spec lifecycle labels track change progress]

## 4. Implementer Spec Integration

- [ ] 4.1 Implement `openspec_get_tasks` tool — accepts `repository`, `change_name`. Reads `tasks.md` from the repo clone, returns structured task list with status and which task this session should work on. Registered for implementer role. [spec: spec-driven-implementation/openspec_get_tasks returns structured task information]
- [ ] 4.2 Implement `openspec_read_spec` tool — accepts `repository`, `capability_name`. Finds and returns spec content from `openspec/specs/<name>/spec.md` (or active change if not yet merged). Registered for implementer and reviewer roles. [spec: spec-driven-implementation/openspec_read_spec returns capability requirements]
- [ ] 4.3 Implement `openspec_mark_task` tool — accepts `repository`, `change_name`, `task_index`. Calls `SpecManager.MarkTaskComplete`. Commits and pushes the updated `tasks.md`. Registered for implementer role. [spec: spec-driven-implementation/openspec_mark_task updates task completion]
- [ ] 4.4 Update implementer system prompt — instruct implementer to call `openspec_get_tasks` and `openspec_read_spec` before coding, validate against verification contract, mark tasks complete after PR creation, and report spec deviations via `forgejo_ping_parent`. [spec: spec-driven-implementation/*]

## 5. Reviewer Spec Integration

- [ ] 5.1 Update reviewer system prompt — instruct reviewer to read spec via `openspec_read_spec`, check implementation against requirements, independently verify verification criteria, flag unmet requirements vs divergences, and respect review round cap (3 rounds). [spec: spec-driven-review/*]
- [ ] 5.2 Implement review round tracking — add `reviewRound` counter to PR sessions (stored in session metadata or derived from PR comment history), enforce 3-round cap with `needs-human-review` label escalation. [spec: spec-driven-review/Review round cap prevents infinite review loops]
- [ ] 5.3 Update `forgejo_merge_pr` tool — before merging, check that spec requirements are satisfied (if spec-driven PR). If not, block merge with explanation. [spec: spec-driven-review/Reviewer uses spec for merge decisions]

## 6. Yolo Mode and End-to-End Wiring

- [ ] 6.1 Wire yolo detection into spec flow — when repo has `fordjent-yolo` topic, PM commits specs to main directly (no spec PR), scheduler creates implementer issues immediately, archive commits go directly to main. [spec: pm-spec-authoring/PM creates spec PR for human review]
- [ ] 6.2 Add `- [ ] 5.5` (initial baseline creation for eval-harness) follow-up or verify it's handled. N/A — previous change's open task.
- [ ] 6.3 End-to-end integration test — create a test repo, file `[pm] Build CLI tool with spec`, verify: PM creates spec, spec PR created (non-yolo), human merges spec PR, scheduler creates implementer issues, implementer reads spec and implements, reviewer reviews against spec, PM archives on completion. Use the eval harness or a dedicated integration test.

## 7. Parallel Fan-Out

- [ ] 7.1 Enhance scheduler to validate `[parallel]` tasks for file disjointness via `mergequeue.Client.CheckGate`. If files overlap, fall back to serial ordering with explicit `Depends on:`. [spec: speccycle/SpecPRManager generates implementer issues on spec merge]
- [ ] 7.2 Ensure parallel implementer sessions use worktree isolation — verify existing `git clone` per session already provides isolation. No code change expected; this is verification only.

## 8. Testing and Hardening

- [ ] 8.1 Unit tests for all new tools (`openspec_get_tasks`, `openspec_read_spec`, `openspec_mark_task`) — use temp repos with pre-seeded `openspec/` directories.
- [ ] 8.2 Unit tests for spec lifecycle handlers — `handleSpecPRMerged` with mocked Forgejo API.
- [ ] 8.3 Run full `go test ./...` — all 16+ packages pass.
- [ ] 8.4 PHASE 5 (initial baseline eval) revisit if needed — coordinate with eval-harness spec.
