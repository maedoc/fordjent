## Context

Fordjent coordinates multiple agent roles (PM, implementer, reviewer/djent-qa) through Forgejo webhooks. Each role has a craft spec describing HOW it works (`pm-spec-authoring`, `ralph-scheduler`, `ralph-protocol`, `spec-driven-implementation`), but no spec defines WHEN each role activates or what artifacts flow between roles. This causes:

1. **Overlapping activation claims**: The PM spec says PM "re-activates" on review comments AND on completion. The Ralph spec says Ralph "queues a reviewer" when AC are met. Both describe handoffs with no shared reference point.
2. **Implicit routing**: `handleEvent` in `internal/session/manager.go` (`Manager.handleEvent`) routes events to roles via ad-hoc conditionals. Adding a new role requires editing scattered `if` blocks with no testable specification.
3. **Shadow state**: Session creation logic, label transitions, and event handling are spread across `manager.go`, `agent.go`, `router.go`, and `scheduler.go`. No single file or spec expresses the full lifecycle.

The `spec-driven` schema already defines craft specs (HOW). What is missing is the orchestration layer (WHEN and WHAT) that binds them into a coherent machine.

## Goals / Non-Goals

**Goals:**
- Explicit stage machine for every change: `spec-proposed → spec-approved → implementing → reviewing → merging → archived`
- Priority-ordered event-to-role routing table driven by PR labels
- Explicit handoff artifacts between roles (e.g. PM writes `tasks.md` → Scheduler consumes it)
- PM archival is a natural consequence of the chain, not an independent trigger
- Ralph is framed as an implementation harness extension within `implementing`, not a lifecycle peer
- All session creation flows through one routing table; no component spawns sessions independently
- Integration tests for the full handoff chain

**Non-Goals:**
- Modifying Ralph's 4-A protocol, turn mechanics, or iteration budget (stays in `ralph-protocol`)
- Modifying PM's spec writing craft (stays in `pm-spec-authoring`)
- New DB tables or external dependencies
- New Forgejo event types (uses existing `issue_comment.created`, `pull_request.merged`, etc.)
- Real-time human-in-the-loop coordination during Ralph (humans comment, routing table dispatches on next event)

## Decisions

### D1: Routing table lives in `router.go`, not `manager.go`

**Choice**: The webhook router (`internal/webhook/router.go`, `Router.ServeHTTP` or `Router.handleEvent`) inspects PR labels and dispatches events to `internal/event.EventBus` with a pre-computed `Role` and session key. The session manager's `handleEvent` trusts the event's `Role` field and does not re-evaluate routing conditions.

**Alternate**: Keep routing in `manager.go` as it is today. `handleEvent` inspects `evt.Type`, PR labels, sender, etc. and decides which role to invoke.

**Rationale**: The router already parses the webhook payload into `event.Event`. Adding a routing decision there keeps session manager focused on session lifecycle (create, run, complete). It also means integration tests can inject `Event{Role: "reviewer", ...}` directly, bypassing the router entirely. Routing is a concern of the event boundary, not session execution.

### D2: Stage machine is implicit via labels, not a DB column

**Choice**: The lifecycle stage of a change is inferred from the set of PR/issue labels at any moment:
- `spec-proposed` label on spec PR → `spec-proposed`
- No `spec-proposed`/PR merged → `spec-approved`
- Open task issues with unmerged PRs → `implementing`
- PR has `ralph-completed` label → `reviewing` (post-Ralph path)
- PR has `automerge` or reviewer merged it → `merging`
- Change directory moved to `archive/` and issue closed → `archived`

**Alternate**: Add a `change_state` column to `lifecycle.db` with explicit stage tracking.

**Rationale**: Labels are already the source of truth for scheduling (`blocked`/`ready`) and routing (`spec-proposed`, `ralph`, `ralph-completed`). Adding a DB column duplicates state and risks desync. Labels are human-visible in Forgejo UI, which aids debugging. The only downside is that stage queries require JOINing labels across PRs/issues, but this is rare (only for the archival trigger check, which runs on merge events).

### D3: `ArchiveChangeRequested` is an internal `event.Event`, not a new webhook type

**Choice**: When the scheduler detects all task issues for a change are closed, it dispatches an `event.Event{Type: "pm.archive_requested", Change: "user-auth"}` onto the internal event bus. The session manager's event consumer picks this up and creates a PM session with a synthetic issue title `[pm] Archive change user-auth`.

**Alternate**: The scheduler creates a real Forgejo issue titled "Archive change X" and lets the webhook router handle it normally.

