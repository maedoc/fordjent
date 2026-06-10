# Ralph Loop for Fordjent — Discussion Summary & Plan

*Date: 2026-06-09*
*Based on: miniprep iterate.sh/ITERATE.md analysis + user feedback*
*OpenSpec change: `openspec/changes/ralph-loop/`*

---

## 1. Origin: Problem Statement

Fordjent's bounded implementer sessions (typically 20-75 turns) work well for small, well-defined tasks. But complex features with multi-step acceptance criteria frequently exhaust the budget before completion, producing `fordjent/failed:max-turns` labels.

The miniprep project uses a "ralph loop" pattern that keeps making progress by spawning new pi processes for each iteration, using git history as the sole cross-session state. The agent reads the git log to figure out what happened, plans the next step, acts, asserts, documents in the commit message, and pushes.

---

## 2. Initial Brainstorming

### 2.1 What the Ralph Loop Is (miniprep)

- **`iterate.sh`**: infinite `while true` bash loop, spawns fresh `pi --no-session` every 10 seconds
- **`ITERATE.md`**: 4-A protocol (awareness, act, assert, append)
  - **Awareness**: read `git log`, `git diff`, `PLAN.md`, choose next effort
  - **Act**: work on task to completion with tests
  - **Assert**: verify with real data, document results in git message (do NOT fix — document gaps)
  - **Append**: stage, commit with proper message, push
- **`json_status.py`**: live progress telemetry (turns, tokens, cost, phase)

**Key insight**: Each iteration is a brand-new pi session. There is no memory across iterations except git history and `PLAN.md`.

### 2.2 How Fordjent Differs

| Dimension | Miniprep Ralph | Fordjent (today) |
|---|---|---|
| Loop driver | Bash `while true` | Webhook → session manager → bounded `for` loop |
| Session model | New pi process per iteration | Single long-lived session |
| State persistence | Git + PLAN.md | `memory.jsonl` + SQLite lifecycle DB |
| Trigger | Cron-like polling | Webhook event |
| Termination | Infinite (human kills) | `max_turns` or error |

---

## 3. Design Evolution Through Discussion

### 3.1 Initial Proposal (Brainstorm)

**Trigger**: `ralph` label on a PR → activates iterative refinement mode.

**Architecture**: Three approaches considered:
- **A. One infinite session** — rejected (context window exhaustion)
- **B. Session per iteration** — clean but loses cross-iteration context
- **C. Hybrid chunked sessions** — **selected** (balances context and continuity)

**Key risks identified**:
- API cost burn (unbounded iterations)
- Agent-comment feedback loop (webhooks on iteration comments)
- Context window exhaustion
- Human can't override mid-loop
- Stuck on same error
- CI not ready between iterations

### 3.2 User Feedback & Refinements

#### Refinement 1: Source of Truth

> *"the git log gets stuffed with useful reasoning about what works and what doesn't"*

Decision: **Git commits ARE the iteration log.** No comment noise, no PR spam. Commit messages carry the reasoning. Time tracking covers metadata.

#### Refinement 2: Yolo Auto-Escalation

> *"adding a ralph label to a PR might be a way to give 'unlimited' turns to djent-dev"*
> *"yolo mode could imply ralph on PR automatically as long as AC ensure termination"*

Decision: **Yolo mode (`fordjent-yolo` topic) auto-escalates incomplete PRs.**

After an implementer session creates a PR, if AC verification fails (build fail, test fail, unchecked spec TODO), the harness automatically adds the `ralph` label and queues the first iteration.

This means yolo mode truly commits to finishing work — the system does not declare victory until the spec is satisfied.

#### Refinement 3: Cost Safety Measures

Three measures to avoid infinite API cost:
1. **Label check**: Before each iteration, verify `ralph` label still present (human can remove to stop)
2. **AC self-removal**: Before ending ralph, agent verifies all acceptance criteria are truly complete and removes its own `ralph` label
3. **Context exhaustion**: Avoided by compaction + each session being new. Previous sessions searchable but not stuffed into context by default

Additional caps added: `max_iterations_per_pr`, `max_cost_per_pr_usd`.

#### Refinement 4: 4-A Protocol Baked Into Harness

> *"The harness can track explicitly what the LLM says for each stage of the 4-A protocol for later inspection"*

Decision: **Option B — enforce via tools, not just prompts.**

A single tool `ralph_update(stage, message)` is registered during ralph mode. The harness:
- Validates stage ordering (awareness → act → assert → append)
- Tracks which stages are completed per session
- Nudges the model based on turn budget consumption (25/50/75% thresholds)
- Each session's stages are persisted for later inspection

#### Refinement 5: Spec Immutability (Critical)

> *"ralph loop can update spec todos but should not change the scope or AC of the spec — this is important for some models who are inclined to make the spec easier in order to pass"*

Decision: **Multi-layer defense**:

