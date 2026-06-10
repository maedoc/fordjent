# Subsystem Audit: PM Role, OpenSpec Protocol, Ralph Iterative Refinement

## 1. PM Role

### (a) Wiring Status: ✅ FULLY WIRED
- **Session creation**: Issue title containing `[pm]` or `[decompose]` triggers `detectRoleFromTitle()` → role = "pm"
- **Session key**: `repo/issues/N` — no special suffix
- **Tool registry**: `buildRoleRegistry()` in `internal/session/agent.go` line 1395 — PM gets specialized tools:
  - `forgejo_create_issue` (with scheduler integration + plan-first gate, max 5 sub-issues per call)
  - `forgejo_get_sub_issues`
  - `forgejo_create_milestone` / `forgejo_set_milestone` / `forgejo_list_milestones`
  - `write_file` — **RESTRICTED**: Only allowed under `openspec/changes/` and `openspec/specs/` prefixes
  - `open_spec_get_tasks` / `open_spec_propose` / `open_spec_archive_change`
  - Common tools: comment, list_issues, get_issue, search_code, reaction, list_branches, list_prs, pr_files, list_files, list_hooks, list_collabs, version, user, sibling_issues
  - **NOT available**: bash (no shell), git (no git), write_file (full repo), forgejo_create_pr, forgejo_merge_pr

### (b) Config Flags: NONE — enabled by default
No config flag controls PM. The PM role is activated purely by issue title convention (`[pm]` or `[decompose]` prefix).

### (c) Forgejo Prerequisites
- **Labels**: FSM labels expected (`planning`, `plan-approved`, `implementing`, `ready`, `blocked`, `done`) — created by `EnsureLabels()` on webhook first event
- **Topic**: `fordjent-yolo` enables zero-friction mode (bypasses plan approval, auto-assumes implementer on untagged sub-issues)
- **Users**: `djent-pm` role user — created by `bootstrap-local.sh` but must exist on cloud Forgejo
- **Milestones**: No special prerequisite — created via API

### (d) Known Gotchas from AGENTS.md
| Issue | Description |
|-------|-------------|
| PM sometimes omits `[implementer]` tag | Sub-issues without role tags get `needs-role` label and are blocked |
| Qwen model analysis-loop (625+ turns, 0 write_file) | PM uses the same model as implementer — if implementer stucks, all roles do |
| Sub-issue ordering | Issues marked `blocked` stay blocked until scheduler unblocks them via `Depends on:` parsing |
| Milestone progress not real-time | Milestone progress bar requires sub-issues to be closed, not just PR merged |
| PM can write files but only under openspec/ paths | If PM tries to write code files in `src/` or `pkg/`, write_file will reject |
| PM reactivation loop | When PM parent's milestones close, PMReactivate events fire — `isBusy()` check prevents double-session, but needs admin client for label ops |

---

## 2. OpenSpec Protocol (Spec-Driven Lifecycle)

### (a) Wiring Status: ⚠️ PARTIALLY WIRED

**What IS wired:**
- **Spec PR detection**: `internal/speccycle/prmanager.go` — `SpecPRManager.IsSpecPR()` checks if a PR's files contain `openspec/changes/<name>/` paths via `GetPRFiles()` API call. Extracts change name from path.
- **handleSpecPRMerged**: In `manager.go` line 2218 — when a PR is merged, calls `specPRManager.IsSpecPR()`. If it's a spec PR, finds the parent issue and dispatches `ArchiveChangeRequested` to PM (line 2222).
- **Archive path**: Line 1112 — `IsArchival: evt.Type == event.ArchiveChangeRequested` → archival session with `open_spec_archive_change` tool
- **Scheduler lifecycle hooks**: `OnPRMerged()` unblocks dependent issues, closes milestones, dispatches `ArchiveChangeRequested` when all task issues are closed
- **Tasks.md tracking**: `MarkTaskInTasksMd()` updates `openspec/specs/<name>/tasks.md` with checkmarks

