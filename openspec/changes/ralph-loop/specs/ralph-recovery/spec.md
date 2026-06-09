## ADDED Requirements

### Requirement: Timeout summary generates commit from partial work
When a ralph session exhausts its turn budget without reaching the `append` stage, the harness SHALL call a fast/cheap LLM to summarize the session's memory. The summary SHALL become the commit message for a commit of whatever files exist in the working tree. The commit is pushed to the PR branch and the iteration is recorded as `failed_turns`.

#### Scenario: Turns exhausted, partial work committed
- **GIVEN** ralph iteration 4 with turn budget 20
- **AND** the agent completed awareness and act but not assert/append
- **AND** `main.go` has been modified in the workdir
- **WHEN** turn 20 completes without append
- **THEN** a fast LLM call generates summary: "Attempted to add error handling to main.go. Nil guard partially implemented. Need to verify against edge cases."
- **AND** all files in workdir are staged
- **AND** a commit is created with message: "ralph-i4 [incomplete]: Attempted to add error handling..."
- **AND** the commit is pushed
- **AND** iteration 4 status is 'failed_turns'

#### Scenario: Turns exhausted, no file changes
- **GIVEN** ralph iteration 2
- **AND** the agent got stuck in awareness phase, no files modified
- **WHEN** turn budget exhausted
- **THEN** a commit is still created (empty commit allowed)
- **AND** message: "ralph-i2 [incomplete]: No progress — agent stuck in awareness phase"
- **AND** iteration 2 status is 'failed_turns'

#### Scenario: Summary model falls back to default
- **GIVEN** `summary_model` config is set to "fast"
- **AND** the "fast" provider is unavailable
- **WHEN** timeout summary is needed
- **THEN** the harness falls back to the session's default provider
- **AND** logs a warning about the fallback

### Requirement: Stall detection prevents infinite loops
The harness SHALL detect when three consecutive ralph iterations for the same PR produce no new commits or are recorded as `failed_turns`. When this occurs, the harness SHALL remove the `ralph` label, add `fordjent/failed:ralph-stalled`, and post a PR comment explaining the stall.

#### Scenario: Three consecutive no-progress iterations trigger stall
- **GIVEN** iterations 3, 4, 5 for PR #42 all have status 'failed_turns'
- **WHEN** the scheduler checks before spawning iteration 6
- **THEN** iteration 6 is NOT spawned
- **AND** `ralph` label is removed
- **AND** `fordjent/failed:ralph-stalled` is added
- **AND** a PR comment is posted with iteration history summary

#### Scenario: One successful iteration resets stall counter
- **GIVEN** iterations 3, 4 have status 'failed_turns'
- **AND** iteration 5 has status 'completed' with a new commit
- **WHEN** the scheduler checks before spawning iteration 6
- **THEN** iteration 6 IS spawned
- **AND** the stall counter is reset to 0

#### Scenario: Two no-progress + one progress does not stall
- **GIVEN** iterations 3, 4 have status 'failed_turns'
- **AND** iteration 5 has status 'completed'
- **WHEN** iteration 6 is spawned
- **THEN** no stall label is added
- **AND** normal ralph flow continues

### Requirement: Budget cap prevents runaway spend
Before spawning each ralph iteration, the harness SHALL query `SUM(cost_usd) FROM ralph_sessions WHERE pr_key = ?`. If the total cost equals or exceeds `max_cost_per_pr_usd`, the harness SHALL halt ralph for that PR, remove the label, and add `fordjent/failed:ralph-budget`.

#### Scenario: Budget cap halts ralph
- **GIVEN** `max_cost_per_pr_usd` is 5.00
- **AND** accumulated cost for PR #42 is 5.15
- **WHEN** the scheduler attempts to spawn the next iteration
- **THEN** the iteration is blocked
- **AND** `ralph` label is removed
- **AND** `fordjent/failed:ralph-budget` is added
- **AND** a PR comment is posted: "Ralph budget ($5.00) exhausted. Human review required."

#### Scenario: Budget tracked across iterations
- **GIVEN** iteration 1 cost $0.80, iteration 2 cost $1.20
- **WHEN** the scheduler checks before iteration 3
- **THEN** the total queried is $2.00
- **AND** iteration 3 proceeds (below cap)

### Requirement: Cooldown duration is configurable and respected
The cooldown between ralph iterations SHALL be configurable via `ralph.cooldown_between_iterations`. The scheduler SHALL enforce this delay regardless of how quickly the previous iteration completed.

#### Scenario: Configurable cooldown
- **GIVEN** `cooldown_between_iterations` is "5m"
- **AND** iteration 3 completed 2 minutes ago
- **WHEN** the scheduler ticker fires
- **THEN** iteration 4 is NOT spawned
- **AND** the scheduler waits until 5 minutes have elapsed

#### Scenario: Zero cooldown allows immediate retry
- **GIVEN** `cooldown_between_iterations` is "0s"
- **AND** iteration 3 just completed
- **WHEN** the scheduler ticker fires
- **THEN** iteration 4 MAY be spawned immediately
- **AND** a warning is logged about zero cooldown
