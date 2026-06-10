## 1. Foundation: `internal/ralph/` package — Data Model & Config

- [x] 1.1 Create `internal/ralph/tracker.go` with `Tracker` struct — stage tracking (awareness/act/assert/append), ordering validation, turn-based nudging (`ShouldNudge`), and `IsComplete()`. Includes `Reset()` for new iterations. [spec: ralph-protocol/ralph_update tool enforces 4-A stage ordering]
- [x] 1.2 Create `internal/ralph/progress.go` with `WriteProgress`, `ReadProgress`, `ListProgress` — writes markdown files to `.ralph/progress/pr-{N}-iteration-{M}.md` with stage summaries. Handles directory creation and idempotent overwrites. [spec: ralph-protocol/append stage commits and pushes progress]
- [x] 1.3 Create `internal/ralph/guard.go` with `Guard` struct — `IsSpecPath(path)`, `ValidateCommitDiff(diff)`, `IsProgressPath(path)`. Uses path prefix matching and resolved path checks. [spec: ralph-guard/write_file blocks spec paths during ralph mode]
- [x] 1.4 Create `internal/ralph/scheduler.go` with `Scheduler` struct — ticker-based PR scanner, iteration dispatch logic, cooldown enforcement, active iteration tracking (`map[string]bool`). Methods: `Start()`, `Stop()`, `scanAndDispatch()`, `shouldSpawn()`, `markActive()`, `markInactive()`. [spec: ralph-scheduler/Ralph scheduler scans for ralph-labeled PRs]
- [x] 1.5 Add `RalphConfig` struct to `internal/config/config.go` with all fields: `Enabled`, `MaxIterationsPerPR`, `TurnBudgetPerIteration`, `CooldownBetweenIterations`, `MaxCostPerPRUSD`, `NudgeThresholdPct`, `SummaryModel`, `AutoRalphOnYolo`. Add validation in `validate()`. [spec: ralph-scheduler/Ralph scheduler respects hard caps]
- [x] 1.6 Add `ralph_sessions` table creation to `internal/lifecycle/lifecycle.go` — schema with all columns, indexes on `pr_key` and `status`. Add helper: `RecordRalphIteration(rec *RalphRecord)`, `GetLastRalphIteration(prKey)`, `GetRalphCost(prKey)`, `ListStalledRalphSessions()`. [spec: ralph-recovery/Stall detection prevents infinite loops]
- [x] 1.7 Write `internal/ralph/ralph_test.go` — unit tests for `Tracker` (ordering, nudging, completion), `Guard` (spec detection, commit diff validation), `Progress` (write/read/list). All tests use temp directories, no external services. [spec: all ralph specs]
- [x] 1.8 Run `go vet ./internal/ralph/...` and `go test ./internal/ralph/...` — all pass.

## 2. Ralph Protocol Tool & Session Integration

- [x] 2.1 Register `ralph_update` tool in `internal/tool/ralph_tools.go` — accepts `stage` and `message`. Calls `ralph_tracker.RecordStage()` and returns acknowledgment or error. Tool description includes stage ordering and the 4-A protocol. [spec: ralph-protocol/ralph_update tool enforces 4-A stage ordering]
- [x] 2.2 Register `ralph_progress` tool in `internal/tool/ralph_tools.go` — accepts `message`. Calls `ralph.WriteProgress()` and returns file path. Only available when `IsRalphSession` is true. [spec: ralph-guard/ralph_progress tool writes to safe path]
- [x] 2.3 Implement ralph session detection in `internal/session/agent.go` — `IsRalphSession(key string) bool` (matches `-ralph-i` suffix). When true, injects ralph section into implementer system prompt, registers `ralph_update` and `ralph_progress` tools, sets `turnBudget = ralph.TurnBudgetPerIteration`. [spec: ralph-protocol/Ralph system prompt variant guides agent behavior]
- [x] 2.4 Implement turn nudging in `internal/session/agent.go` `ProcessEvent()` — before each LLM turn, calls `tracker.ShouldNudge(turn)` and injects steering message if needed. Tracks nudges per threshold to avoid duplicates. [spec: ralph-protocol/Turn-based nudging guides model through protocol]
- [x] 2.5 Implement append stage auto-commit in `internal/session/agent.go` — when `ralph_update(stage="append")` succeeds, automatically stages changes, writes progress file, commits with formatted message including iteration number and all stage summaries, and pushes to PR branch. [spec: ralph-protocol/append stage commits and pushes progress]
- [x] 2.6 Write integration tests in `internal/session/` — mock ralph session creation, verify prompt includes ralph section, verify `ralph_update` tool registered, verify out-of-order stage returns error. [spec: ralph-protocol]