1. **`write_file` blocking**: During ralph sessions, any path matching `openspec/**/spec.md` is rejected
2. **`git commit` diff gate**: Before committing, inspect staged files. Any spec file → reject commit
3. **`ralph_progress` tool**: The only spec-adjacent write channel. Writes to `.ralph/progress/pr-{N}-iteration-{M}.md` (not the spec itself)

Spec files are completely immutable during ralph. The only exception is QA reviewer sync after ralph completion (see below).

#### Refinement 6: QA Spec TODO Sync

> *"when ralph AC met, it can be the djent-qa role job to update spec TODO based on .ralph/progress?"*

Decision: **Yes — QA bridges progress files and spec checkboxes.**

Flow:
1. Ralph detects all AC met → removes `ralph` label, adds temporary `ralph-completed` label
2. Reviewer session (djent-qa) starts with special prompt
3. QA reads `.ralph/progress/*.md`, checks off completed TODOs in spec
4. Commits spec update with `docs:` prefix
5. Removes `ralph-completed` label
6. Normal review/merge flow proceeds

QA is restricted to **checkbox updates only** — no rewording, no scope changes, no adding/removing TODOs.

#### Refinement 7: Failure Recovery

> *"if we get to end of turn budget without progress, we can request a LLM summary and use that as the git log message for committing partial results"*

Decision: **Turns-exhausted sessions trigger LLM summary commit.**

- Fast/cheap model summarizes session memory
- Whatever exists in workdir is committed with message: `ralph-iN [incomplete]: <summary>`
- Iteration recorded as `failed_turns`
- Next iteration scheduled normally

Stall detection: 3 consecutive failed iterations → `fordjent/failed:ralph-stalled` label, ralph removed.

#### Refinement 8: Human Comments During Ralph

Human comments on a PR with `ralph` label are picked up in the next iteration's **Awareness** phase (reading recent PR comments). No special session needed — it becomes input to the next loop cycle.

### 3.3 User Disagreements / Overrides of My Suggestions

| My Suggestion | User Response | Final Decision |
|---|---|---|
| Ralph as manual label on PR | **Override**: Yolo mode should auto-escalate | Auto-ralph in yolo; manual `ralph` label for non-yolo |
| Ralph progress stored in DB rows | **Override**: Git commits are the log | `.ralph/progress/*.md` files committed to branch |
| Separate `djent-ralph` role | **Keep**: `djent-dev` is fine | `[ralph]` prefix in commit messages for filtering |
| Comment-based iteration log | **Override**: Git commits + time tracking | No comment spam; commits carry reasoning |
| No spec on branch → skip AC | **Agree** with fallback to build/test only | Graceful degradation to build gate |
| Repo topic for ralph opt-in | **Override**: Per-PR label is opt-in; yolo is topic-level | `ralph` label = per-PR; `fordjent-yolo` = auto-ralph |
| Spec file modification by ralph | **Strong override**: Absolutely forbidden | Multi-layer guard; only QA sync allowed after completion |

---

## 4. Final Architecture

### 4.1 Activation

Three paths into ralph mode:
1. **Manual**: Human adds `ralph` label to an open PR
2. **Yolo auto-escalation**: Implementer creates PR, AC verification fails → `ralph` label auto-added
3. **Ralph PR merged, new PR created**: If a ralph PR is closed without merging, human can open new PR and label it

### 4.2 Outer Loop (Manager-Level Ticker)

```
Every 2 minutes:
  Scan all open PRs with 'ralph' label
  For each PR:
    If last iteration completed AND cooldown elapsed:
      Compute next iteration number
      Read last SHA from git
      Checkout PR branch, rebase origin/main
      Spawn new bounded implementer session (20 turns)
      Register ralph_update + ralph_progress tools
      Inject 4-A system prompt variant
```

### 4.3 Inner Loop (Per-Session 4-A Protocol)

```
Turn 1-5:   awareness  → ralph_update(stage='awareness')
Turn 6-12:  act        → ralph_update(stage='act')
Turn 13-17: assert     → ralph_update(stage='assert') + evidence
Turn 18-20: append     → ralph_update(stage='append') → auto-commit + push
```

Nudging at 25/50/75% turn budget thresholds if stages skipped.

### 4.4 Completion & De-escalation

```
After append stage:
  AC verification (spec TODOs + build/test):
    All met → remove 'ralph', add 'ralph-completed' → queue QA sync
    Unmet  → schedule next iteration after cooldown
```

### 4.5 Failure Paths

| Failure | Action |
|---|---|
| Turns exhausted without append | LLM summary → partial commit → `failed_turns` |
| 3x consecutive `failed_turns` | `fordjent/failed:ralph-stalled` → label removed |
| `max_iterations` exceeded | `fordjent/failed:ralph-exceeded` → label removed |
| `max_cost` exceeded | `fordjent/failed:ralph-budget` → label removed |
| Label removed by human | Graceful stop, no new iterations |
| PR merged/closed | Natural end, cleanup |

---

## 5. OpenSpec Change Structure

Created at: `openspec/changes/ralph-loop/`