**Rationale**: Creating a real issue would cause webhook round-trips and add noise to the issue tracker. The archive action is internal bookkeeping, not human-facing work. An internal event keeps the loop tight and avoids the webhook delivery path entirely. The PM session still gets a `[pm]` tag so role detection works normally.

### D4: `tasks.md` is scheduler-writable after PM creates it

**Choice**: PM authors `tasks.md` during spec creation. After that, only the scheduler may update it (marking task checkboxes on PR merge). Implementers and Ralph read it but never write it.

**Alternate**: Allow the PM to also update `tasks.md` during archival review.

**Rationale**: PM archival is a passive review action (moving `openspec/changes/<name>/` to `archive/`). If tasks truly need editing after creation, a human should file a new PM issue. Letting the PM edit tasks mid-implementation creates confusion about scope. The scheduler owns task state because it is the component that knows when PRs merge.

### D5: Routing table is source-of-truth for ALL session creation

**Choice**: Every Fordjent session — PM, implementer, Ralph, reviewer, `-fix` — is created via the routing table. No component (scheduler, lifecycle, automerge handler) may call `mgr.CreateSession` directly.

**Alternate**: Some components call `CreateSession` directly for convenience (e.g. scheduler creates implementer issues, Ralph scheduler spawns iterations).

**Rationale**: Centralizing session creation in the routing table makes it impossible to accidentally create the wrong role for an event. It also makes the system testable: a single table + mock events = full orchestration tests. The cost is minor indirection (scheduler dispatches an event to the bus instead of calling `CreateSession` directly).

## Data Flow

```
┌──────────────────────────────────────────────────────────────────────┐
│                     Webhook from Forgejo                              │
│  issue_comment.created / pull_request.merged / issue.closed          │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│                     Router.ServeHTTP                                  │
│  1. Parse payload → event.Event                                       │
│  2. Fetch PR labels (API call or cache)                               │
│  3. Apply routing table (priority 1-10)                               │
│  4. Set event.Role, SessionKey, PRNumber, IssueNumber                 │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│                     Session Manager.handleEvent                       │
│  - Match SessionKey → existing session?                               │
│    - Yes → continue / review comment dispatch                         │
│    - No  → create session for Role                                    │
│      - PM: open_system_prompt + spec-writing tools                    │
│      - Implementer: write_file + git + bash + forgejo_create_pr      │
│      - Ralph: ralph_update + ralph_progress + implementer tools       │
│      - Reviewer: read_file + forgejo_comment + forgejo_merge_pr      │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼ (session runs bounded LLM loop)
┌──────────────────────────────────────────────────────────────────────┐
│                     Session completes                                 │
│  PM     → commits spec to branch (spec-proposed label)                │
│  Impl   → creates PR (build gate runs)                                │
│  Ralph  → commits to PR branch, records ac_met                        │
│  Review → merges PR or posts review comment                           │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│                     Scheduler.OnPRMerged                              │
│  1. Mark task checkbox in tasks.md                                    │
│  2. Check if all task issues for change are closed                    │
│  3. If yes → dispatch ArchiveChangeRequested event                    │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────┐
│                     PM Archival Session                               │
│  1. Verify no open PR has `ralph` label for this change               │
│  2. Call openspec_archive_change(change_name)                         │
│  3. Commit + push (or create archive PR in non-yolo)                  │
└──────────────────────────────────────────────────────────────────────┘
```

## Interfaces

### Routing Table (new, in `internal/webhook/router.go`)

```go
// RouteResult is computed by the routing table for each event.
type RouteResult struct {
    Role       string // "pm", "implementer", "reviewer", "ralph"
    SessionKey string // e.g. "fjadmin/testbed/pulls/42"
    IsFix      bool   // true for pulls/N-fix sessions
}

// RouteTable matches events to roles.
type RouteTable struct {
    forgejo *forgejo.Client
}

func NewRouteTable(forgejo *forgejo.Client) *RouteTable

// Route evaluates the 10 priority rules and returns the result.
// It returns (result, matched=false) if no rule matches.
func (rt *RouteTable) Route(ctx context.Context, evt event.Event) (RouteResult, bool)

// PRLabels fetches labels for a PR; cached per-request.
func (rt *RouteTable) PRLabels(ctx context.Context, repo string, prNum int) ([]string, error)
```

### Event Extensions (in `internal/event/event.go`)

```go
type Event struct {
    Type       EventType
    // ... existing fields ...
    Role       string // NEW: set by routing table before dispatch
    SessionKey string // NEW: canonical key, set by routing table
    Change     string // NEW: for internal events like ArchiveChangeRequested
}

// New internal event type for scheduler → PM handoff.
const ArchiveChangeRequested EventType = "pm.archive_requested"
```