**What is NOT wired:**
- **No spec-authoring PM session creation**: When a feature issue like `[pm] Plan and implement X` is opened, there's **no automatic creation of spec authoring PM sessions**. The PM must manually create the spec issue within its decomposition. There's no `spec-authoring` event type or auto-spawner.
- **PM prompt mentions spec but doesn't enforce it**: The PM system prompt has "OpenSpec Workflow" section but doesn't mandate spec creation before implementation. It's a suggestion, not an enforcement.
- **No `open_spec_create_change` tool**: PM has `open_spec_get_tasks`, `open_spec_propose`, `open_spec_archive_change` — but no `open_spec_create_change` to create new spec changes. The `openspec/changes/` directory structure must be created manually.
- **`open_spec_mark_task` is ONLY for implementers**: Reviewers get `read_spec` + `read_change` + `get_tasks`, but NOT `mark_task`. Only implementers can check off completed tasks.

### (b) Config Flags: NONE — enabled by default
No config flag controls OpenSpec protocol. It runs whenever `openspec/` directory structure exists and spec-pr detection runs on merge.

### (c) Forgejo Prerequisites
- **Label**: `spec` label expected (created by `EnsureLabels()`)
- **No special topic or user needed**: Spec PRs are created by implementers under the `djent-dev` identity
- **Directory structure**: `openspec/changes/` and `openspec/specs/` dirs must exist in the repo (created by PM or initial scaffold)

### (d) Known Gotchas from AGENTS.md
| Issue | Description |
|-------|-------------|
| No auto spec authoring PM | Feature issues with `[pm]` prefix don't auto-generate spec authoring sub-issues |
| Spec creation is manual | PM must create `openspec/changes/<name>/` directory structure and spec.md manually |
| Open tasks.md not auto-trimmed | When tasks are completed, old completed entries remain in tasks.md (manual cleanup) |
| Multi-change spec PRs not supported | `isSpecPR()` returns only ONE change name even if PR contains multiple changes |
| ArchiveChangeRequested only on "all tasks closed" | PR merge unblocks dependents but doesn't trigger archive until ALL task issues are closed |

---

## 3. Ralph Iterative Refinement

### (a) Wiring Status: ❌ NOT WIRED — DORMANT CODE

**What EXISTS but is NOT wired:**
| Component | File | Status |
|-----------|------|--------|
| Ralph config struct | `internal/config/config.go:150-160` | Defined but never read |
| Ralph scheduler | `internal/ralph/scheduler.go` | `NewScheduler()` + `Start()`/`Stop()`/`ShouldSpawn()/`MarkActive()`/`IsActive()` exist |
| Ralph tracker | `internal/ralph/tracker.go` | `RalphConfig` + `DefaultRalphConfig()` + scheduler methods |
| Ralph guard | `internal/ralph/guard.go` | `RalphConfig` + `DefaultRalphConfig()` + scheduler methods (duplicate?) |
| Ralph tools | `internal/tool/ralph_tools.go` | `ralph_update`, `ralph_progress`, `ralph_status` tools defined |
| Ralph lifecycle table | `internal/lifecycle/lifecycle.go:384-583` | `ralph_sessions` table + `RecordRalphIteration()`, `GetLastRalphIteration()`, `GetRalphCost()`, `ListStalledRalphSessions()`, `CountRalphIterations()`, `GetLastNRalphIterations()` |
| Lifecycle ralph handler | `internal/session/manager.go:2052-2071` | `OnRalphAppendComplete()` — removes ralph label, adds ralph-completed |
| BuildRalphGuard | `internal/tool/registry.go:120-135` | `SetRalphGuard()` — enables spec immutability on write_file + git commit tools |

**What is MISSING (gaps preventing wiring):**
| Gap | Impact |
|-----|--------|
| **No ralph.Scheduler.Start() call** | `main.go` line 46 — `mgr.Run(ctx)` starts session manager, but nothing creates or starts `ralph.NewScheduler()`. The scheduler goroutine never runs. |
| **Ralph tools never registered** | `buildRoleRegistry()` in `agent.go` — NO case for Ralph tools (`ralph_update`, `ralph_progress`, `ralph_status`) on ANY role. |
| **No RalphGuard set** | `SetRalphGuard()` in registry is never called — spec files can be modified in ralph sessions |
| **No ralph session key detection** | `buildContext()` / session creation has no check for `ralph` label or `-ralph-iN` session key suffix |
| **`ralph.Sched.ShouldSpawn()` never called** | No auto-scan for PRs that need ralph refinement (stalled implementer → auto-activate ralph) |
| **Auto-ralph on yolo not wired** | `cfg.Ralph.AutoRalphOnYolo` config flag exists but the implementer exit path doesn't check it |
| **`ralph.Sched.IsActive()` never consulted** | Duplicate ralph session prevention not implemented |

### (b) Config Flags: ⚠️ EXIST IN YAML but NOT USED
```yaml
# In fordjent.local.yaml via DefaultRalphConfig():
ralph:
  enabled: true                      # ← NOT checked anywhere
  max_iterations_per_pr: 15          # ← NOT read
  turn_budget_per_iteration: 20      # ← NOT read
  cooldown_between_iterations: 5m    # ← NOT read
  max_cost_per_pr_usd: 10.00         # ← NOT read (lifecycle tracks cost but never enforces)
  summary_model: ""                  # ← NOT read
  nudge_threshold_pct: 0.30          # ← NOT read
  auto_ralph_on_yolo: true           # ← NOT checked