## 3. Ralph Guard & Spec Immutability

- [x] 3.1 Integrate `RalphGuard.IsSpecPath()` into `internal/tool/local_tools.go` `writeFileTool.Execute()` — during ralph sessions, reject paths matching `openspec/**/spec.md` with descriptive error. Include resolved path check for traversal attempts. [spec: ralph-guard/write_file blocks spec paths during ralph mode]
- [x] 3.2 Integrate `RalphGuard.ValidateCommitDiff()` into `internal/tool/local_tools.go` `gitTool.Execute()` — before executing `git commit`, run `git diff --cached --name-only`, reject if any path matches spec pattern. Error includes the offending file paths. [spec: ralph-guard/git commit gate rejects spec modifications]
- [x] 3.3 Write integration tests for guard — mock ralph session, attempt `write_file` on spec path → assert error. Attempt `git commit` with staged spec changes → assert error. Attempt `write_file` on non-spec openspec path → assert success. [spec: ralph-guard]
- [x] 3.4 Write tests for reviewer spec sync exception — mock reviewer session with `ralph-completed` label, verify `write_file` allows spec path modification, verify commit gate allows spec changes. [spec: ralph-guard/Spec immutability does not block reviewer spec sync]

## 4. Ralph Scheduler & Auto-Escalation

- [x] 4.1 Wire `RalphScheduler` into `internal/session/manager.go` — instantiate in `NewManager`, start in `Manager.Start()`, stop in `Manager.Shutdown()`. Scheduler receives manager, forgejo client, and config. [spec: ralph-scheduler/Ralph scheduler scans for ralph-labeled PRs]
- [x] 4.2 Implement `scanAndDispatch()` — list open PRs via `forgejo.ListOpenPRsByLabel()`, filter by `ralph` label, check cooldown and iteration caps, call `Manager.createRalphSession(pr, iterNum)`. Handle pagination for large repos. [spec: ralph-scheduler/Ralph scheduler scans for ralph-labeled PRs]
- [x] 4.3 Implement ralph session factory in `internal/session/manager.go` — compute iteration number from DB, read last SHA from git, checkout PR branch, rebase `origin/main`, build session with ralph prompt variant, spawn via goroutine. [spec: ralph-scheduler/Ralph session factory prepares workdir and context]
- [x] 4.4 Implement auto-ralph escalation in yolo mode — after implementer session creates a PR and `m.verifyAC()` returns false, call `forgejo.AddIssueLabel(pr, "ralph")` and queue first iteration (no cooldown). [spec: ralph-scheduler/Yolo mode auto-escalates incomplete PRs to ralph]
- [x] 4.5 Implement AC verification `verifyAC(pr)` — read active spec from branch or linked issue body, check TODO checkboxes, run build/test gate, return `ACResult{Met bool, Unmet []string, BuildOK bool, TestsOK bool}`. [spec: ralph-scheduler/AC verification detects completion and removes ralph label]
- [x] 4.6 Implement ralph label removal on AC completion — after `append` stage, call `verifyAC()`. If all met: remove `ralph`, add `ralph-completed`, queue reviewer session. If not: schedule next iteration after cooldown. [spec: ralph-scheduler/AC verification detects completion and removes ralph label]
- [x] 4.7 Implement cap enforcement in scheduler — before spawning, query total iterations and cost from `lifecycle.db`. Block if caps exceeded, add failure labels. [spec: ralph-scheduler/Ralph scheduler respects hard caps]
- [x] 4.8 Write integration tests in `internal/session/` — mock Forgejo with labeled PRs, verify scheduler spawns iterations at correct intervals, verify cooldown enforcement, verify cap blocking. [spec: ralph-scheduler]

## 5. Ralph Recovery: Timeout Summary & Stall Detection

