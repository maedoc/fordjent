## MODIFIED Requirements

### Requirement: SpecPRManager generates implementer issues on spec merge
When a spec PR is merged, `SpecPRManager` SHALL parse `tasks.md` from the merged change and create one Forgejo issue per task. For `[parallel]` tasks, the scheduler SHALL validate file disjointness via a lightweight heuristic that extracts path-like tokens from task descriptions and checks for overlapping prefixes. When overlap is detected, the conflicting task SHALL be downgraded to serial ordering with a `*(auto-downgraded from parallel)*` annotation in the issue body.

#### Scenario: Spec PR merged, tasks generated
- **WHEN** a spec PR for `user-auth` is merged
- **AND** `tasks.md` contains 3 tasks: "Create auth module", "Implement OAuth flow [parallel]", "Write integration tests [parallel]"
- **AND** `validateParallelTasks` confirms the two parallel tasks have no overlapping path prefixes
- **THEN** 3 Forgejo issues are created with `[implementer]` tags
- **AND** tasks 2 and 3 have no `Depends on:` between them (parallel)
- **AND** task 1 has `Depends on: #<parent>` referencing the PM issue
- **AND** all issues are attached to the correct milestone

#### Scenario: Parallel tasks conflict at file level
- **WHEN** two `[parallel]` tasks mention the same directory or file path in their descriptions (e.g., both reference `pkg/auth/`)
- **AND** the scheduler's lightweight heuristic detects the overlap
- **THEN** the conflicting task is downgraded from parallel to serial
- **AND** the downgraded task gets `Depends on: #<first-task-issue>`
- **AND** the downgraded task's issue body contains `*(auto-downgraded from parallel)*`

> **Implementation note**: `mergequeue.Client.CheckGate()` requires branches to compare against base, but implementer branches don't exist at issue-creation time. Instead, `validateParallelTasks()` extracts path-like tokens from task descriptions and checks for overlapping prefixes. Full file-disjointness protection is still enforced by the merge queue at PR creation time.

#### Scenario: Spec PR merged with no tasks.md
- **WHEN** a spec PR is merged but `tasks.md` is empty or missing
- **THEN** the scheduler logs a warning
- **AND** no implementer issues are created
- **AND** the parent issue is not labeled `spec-implementing`