```
The config struct is defined and default values exist, but `config.Ralph` is ONLY used in `fordjent.local.yaml` serialization — never read by any component.

### (c) Forgejo Prerequisites
- **Label**: `ralph` label (to activate iterative mode)
- **Label**: `ralph-completed` label (to signal AC met)
- **Topic**: `fordjent-yolo` (for auto-escalation)
- **No special user needed**: Ralph iterations run under the implementer role user (`djent-dev`)

### (d) Known Gotchas

#### From AGENTS.md (architectural)
| Gap | Severity | Description |
|-----|----------|-------------|
| **Ralph completely dormant** | CRITICAL | No wiring to main.go, no tool registration, no scheduler start — d57b851 is the last commit for this feature and no subsequent commit wired it in |
| **Duplicate ralph package** | MEDIUM | `internal/ralph/scheduler.go` and `internal/ralph/tracker.go` both define `RalphConfig` + `DefaultRalphConfig()` + `NewScheduler()` + 7 methods — `guard.go` also has `RalphConfig` + `DefaultRalphConfig()` + `Scheduler` + `NewScheduler()` + `Start()`/`Stop()` + `ShouldSpawn()` + `MarkActive()`/`MarkInactive()` + `IsActive()`. This looks like a merge conflict or refactoring leftover. |
| **Lifecycle tracking only** | LOW | `ralph_sessions` table in `lifecycle.db` exists and has methods, but no code writes to it except `OnRalphAppendComplete()` which is never triggered |
| **Spec immutability guard unused** | LOW | `SetRalphGuard()` in registry, `guard.go` has `IsSpecPath()` + `ValidateCommitDiff()` — but `SetRalphGuard()` is never called on any registry |

#### From Bug Fixes 1-33
No prior bug fixes touched Ralph. The last Ralph-related mention in AGENTS.md was under "Gap Fixes 4-5" and "Deep Analysis" sections, describing the design. No testing or debugging was done on live Ralph sessions because it was never wired.

---

## Summary: Stress Test Readiness

| Subsystem | Wiring Status | Can Test Now? | Blocker |
|-----------|--------------|---------------|---------|
| **PM Role** | ✅ Fully wired | Yes | PM prompt quality / model choice |
| **OpenSpec Protocol** | ⚠️ Partially wired | Yes (spec PR detection + archival) | No auto spec-authoring PM; must create spec issues manually |
| **Ralph Iterative** | ❌ Not wired | No | Scheduler not started, tools not registered, no session detection |

## Start Here: Files to Open for Stress Testing

1. **PM Role**: `internal/session/manager.go` — read `handleEvent()` for topic filtering + issue title detection; `buildRoleRegistry()` for PM tool list
2. **OpenSpec**: `internal/speccycle/prmanager.go` — `IsSpecPR()` detection; `handleSpecPRMerged()` in manager.go:2218 — archival dispatch
3. **Ralph**: `internal/ralph/scheduler.go` — start wiring; `internal/session/agent.go` registerTools; `internal/session/manager.go` handleEvent → Ralph activation; `cmd/fordjent/main.go` line 46 — add scheduler start
