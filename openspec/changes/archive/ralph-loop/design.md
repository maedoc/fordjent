## Context

Fordjent processes issues via webhook-driven bounded sessions. Each session runs a `for` loop of up to `max_turns` LLM turns, then terminates. This architecture has proven reliable for small-to-medium tasks but hits three hard problems:

1. **Context window exhaustion**: Complex tasks (multi-file refactors, parser implementations, integration wiring) burn context. Auto-compaction helps but degrades quality.
2. **Stuck agents**: A model can call the same tool with the same arguments across multiple turns, producing zero diff.
3. **Max-turns failures**: Once the budget is exhausted, the session dies with a failure label. There's no recovery path.

The miniprep project solves these by making each iteration a **brand new pi process** (`--no-session`). The agent grounds itself by reading `git log` and `PLAN.md`, not by remembering a conversation. Commit messages carry the reasoning audit trail forward.

Fordjent cannot spawn new OS processes per iteration — it's a long-running Go binary. But it CAN spawn **new bounded sessions** against the same PR branch, using git as the shared state medium. This design adapts the ralph loop pattern to Fordjent's webhook-centric architecture.

## Goals / Non-Goals

**Goals:**
- Iterative refinement of PRs that don't satisfy acceptance criteria on first pass
- New session per iteration (resets context, avoids accumulation)
- 4-A protocol (awareness/act/assert/append) enforced via tools and turn-based nudging
- Git-centric state: the commit graph on the PR branch IS the audit trail
- Spec immutability: ralph cannot relax AC by editing specs
- Yolo mode auto-escalation: incomplete PRs automatically enter ralph
- Bounded by iteration count, turn budget per iteration, and cost cap per PR
- Graceful degradation: partial commits on timeout, LLM summary for next iteration
- QA spec TODO sync: djent-qa updates spec checkboxes from `.ralph/progress/` after completion
- Zero new external dependencies