- [x] 5.1 Implement timeout summary in `internal/session/manager.go` — when ralph session exhausts turns without `append`, call fast LLM with session memory (last N messages), generate summary. Stage workdir files, commit with `ralph-iN [incomplete]:` prefix + summary. Record as `failed_turns`. [spec: ralph-recovery/Timeout summary generates commit from partial work]
- [x] 5.2 Implement stall detection in `internal/session/manager.go` — before spawning next iteration, query last 3 iterations for PR. If all have status `failed_turns` AND no new commits, add `fordjent/failed:ralph-stalled`, remove `ralph`, post comment. [spec: ralph-recovery/Stall detection prevents infinite loops]
- [x] 5.3 Implement budget tracking — accumulate cost per PR from `ralph_sessions` table. Enforce `max_cost_per_pr_usd` in scheduler before spawn. [spec: ralph-recovery/Budget cap prevents runaway spend]
- [x] 5.4 Add `fordjent/failed:ralph-exceeded` and `fordjent/failed:ralph-stalled` and `fordjent/failed:ralph-budget` labels to `EnsureLabels()` in `internal/forgejo/client.go`. [spec: ralph-recovery]
- [x] 5.5 Write integration tests for recovery — simulate 3 failed iterations → assert stall label added. Simulate cost cap exceeded → assert budget label added. [spec: ralph-recovery]

## 6. QA Spec Sync

- [x] 6.1 Implement QA reviewer spec sync in `internal/session/agent.go` — when reviewer session starts on PR with `ralph-completed` label, inject reviewer prompt variant: "Read .ralph/progress/*.md, update spec TODO checkboxes, commit and push." Temporarily allow spec writes. [spec: ralph-qa-sync/QA reviewer syncs spec TODOs from ralph progress]
- [x] 6.2 Implement spec sync commit flow — reviewer reads progress files, matches completed work to spec TODOs, updates checkboxes only (no scope changes), commits with `docs:` prefix, pushes. Remove `ralph-completed` label. [spec: ralph-qa-sync/QA reviewer syncs spec TODOs from ralph progress]
- [x] 6.3 Implement reviewer commit gate exception — during reviewer sync, `git commit` gate skips spec path validation. Gate checks `ralph-completed` label presence before allowing. [spec: ralph-qa-sync/QA reviewer does not modify unchecked TODOs]
- [x] 6.4 Implement missing spec / missing progress file handling — if no spec or no progress files, log warning, remove `ralph-completed`, proceed with normal review. [spec: ralph-qa-sync/Missing spec on branch skips QA sync gracefully]
- [x] 6.5 Implement `ralph-completed` label cleanup — scheduled cleanup task removes label after 1 hour if reviewer session never completes. Prevents label orphaning. [spec: ralph-qa-sync/ralph-completed label is temporary]
- [x] 6.6 Write integration tests for QA sync — mock PR with `ralph-completed`, progress files, and spec. Verify reviewer checks correct TODOs, ignores unchecked ones, commits with proper prefix, label removed. [spec: ralph-qa-sync]

## 7. End-to-End Integration & Testing

- [x] 7.1 Add `ralph` label color and description to `EnsureLabels()` in `internal/forgejo/client.go` — green color, description "Iterative refinement mode". [spec: ralph-scheduler]
- [x] 7.2 Update status dashboard (`internal/webhook/router.go`) to include ralph section — active ralph PRs, iteration counts, current stage, cost burn, last commit SHA. [spec: design/Data Flow]
- [x] 7.3 Run `go test ./internal/ralph/... ./internal/session/...` — all existing tests still pass, new ralph tests pass. [spec: all ralph specs]
- [ ] 7.4 Manual end-to-end validation — create a repo with `fordjent-yolo`, file an implementer issue with a complex spec (e.g., "Implement a concurrent safe LRU cache with 5 acceptance criteria"). Verify: auto-ralph escalation, 3+ iterations, progress files committed, spec immutability enforced, ralph label removed on AC completion, QA sync updates spec TODOs. [spec: all ralph specs]
- [x] 7.5 Document ralph mode in `AGENTS.md` — activation (label or yolo), protocol (4-A), config options, failure modes, debugging tips. [spec: design/Context]