### Scheduler Extension (in `internal/scheduler/scheduler.go`)

```go
// OnPRMerged is extended to mark tasks and check for archival.
func (s *Scheduler) OnPRMerged(ctx context.Context, repo string, pr *forgejo.PullRequest) error

// checkAllTasksClosed returns true if every task issue for the change is closed.
func (s *Scheduler) checkAllTasksClosed(ctx context.Context, repo, changeName string) (bool, error)

// markTaskDone updates tasks.md checkbox for the merged PR's task.
func (s *Scheduler) markTaskDone(repoDir, changeName string, taskNum int) error

// dispatchArchiveChangeRequested sends internal event to the bus.
func (s *Scheduler) dispatchArchiveChangeRequested(changeName string)
```

### Ralph Completion (in `internal/session/manager.go` or Ralph harness)

```go
// onRalphAppendComplete is called after ralph_update(stage="append") succeeds.
// It records ac_met, removes `ralph` label, adds `ralph-completed` label.
func onRalphAppendComplete(ctx context.Context, forgejo *forgejo.Client, pr *forgejo.PullRequest, iteration *ralph.IterationRecord) error
```

## Integration Points

### Session Manager (`internal/session/manager.go`)
- `handleEvent` trusts `evt.Role` from the routing table; no re-evaluation
- On `ArchiveChangeRequested`, creates a PM session with synthetic issue title `[pm] Archive change <name>`
- On `ralph-completed` PR labels, creates a reviewer session (or relies on routing table to dispatch next event)

### Router (`internal/webhook/router.go`)
- `ServeHTTP` calls `RouteTable.Route(ctx, evt)` before dispatching to the event bus
- Sets `evt.Role` and `evt.SessionKey` on the dispatched event
- Caches PR labels per request (single Forgejo API call)

### Scheduler (`internal/scheduler/scheduler.go`)
- `OnPRMerged` extended with `markTaskDone` + `checkAllTasksClosed` + `dispatchArchiveChangeRequested`
- No longer calls `CreateSession` directly; dispatches events to the bus instead
- `checkAllTasksClosed` queries open issues matching the change name (stored in issue body or label)

### Agent (`internal/session/agent.go`)
- `buildSystemPrompt` checks `evt.Role` (not session key heuristics) to select prompt variant
- Ralph prompt variant injected when `Role == "ralph"`
- PM prompt variant injected when `Role == "pm"`

### Forgejo Client (`internal/forgejo/client.go`)
- `GetPRLabels(repo, prNum)` used by `RouteTable.PRLabels`
- `AddIssueLabels`, `RemoveIssueLabel` used by `onRalphAppendComplete`

## Testing Strategy

- **Unit test**: `TestRouteTable` — mock Forgejo returning various label sets; assert correct `Role`/`SessionKey` for each rule
- **Unit test**: `TestRouteTablePriority` — PR with multiple labels; assert highest-priority rule wins + warning logged
- **Integration test**: Full handoff chain with fake Forgejo:
  1. Spec PR merged → scheduler creates task issues
  2. Implementer issue created → session runs → PR created
  3. Ralph activated → iteration runs → `ralph-completed` label added
  4. Reviewer session created → merges PR
  5. Scheduler marks task done → all tasks closed → `ArchiveChangeRequested` dispatched
  6. PM archival session created → `openspec_archive_change` called
- **Integration test**: Routing table sovereignty — assert that calling `mgr.CreateSession` directly from any component logs a panic/warning

## Risks / Trade-offs

**[PR label caching becomes stale between event dispatch and session start]** → Labels are fetched once in the router at event receipt time. If a label changes between dispatch and session start (e.g. `ralph` removed by scheduler while session is being created), the session may have the wrong role. Mitigation: session creation checks labels again; if mismatched, logs warning and re-routes.

**[Fetching PR labels per event adds API latency]** → One Forgejo API call per PR event. Events have `pr_number` from the payload. Label sets are small (3-5 labels). Acceptable overhead (~50ms per event).

**[ArchiveChangeRequested synthetic issue bypasses role gate]** → The synthetic issue has `[pm]` in the title and triggers PM role detection normally. If `require_role_tag: true` would block it, the scheduler bypasses the gate for internal events.

**[tasks.md checkbox marking may fail if tasks.md was manually edited]** → The scheduler uses a simple regex to find the task line (`^- \[ \] .*\[Task N\]`). If the manual edit breaks the format, the checkbox stays unchecked and archival never triggers. Mitigation: PM spec requires stable task line format. If marking fails, scheduler logs warning and continues.
