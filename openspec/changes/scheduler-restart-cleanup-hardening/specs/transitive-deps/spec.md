## ADDED Requirements

### Requirement: Cascade unblocking in checkAndUnblock
The scheduler SHALL iteratively re-scan remaining blocked issues after each unblock within a single invocation of `checkAndUnblock`. When a PR merge unblocks issue #A, and #A's unblocking satisfies #B's dependencies, #B SHALL be unblocked in the same pass without waiting for another event or the reconciliation ticker.

#### Scenario: Direct chain (A→B→C)
- **WHEN** PR #10 merges, issue #20 depends on #10, and issue #30 depends on #20
- **THEN** both #20 and #30 are unblocked in a single `checkAndUnblock` invocation

#### Scenario: Diamond dependency (A→B, A→C, B+C→D)
- **WHEN** PR #10 merges, #20 depends on #10, #30 depends on #10, and #40 depends on both #20 and #30
- **THEN** #20 and #30 are unblocked in the first cascade round; #40 is unblocked in the second cascade round

#### Scenario: No cascade needed
- **WHEN** PR #10 merges and no blocked issue depends on an issue that depends on #10
- **THEN** only directly-dependent issues are unblocked (identical to current behavior)

#### Scenario: Cascade bounded by maxCascadeRounds
- **WHEN** the cascade loop exceeds `maxCascadeRounds` (default 10)
- **THEN** the loop terminates and logs a warning; remaining issues stay blocked until the next event or reconciliation tick

### Requirement: Cascade does not unblock cyclic dependencies
The cascade loop SHALL NOT unblock issues involved in a circular dependency. The existing `detectCircularDeps` check runs before the cascade loop and excludes cyclic issues from candidates.

#### Scenario: Cycle in dependency chain
- **WHEN** #5 depends on #10 and #10 depends on #5 (cycle), and PR #15 merges which could unblock #5 if #10 were closed
- **THEN** neither #5 nor #10 is unblocked (skipped by cycle detection)

### Requirement: ReconcileBlocked uses cascade logic
The `ReconcileBlocked` method (periodic safety net) SHALL use the same iterative cascade logic as `checkAndUnblock`.

#### Scenario: Reconciliation with transitive chain
- **WHEN** the 2-hour reconciliation tick runs and an issue's dependency was unblocked by a previous reconciliation
- **THEN** the transitively-dependent issue is also unblocked in the same reconciliation pass

### Requirement: Cascade round logging
Each cascade round SHALL log the round number and the number of issues unblocked in that round for observability.

#### Scenario: Observability on cascade
- **WHEN** a cascade unblocks 2 issues in round 1 and 1 issue in round 2
- **THEN** log messages show "cascade round 1 unblocked 2 issues" and "cascade round 2 unblocked 1 issues"