**Non-Goals:**
- Ralph for spec writing (spec authoring is the PM role's job, done before implementation)
- Ralph for issues without PRs (no branch to iterate on)
- Replacing Fordjent's existing bounded session (ralph is an outer scheduling layer)
- Automatic conflict resolution (rebase failures are surfaced to the agent)
- Spec modification during ralph (explicitly out of scope — blocked)
- Requiring the OpenSpec CLI (all file ops in Go stdlib)
- Real-time collaboration with humans during ralph (humans comment/label, agent reads in next awareness phase)

## Decisions

### Decision 1: Manager-level ticker, not per-PR goroutines

**Choice**: A single `time.Ticker` in the session manager scans all open PRs every `cooldown_between_iterations` (default 2m). For each PR with the `ralph` label, it checks if the last iteration has completed and the cooldown has elapsed, then spawns the next iteration.

**Alternate considered**: One goroutine per active ralph PR, each with its own timer.

**Rationale**:
- Single goroutine is simpler to monitor, log, and test.
- No goroutine leak risk if a PR is suddenly merged or labeled.
- Cooldown is a best-effort throttle, not a hard real-time guarantee. Ticker jitter is acceptable.
- Easier to implement inertness detection ("ticker hasn't scanned in 5 minutes" = bug).

### Decision 2: Session key per iteration, not per PR

**Choice**: Each ralph iteration is a separate Fordjent session with key `repo/pulls/N-ralph-iM` (e.g., `fjadmin/testbed/pulls/42-ralph-i7`). The session is created fresh, runs up to the turn budget, and is then destroyed.

**Alternate considered**: Reuse the same session key across iterations, appending turns to `memory.jsonl`.

**Rationale**:
- Fresh session = fresh context window. No compaction quality degradation.
- Each session's memory is isolated; debugging a failed iteration means reading one file.
- Session recovery (`enable_session_recovery`) works per-iteration. If one iteration fails, the next iteration starts clean.
- The existing session manager code paths don't need modification for "session reuse" semantics.

### Decision 3: Single `ralph_update` tool instead of four separate tools

**Choice**: One tool with `stage` enum parameter. The harness tracks which stages have been completed in the current session and enforces ordering.

**Alternate considered**: Four separate tools (`ralph_awareness`, `ralph_act`, `ralph_assert`, `ralph_append`).

**Rationale**:
- Smaller models (12B, 35B) struggle with large tool schemas. One tool vs. four is a meaningful reduction.
- The `stage` parameter is a single string. The model learns the sequence once (A→A→A→A).
- Ordering enforcement is in Go harness code, not the model's reasoning. The harness returns clear errors: `"Error: must call ralph_update with stage='awareness' before 'act'"`.
- Testing shows single tool is sufficient; easy to split later if needed.

### Decision 4: `.ralph/progress/` files, not DB rows, for iteration detail

**Choice**: The `ralph_update` tool stores the agent's stage summary in a markdown file committed to the PR branch. The DB only tracks iteration metadata (number, status, cost, SHA). The content lives in git.

**Alternate considered**: Store stage summaries as JSON columns in `ralph_sessions`.

**Rationale**:
- Git is the single source of truth. A human reading the PR diff sees `.ralph/progress/pr-42-iteration-7.md` without needing DB access.
- Content is searchable via `git log --grep` and `git blame`.
- No DB schema migration needed when we add new stage types.
- `.ralph/` directory is `.gitignore`-friendly if repos want to exclude it from merge.

### Decision 5: Spec immutability via commit diff gate, not just write_file blocking

**Choice**: Two-layer defense: `write_file` rejects spec paths during ralph, AND the `git` tool's commit handler inspects the staged diff and rejects any commit touching `openspec/**/spec.md`.

**Alternate considered**: Only write_file blocking (lighter weight).

**Rationale**:
- Write_file blocking catches the obvious path.
- Bash tool can bypass write_file via `echo "x" > openspec/foo/spec.md`. The commit gate is the backstop.
- The commit gate is also where protected branch checks already run. It's a natural enforcement point.
- Both layers are cheap (simple string prefix checks on file paths).

### Decision 6: Yolo auto-ralph triggers on AC gap, not just test/build failure

**Choice**: After an implementer session creates a PR in yolo mode, the harness runs spec-driven AC verification before declaring the session complete. If ANY acceptance criterion is unmet (unchecked TODO in spec, missing test, build fail), the PR is auto-ralph'd.

**Alternate considered**: Only auto-ralph on build/test failure.

**Rationale**:
- A PR can build and pass tests but still miss AC (e.g., "must handle empty input edge case" — if no test for empty input, the spec AC is unmet).
- The whole point of yolo is "commit to finish." Auto-ralph ensures the system doesn't declare victory prematurely.
- AC verification reuses the same spec-reading logic that ralph uses once active.

## Data Flow

```
┌──────────────────────────────────────────────────────────────────────┐
│                       Ralph Scheduler Ticker                          │
│  Every 2 minutes:                                                     │
│    1. List open PRs with 'ralph' label                                │
│    2. For each PR, check last ralph iteration status                  │
│    3. If completed AND cooldown elapsed → spawn next iteration        │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│                    Ralph Session Factory                              │
│  - Compute iteration number (max existing + 1)                        │
│  - Read last committed SHA from git                                   │
│  - Generate session key: repo/pulls/42-ralph-i7                       │
│  - Checkout PR branch in workdir                                      │
│  - Rebase origin/main if needed                                       │
│  - Inject 4-A system prompt variant                                   │
│  - Register ralph_update + ralph_progress tools                       │
│  - Set turn budget = ralph.turn_budget_per_iteration (default 20)    │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│                    Bounded Implementer Session                        │
│  Turn 1-5:   Agent calls ralph_update(stage='awareness')              │
│              → reads git log, PR comments, spec files                 │
│  Turn 6-12:  Agent calls ralph_update(stage='act')                    │
│              → writes code, runs build/tests                          │
│  Turn 13-17: Agent calls ralph_update(stage='assert')                 │
│              → verifies results, documents gaps                       │
│  Turn 18-20: Agent calls ralph_update(stage='append')                 │
│              → commits, pushes, writes progress file                  │
│  (Turn nudging enforced at 25/50/75% thresholds)                      │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│                    Session Completion Handler                         │
│  If append completed:                                                 │
│    → Record iteration in DB                                           │
│    → Check AC (spec TODOs + test/build gate)                          │
│    → If all AC met: remove 'ralph' label, queue QA review             │
│    → If not: schedule next iteration (after cooldown)                 │
│  If turns exhausted without append:                                   │
│    → Call LLM summary (fast model)                                    │
│    → Commit partial work with summary                                 │
│    → Record failed_turns, schedule retry                              │
│  If 3 consecutive failed_turns:                                       │
│    → Label 'fordjent/failed:ralph-stalled', remove 'ralph'            │
└──────────────────────────────────────────────────────────────────────┘
```

## Interfaces

### RalphScheduler

```go
package ralph

type Scheduler struct {
    mgr         *session.Manager
    forgejo     *forgejo.Client
    cfg         *config.RalphConfig
    ticker      *time.Ticker
    mu          sync.Mutex
    active      map[string]bool // prKey -> iteration running
}

func NewScheduler(mgr *session.Manager, forgejo *forgejo.Client, cfg *config.RalphConfig) *Scheduler
func (s *Scheduler) Start()                          // starts ticker goroutine
func (s *Scheduler) Stop()                           // stops ticker
func (s *Scheduler) scanAndDispatch(ctx context.Context)
func (s *Scheduler) shouldSpawn(pr *forgejo.PullRequest, last *RalphIterationRecord) bool
func (s *Scheduler) markActive(prKey string)
func (s *Scheduler) markInactive(prKey string)
```

### RalphTracker

```go
type Tracker struct {
    stagesCompleted map[string]bool // within current session
    turnBudget      int
    nudgeThresholds []float64       // [0.25, 0.50, 0.75]
}

func NewTracker(turnBudget int) *Tracker
func (t *Tracker) RecordStage(stage string) error          // validates ordering
func (t *Tracker) ShouldNudge(turn int) (string, bool)    // returns nudge message if any
func (t *Tracker) IsComplete() bool                        // all 4 stages done
func (t *Tracker) Reset()                                  // new iteration
```

### RalphGuard

```go
type Guard struct {
    repoDir        string
    scopePrefixes  []string
}

func NewGuard(repoDir string) *Guard
func (g *Guard) IsSpecPath(path string) bool               // openspec/**/spec.md
func (g *Guard) ValidateCommitDiff(diff string) error      // reject spec changes
func (g *Guard) IsProgressPath(path string) bool           // .ralph/progress/*.md
```

### RalphProgress

```go
type Progress struct {
    PRNumber   int
    Iteration  int
    Stage      string
    Message    string
    Timestamp  time.Time
}

func WriteProgress(repoDir string, prNum, iter int, stage, message string) (string, error) // returns file path
func ReadProgress(repoDir string, prNum int, iter int) (*Progress, error)
func ListProgress(repoDir string, prNum int) ([]*Progress, error)
```

### Config Additions

```go
// internal/config/config.go

type RalphConfig struct {
    Enabled                   bool          `yaml:"enabled"`
    MaxIterationsPerPR        int           `yaml:"max_iterations_per_pr"`
    TurnBudgetPerIteration    int           `yaml:"turn_budget_per_iteration"`
    CooldownBetweenIterations time.Duration `yaml:"cooldown_between_iterations"`
    MaxCostPerPRUSD           float64       `yaml:"max_cost_per_pr_usd"`
    NudgeThresholdPct         float64       `yaml:"nudge_threshold_pct"`
    SummaryModel              string        `yaml:"summary_model"` // provider name for timeout summary
    AutoRalphOnYolo           bool          `yaml:"auto_ralph_on_yolo"` // default true
}
```

### DB Schema

```sql
-- Added to lifecycle.db

CREATE TABLE ralph_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    pr_key TEXT NOT NULL,
    iteration INTEGER NOT NULL,
    session_key TEXT,
    stage_awareness TEXT,
    stage_act TEXT,
    stage_assert TEXT,
    stage_append TEXT,
    committed_sha TEXT,
    diff_stat TEXT,
    status TEXT NOT NULL DEFAULT 'running',
    cost_usd REAL DEFAULT 0.0,
    tokens_in INTEGER DEFAULT 0,
    tokens_out INTEGER DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME,
    UNIQUE(pr_key, iteration)
);

CREATE INDEX idx_ralph_pr_key ON ralph_sessions(pr_key);
CREATE INDEX idx_ralph_status ON ralph_sessions(status);
```

## Integration Points

### Session Manager
- Ticker goroutine starts in `Manager.Start()` (spawns alongside existing event bus consumer)
- Ticker stops in `Manager.Shutdown()`
- `handleEvent` extended: after `PullRequestLabelUpdated` with `ralph`, immediately queues first iteration (no cooldown for first)
- Yolo auto-ralph: after implementer session creates PR, `verifyAC()` called. If incomplete, `forgejo.AddIssueLabel(pr, "ralph")` + `ralph.QueueInitialIteration(pr)`

### Agent
- `buildSystemPrompt` checks if session key matches `-ralph-i` pattern → injects ralph system prompt section
- `NewAgent` registers `ralph_update` and `ralph_progress` tools when in ralph mode
- Turn nudging: `ProcessEvent` consults `RalphTracker.ShouldNudge(turn)` before each LLM call

### Tool Registry
- `write_file` path validator consults `RalphGuard.IsSpecPath()` during ralph sessions → returns error
- `git` tool commit handler consults `RalphGuard.ValidateCommitDiff()` → returns error if spec files touched
- `ralph_update` and `ralph_progress` are new tools with no-op or file-write behavior

### Lifecycle
- `OnSessionFailed` extended for ralph-specific failure types
- `ListStalledRalphSessions()` for dashboard
- `GetRalphCost(prKey)` for budget enforcement

### Reviewer (djent-qa)
- On PR where `ralph` label was just removed AND AC appear met → reviewer prompt extended:
  "This PR completed ralph mode. Read `.ralph/progress/*.md`, update spec TODO checkboxes in the active spec."
- `write_file` exception: spec files are writable by reviewer ONLY when the PR has `ralph-completed` label (temporary, removed after sync)

## Testing Strategy

- Unit tests in `internal/ralph/ralph_test.go` for `Tracker`, `Guard`, `Progress`
- Unit tests in `internal/session/` for ralph session key detection, prompt injection, tool registration
- Integration test: mock Forgejo with PR #42 labeled `ralph`, assert 3 iterations spawn with correct session keys
- Integration test: yolo repo topic + failing build → assert `ralph` label added automatically
- Integration test: agent attempts `write_file` on spec path during ralph → assert error returned
- Integration test: 3 consecutive failed iterations → assert `fordjent/failed:ralph-stalled` label added
