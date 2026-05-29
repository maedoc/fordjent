## Context

Fordjent currently operates on a simple issue→PR→merge pipeline. The PM role decomposes work into sub-issues with `Depends on: #N` links. The implementer reads issue bodies and writes code. The reviewer checks correctness and merges. There is no structured specification layer — no proposal, design doc, requirements spec, or task list that survives beyond a single issue body.

Meanwhile, this repo (fordjent itself) uses OpenSpec for all feature work: `openspec/changes/<name>/{proposal,design,specs,tasks}.md` committed to git, driven by pi skills (`openspec-propose`, `openspec-apply`, `openspec-archive`). The process works: proposal clarifies "why," design captures "how," specs define requirements, tasks track progress, and archive preserves history.

The gap is that Fordjent agents — djent-pm, djent-dev, djent-qa — cannot participate in or benefit from this process. They don't know what `openspec/changes/` is, can't create specs, and can't read them for implementation guidance.

This design adds a spec lifecycle subsystem to Fordjent that makes OpenSpec artifacts first-class citizens in the agent workflow, re-implementing the OpenSpec CLI mechanics in Go (no Node.js dependency) while keeping the artifact format identical to what pi uses.

## Goals / Non-Goals

**Goals:**
- PM creates structured specs (proposal, design, specs/*.md, tasks.md) and pushes them as PRs
- Human reviews spec PRs, djent-pm refines per feedback, human merges when approved
- Scheduler generates implementer issues from merged `tasks.md` with milestones
- Implementer reads specs and tasks for implementation guidance
- Reviewer checks implementation against spec requirements
- PM auto-archives completed changes (or creates archive PR for human review)
- Yolo mode (fordjent-yolo topic) skips spec PR review and auto-approves everything
- Zero new external dependencies — all file ops use Go stdlib
- Parallel fan-out for file-disjoint tasks declared in tasks.md

**Non-Goals:**
- Reimplementing the OpenSpec CLI as a standalone binary
- Changing the artifact format — must remain identical to what pi creates
- Adding `openspec` as a Node.js dependency
- Modifying existing issue→PR→merge flows that don't use specs
- Spec-driven implementation for repos that don't opt in
- Cross-repo spec references
- Spec versioning or diffing between versions

## Decisions

### Decision 1: Re-implement OpenSpec mechanics in Go, don't depend on CLI

**Choice**: Implement file scaffolding, task parsing, archive moves, and delta spec sync entirely in Go within `internal/speccycle/`. The PM agent writes files via existing `write_file` tool; the `speccycle` package provides helper functions for the scheduler and tools.

**Alternate considered**: Shell out to `openspec` CLI.

**Rationale**:
- The CLI does ~150 lines of work: `mkdir`, parsing `- [ ]` / `- [x]` checkboxes, `mv` to archive. Adding a Node.js dependency to a Go binary for this is overkill.
- The "intelligence" — what artifacts to create, their structure, dependency rules — lives in the PM system prompt, not in the CLI.
- Go stdlib (`os`, `path/filepath`, `regexp`, `strings`, `bufio`) handles all needed operations.
- No runtime dependency, no version compatibility issues, no install step.

### Decision 2: Spec files live in the application repo under `openspec/`

**Choice**: PM writes to `openspec/changes/<name>/` and `openspec/specs/<capability>/` in the same repo as the code being built.

**Alternate considered**: Separate `specs/` repo.

**Rationale**:
- Specs travel with code — any checkout has the right specs.
- Git history tracks spec evolution alongside code changes.
- PR review works naturally (same repo, same webhooks, same permissions).
- Matches how this project already uses OpenSpec.

### Decision 3: PM uses existing `write_file` tool, not a new Forgejo API tool

**Choice**: PM role gets `write_file` permission restricted to `openspec/changes/` paths. Creates local files, then uses `git` tool to commit and push spec branches.

**Alternate considered**: New `openspec_propose` tool that calls Forgejo Contents API to create files directly.

**Rationale**:
- Avoids base64-encoding and multi-step API calls for multi-file spec creation.
- PM can create all artifact files (5+) in a single turn via multiple `write_file` calls.
- Atomic: all files written locally, then `git commit` creates a single coherent commit.
- Simpler implementation — no new API surface, just path-gated `write_file`.

### Decision 4: Yolo mode skips spec PR entirely

**Choice**: When repo has `fordjent-yolo` topic, PM commits specs directly to `main` branch and scheduler immediately creates implementer issues.

**Alternate considered**: Yolo still creates spec PR but auto-merges it.

**Rationale**:
- The spec PR review phase IS the human gate. Remove the gate, remove the PR.
- Auto-merging a PR the agent created seconds ago is a no-op ceremony.
- Spec artifacts still land in `openspec/changes/` on main and are archived on completion — history is preserved.
- Matches existing yolo behavior where implementer commits to main directly.

### Decision 5: Scheduler parses `tasks.md` for task→issue generation

**Choice**: On spec PR merge, scheduler reads `openspec/changes/<name>/tasks.md`, parses `- [ ]` lines, and creates one Forgejo issue per task with `[implementer]` tag and milestone attachment.

**Alternate considered**: PM explicitly calls `forgejo_create_issue` for each task (current behavior).

**Rationale**:
- Current PM emits sub-issues immediately, before human review. With spec PR review, task issues should only be created AFTER the spec is approved.
- Parsing `tasks.md` ensures the tasks in Forgejo exactly match the reviewed spec.
- The PM already writes `tasks.md` during spec creation — no additional PM work.
- File-disjoint tasks can be detected at parse time for parallel fan-out.

### Decision 6: File-disjoint parallelism via scheduler + merge queue check

**Choice**: Tasks.md entries can include `[parallel]` tag. Scheduler detects these, validates file disjointness via merge queue's existing file-gate logic, and creates issues simultaneously with no inter-task dependencies.

**Alternate considered**: Always serialize (one implementer at a time).

**Rationale**:
- Fordjent already spawns sessions concurrently (260ms for 5 parallel spawns).
- The merge queue already has file-overlap detection (`internal/mergequeue/`).
- Adding a pre-creation check: for each `[parallel]` task, verify changed files (from spec context) don't overlap with other parallel tasks. If they do, serialize them.
- Matches pi-subagents' `worktree: true` pattern — concurrent writers in isolated worktrees.

### Decision 7: Verification contract in specs, checked by reviewer

**Choice**: Specs include an optional `## Verification` section. Reviewer's system prompt instructs it to check implementation against verification criteria. `forgejo_merge_pr` tool checks verification before merging.

**Alternate considered**: Verification is implicit (reviewer figures it out).

**Rationale**:
- pi-subagents emphasizes "validation contract before implementation" — gives shared definition of done.
- Concrete checkboxes (`- [ ] go build passes`, `- [ ] curl returns 200`) are parseable and testable.
- Reviewer doesn't need to guess what "done" means.
- Build checks already exist in `forgejo_merge_pr` — verification criteria extend that pattern.

## Data Flow

### Non-Yolo Spec→Implement Flow

```
Human: "[pm] Build feature X"
        │
        ▼
  ┌─────────────────────────────────────────────┐
  │ djent-pm session                            │
  │                                             │
  │ 1. Scout codebase (read_file, bash ls)      │
  │ 2. write_file openspec/changes/feature-x/   │
  │    ├── proposal.md                          │
  │    ├── design.md                            │
  │    ├── specs/feature-core/spec.md            │
  │    └── tasks.md                             │
  │ 3. git checkout -b spec/feature-x            │
  │ 4. git add openspec/ && git commit           │
  │ 5. git push origin spec/feature-x            │
  │ 6. forgejo_create_pr (base: main,             │
  │    head: spec/feature-x,                     │
  │    labels: spec-proposed)                    │
  │ 7. forgejo_comment: "Spec PR #N ready"       │
  └─────────────┬───────────────────────────────┘
                │
                ▼
  ┌─────────────────────────────────────────────┐
  │ Human reviews PR #N                         │
  │ • Leaves comments on proposal/design/specs   │
  │ • djent-pm reactivates on review comments   │
  │   → Updates files, pushes to spec branch    │
  │ • Human approves → merges PR                │
  └─────────────┬───────────────────────────────┘
                │ pull_request.merged
                ▼
  ┌─────────────────────────────────────────────┐
  │ Scheduler (handleEvent)                     │
  │                                             │
  │ 1. Detect: is this a spec PR?               │
  │    → Check diff for openspec/changes/        │
  │ 2. speccycle.ParseTasks(repo, change)        │
  │    → Reads tasks.md from merged main         │
  │ 3. For each task:                            │
  │    → Create Forgejo issue                    │
  │    → "[implementer] <task description>"       │
  │    → Body: "Spec: openspec/changes/...       │
  │              Depends on: #<parent>"           │
  │    → Attach to milestone                     │
  │ 4. For [parallel] tasks:                     │
  │    → Validate file disjointness via mq       │
  │    → Skip Depends on: for disjoint tasks     │
  │ 5. Label parent: spec-implementing           │
  │ 6. forgejo_comment: "Spec approved.           │
  │    Implementation beginning..."              │
  └─────────────┬───────────────────────────────┘
                │
                ▼
  ┌─────────────────────────────────────────────┐
  │ djent-dev sessions (parallel for [parallel]) │
  │                                             │
  │ 1. openspec_get_tasks("feature-x")            │
  │    → Returns task list with context          │
  │ 2. openspec_read_spec("feature-core")         │
  │    → Returns requirements                    │
  │ 3. Implement, test, PR                       │
  │ 4. openspec_mark_task("feature-x", 3)         │
  │    → Updates tasks.md checkbox               │
  └─────────────┬───────────────────────────────┘
                │ all tasks complete
                ▼
  ┌─────────────────────────────────────────────┐
  │ Scheduler: milestone 100%                    │
  │ → Trigger PM follow-up                      │
  │                                             │
  │ djent-pm follow-up:                         │
  │ 1. Verify all tasks done                     │
  │ 2. openspec_archive_change("feature-x")       │
  │    → mv openspec/changes/feature-x/          │
  │      → openspec/changes/archive/             │
  │      YYYY-MM-DD-feature-x/                   │
  │ 3. Sync delta specs → main specs/            │
  │ 4. git commit + push (or archive PR)         │
  │ 5. Label parent: spec-complete               │
  └─────────────────────────────────────────────┘
```

### Yolo Fast Path

```
Human: "[pm] Build feature X"  (repo has fordjent-yolo)
        │
        ▼
  djent-pm: write spec files → commit to main → scheduler creates issues
  (no spec PR, no human review gate)

  djent-dev: implement per tasks.md → auto-merge PRs
  (no review gate)

  djent-pm: auto-archive on completion
  (no archive PR)
```

## Risks / Trade-offs

### Risk: PM writes low-quality or incomplete specs
**Mitigation**: In non-yolo mode, human reviews spec PR. In yolo mode, the assumption is that speed matters more than spec quality. Specs can be refined later via follow-up issues.

### Risk: Spec files accumulate and bloat the repo
**Mitigation**: Archive moves completed changes to `openspec/changes/archive/` which is organized by date. Delta specs synced to `openspec/specs/` are the canonical reference. Old archives can be pruned.

### Risk: `tasks.md` format must be parseable but the PM is an LLM
**Mitigation**: PM system prompt specifies exact format: `- [ ] Task description` with one task per line. The parser is lenient (regex-based, strips whitespace, handles missing checkboxes). Invalid lines are ignored with a log warning. Human can always edit tasks.md to fix format issues.

### Risk: Spec branch merge conflicts if two PM issues run simultaneously
**Mitigation**: Same as any concurrent git work — last to push must rebase. The stale gate in `forgejo_create_pr` applies to spec PRs too. PM sees "your branch is behind origin/main" and must rebase.

### Risk: `[parallel]` tasks declared parallel but actually conflict at file level
**Mitigation**: Scheduler's merge queue check (`internal/mergequeue/`) validates file disjointness before creating parallel issues. If files overlap, the scheduler falls back to serial ordering with explicit `Depends on:` chain.

### Risk: PM burns excessive turns writing multi-file specs
**Mitigation**: PM turn budget is configurable (`max_turns_pm`). Spec writing is mostly `write_file` calls — fast, low-token operations. PM already does codebase exploration which is the expensive part; spec writing adds 3-5 turns of `write_file` + `git` operations.

### Risk: Verification contract in specs becomes stale vs actual code behavior
**Mitigation**: Verification is aspirational until reviewer checks it. If the spec says "returns 200" but implementation returns 201, reviewer flags it as a spec deviation. Human decides whether to update spec or fix code.

## Open Questions

1. **Should `openspec/changes/<name>/tasks.md` be the single source of truth, or should Forgejo issues be the source of truth?** Design assumes tasks.md is authoritative and issues are generated from it. The implementer still uses Forgejo issues for tracking (labels, PR references), but `openspec_mark_task` updates the file. If the file desyncs from issues, which wins? Tentative: file wins, issues are read-only snapshots.

2. **Should the PM create `design.md` for every change, or only complex ones?** The OpenSpec schema treats design as a first-class artifact, but simple changes may not need architecture decisions. Tentative: PM creates design.md when the task complexity warrants it (multi-file, cross-package, new pattern). The system prompt should give guidance: "For single-file changes or bug fixes, skip design.md."

3. **How does the implementer find the right spec for a given issue?** Design says the issue body includes a spec reference. But if the implementer doesn't read the issue body carefully, it might miss it. Tentative: `openspec_get_tasks` tool does automatic lookup from the milestone or parent issue, making the reference path explicit in the tool response.
