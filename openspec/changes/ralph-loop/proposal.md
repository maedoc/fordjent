## Why

Fordjent's implementer sessions are bounded to a configurable turn budget (typically 20–75 turns). This works well for small, well-defined tasks. But complex features with multi-step acceptance criteria frequently exhaust the budget before completion, producing `fordjent/failed:max-turns` labels and requiring human intervention. The agent has no mechanism to iteratively refine its work across multiple sessions while staying anchored to a spec.

The "ralph loop" pattern (pioneered in the miniprep project) solves this by using git history as the sole cross-session state. Each iteration starts fresh: the agent reads the git log, understands what the previous iteration achieved, plans the next step, acts, asserts, and commits. The result is an audit trail of incremental progress that survives model context limits and LLM failures.

In yolo mode (`fordjent-yolo` topic), Fordjent commits to finishing work automatically. Adding a `ralph` loop means the system can escalate a PR that doesn't satisfy acceptance criteria on the first pass into an iterative refinement cycle — without human intervention — while remaining bounded by iteration count and cost caps.

## What Changes

- **New package `internal/ralph/`**: `RalphScheduler` (ticker-based PR scanner, cooldown management), `RalphTracker` (4-A stage tool tracking, turn nudging), `RalphProgress` (progress file management, `.ralph/progress/` I/O), and `RalphGuard` (spec immutability enforcement, write_file path blocking, commit diff validation).
- **New DB table `ralph_sessions`**: Tracks per-PR iteration history, stage completions, cost, and status.
- **New LLM tool `ralph_update`**: Single tool with `stage` param (awareness/act/assert/append) and `message` summary. Harness enforces ordering, tracks completion, and nudges the model based on turn budget consumption.
- **Ralph-mode system prompt variant**: Implementer prompt augmented with 4-A protocol instructions, spec discovery guidance, and iteration-aware context injection.
- **Auto-ralph escalation in yolo mode**: When a yolo implementer session creates a PR but tests fail or AC are unmet, the harness automatically adds the `ralph` label and begins iterative refinement.
- **Spec immutability during ralph**: Spec files (`openspec/**/spec.md`) are hard-blocked from modification during ralph. The only spec-adjacent write channel is `ralph_progress`, which writes to `.ralph/progress/pr-{N}-iteration-{M}.md`.
- **QA spec TODO sync on completion**: When ralph detects all AC met and removes its label, `djent-qa` reviews `.ralph/progress/` and updates spec TODO checkboxes before the normal review/merge flow.
- **Failure recovery**: Turns-exhausted sessions trigger an LLM summary that becomes a git commit message for partial work. Stall detection (3 consecutive no-progress iterations) and budget caps prevent runaway spend.

## Capabilities

### New Capabilities
- `ralph-loop`: Iterative PR refinement engine — bounded sessions, 4-A protocol enforcement, git-centric state, spec-aware AC tracking, and automatic escalation/de-escalation.
- `ralph-guard`: Spec immutability enforcement, write_file path blocking, commit diff validation, and `.ralph/progress/` file management.
- `ralph-recovery`: Turn-budget-exhaustion summary, stall detection, budget enforcement, and gradient degradation (label-based failure instead of silent death).

### Modified Capabilities
- `implementer-session`: Augmented system prompt when in ralph mode. Additional tools registered (`ralph_update`, `ralph_progress`).
- `reviewer-session`: Extended to include spec TODO sync after ralph completion.
- `session-manager`: New scheduler ticker goroutine for ralph PR scanning. Automatic `ralph` label addition in yolo mode on incomplete PRs.
- `lifecycle`: New `ralph_sessions` table. Failure transitions for `ralph-exceeded`, `ralph-stalled`, `ralph-budget`.

## Impact

- **New packages**: `internal/ralph/` (~5 files: `scheduler.go`, `tracker.go`, `guard.go`, `progress.go`, `ralph_test.go`)
- **Modified packages**: `internal/session/agent.go` (ralph prompt variant, tool registration), `internal/session/manager.go` (ralph ticker, auto-escalation, AC verification), `internal/tool/local_tools.go` (spec path blocking, commit diff gate), `internal/lifecycle/lifecycle.go` (ralph session transitions), `internal/config/config.go` (ralph config section)
- **New labels**: `ralph` (green), `fordjent/failed:ralph-exceeded`, `fordjent/failed:ralph-stalled`, `fordjent/failed:ralph-budget`
- **New DB table**: `ralph_sessions` in `lifecycle.db`
- **New files in repo**: `.ralph/progress/*.md` (in agent workdirs, committed to PR branches)
- **Dependencies**: `internal/openspec/` (for spec discovery), `internal/scaffold/` (for language detection during build gate)
- **Zero breaking changes**: All existing issue→PR→merge flows remain untouched. Ralph is opt-in via label or yolo escalation.