### Artifacts

| Artifact | Description |
|---|---|
| `proposal.md` | Why ralph, what changes, capabilities, impact |
| `design.md` | Architecture, data flow, interfaces, decisions, testing |
| `specs/ralph-scheduler/spec.md` | Ticker, auto-escalation, AC verification, caps |
| `specs/ralph-protocol/spec.md` | ralph_update tool, nudging, prompt variant, append commit |
| `specs/ralph-guard/spec.md` | Spec immutability, write_file blocking, commit gate, QA exception |
| `specs/ralph-recovery/spec.md` | Timeout summary, stall detection, budget enforcement |
| `specs/ralph-qa-sync/spec.md` | QA TODO sync from progress files, label lifecycle |
| `tasks.md` | 33 implementation tasks in 7 groups, dependency-ordered |

### New Package

- `internal/ralph/` — `tracker.go`, `progress.go`, `guard.go`, `scheduler.go`, `ralph_test.go`

### Modified Packages

- `internal/session/agent.go` — Ralph prompt variant, tool registration, nudging
- `internal/session/manager.go` — Ticker integration, session factory, AC verification
- `internal/tool/local_tools.go` — Spec path blocking, commit diff gate
- `internal/lifecycle/lifecycle.go` — `ralph_sessions` table, transitions
- `internal/config/config.go` — `RalphConfig` struct
- `internal/forgejo/client.go` — New labels: `ralph`, `ralph-completed`, failure labels

---

## 6. Key Decisions Log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | Session per iteration (not infinite) | Fresh context window, no compaction degradation |
| 2 | Single `ralph_update` tool vs 4 tools | Smaller models handle one tool better |
| 3 | Git commits as audit trail (not comments) | No noise, readable in `git log`, no webhook loops |
| 4 | Yolo auto-escalation | "Commit to finish" contract in yolo mode |
| 5 | Spec immutability (multi-layer) | Prevent models from making AC easier to pass |
| 6 | QA syncs spec TODOs (not ralph agent) | Separation of concerns; reviewer verifies completion |
| 7 | Manager-level ticker (not per-PR goroutines) | Simpler monitoring, no goroutine leaks |
| 8 | `.ralph/progress/*.md` files (not DB rows) | Git is single source of truth; searchable, blameable |
| 9 | AC verification from spec + build/test (not just build) | Catch "build passes but AC unmet" scenarios |
| 10 | Fast LLM summary on timeout turns | Document partial work for next iteration |
| 11 | Human comments feed into next awareness phase | No special session; natural loop flow |
| 12 | Spec files read-only during ralph; only QA sync allowed after | Scope creep prevention |

---

## 7. Config (fordjent.local.yaml additions)

```yaml
ralph:
  enabled: true
  max_iterations_per_pr: 20
  turn_budget_per_iteration: 20
  cooldown_between_iterations: "2m"
  max_cost_per_pr_usd: 5.00
  nudge_threshold_pct: 50
  summary_model: "fast"
  auto_ralph_on_yolo: true
```

---

## 8. Remaining Open Questions (Post-Discussion)

| # | Question | Status |
|---|----------|--------|
| 1 | Should ralph sessions have a different turn budget than standard implementer? | Configurable via `turn_budget_per_iteration` (default 20) |
| 2 | What happens if rebase fails during session setup? | Agent starts with conflict-warning in prompt; iteration consumed resolving conflicts |
| 3 | Should ralph iterations show up differently in the status dashboard? | Yes — add ralph section (active PRs, iteration counts, cost burn) |
| 4 | How does ralph interact with the merge queue? | No direct interaction; ralph PRs are NOT ready to merge. Merge queue only matters after `ralph` removed |
| 5 | What if spec is on `main` but not on the PR branch? | AC verification falls back to linked issue body or build/test gate only |
| 6 | Should `ralph` label be visually distinct? | Green color, distinct from `automerge` |
| 7 | Can humans pause ralph without removing label? | `ralph-pause` label could be added; future enhancement |

---

## 9. Implementation Order

From `tasks.md`:

1. **P0**: Data model (`ralph_sessions` table), config, `internal/ralph/` package foundation
2. **P1**: `ralph_update` tool, session integration, turn nudging
3. **P2**: Spec immutability guards (write_file + commit gate)
4. **P3**: Scheduler ticker, auto-escalation, AC verification
5. **P4**: Recovery (timeout summary, stall detection, budget cap)
6. **P5**: QA spec TODO sync
7. **P6**: End-to-end integration testing + manual validation

---

## 10. References

- miniprep ralph loop: `~/src/miniprep/iterate.sh`, `~/src/miniprep/ITERATE.md`
- OpenSpec change: `~/src/fordjent/openspec/changes/ralph-loop/`
- Fordjent design doc: `~/src/fordjent/PLAN.md` (Phase 2: Ralph Loop Resilience)
- Existing ralph branch: `origin/feat/ralph-loop` (commit `42b84e75`)
