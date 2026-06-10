## ADDED Requirements

### Requirement: Ralph scheduler scans for ralph-labeled PRs
`RalphScheduler` SHALL maintain a `time.Ticker` that scans all open PRs in all watched repositories every `ralph.cooldown_between_iterations` (default 2 minutes). For each PR with the `ralph` label, the scheduler SHALL check whether the last recorded ralph iteration for that PR has completed. If completed AND cooldown elapsed, the scheduler SHALL spawn the next iteration.

#### Scenario: PR labeled with 'ralph' triggers first iteration
- **GIVEN** open PR #42 in repo `fjadmin/testbed` with the `ralph` label
- **AND** no existing ralph iterations for PR #42
- **WHEN** the scheduler ticker fires
- **THEN** a ralph session is created with key `fjadmin/testbed/pulls/42-ralph-i1`
- **AND** the session is recorded in `ralph_sessions` with iteration=1, status='running'

#### Scenario: Completed iteration triggers next after cooldown
- **GIVEN** ralph iteration 3 for PR #42 completed 3 minutes ago
- **AND** cooldown is 2 minutes
- **WHEN** the scheduler ticker fires
- **THEN** ralph iteration 4 is spawned with key `.../pulls/42-ralph-i4`
- **AND** iteration 3 status is updated to 'completed'

#### Scenario: Active iteration blocks next spawn
- **GIVEN** ralph iteration 5 for PR #42 is still running (status='running')
- **WHEN** the scheduler ticker fires
- **THEN** no new iteration is spawned
- **AND** a debug log is emitted: "ralph iteration in progress, skipping"

#### Scenario: Cooldown not elapsed blocks next spawn
- **GIVEN** ralph iteration 2 completed 30 seconds ago
- **AND** cooldown is 2 minutes
- **WHEN** the scheduler ticker fires
- **THEN** no new iteration is spawned
- **AND** a debug log is emitted with remaining cooldown time

### Requirement: Ralph session factory prepares workdir and context
For each ralph iteration, the factory SHALL compute the iteration number, read the last committed SHA from git, checkout the PR branch in the workdir, rebase `origin/main` if the branch is stale, and inject the ralph-mode system prompt with iteration-aware context.

#### Scenario: First iteration starts from PR head
- **GIVEN** PR #42 head is at commit `abc1234`
- **WHEN** iteration 1 is spawned
- **THEN** the workdir clones the repo and checks out the PR branch
- **AND** the session prompt includes "This is iteration 1 of ralph on PR #42"
- **AND** `ralph_update` and `ralph_progress` tools are registered

#### Scenario: Subsequent iteration reads git log for grounding
- **GIVEN** iteration 3 is spawned
- **AND** the last ralph commit message was "ralph-i2 [assert]: benchmark shows 3% regression on large inputs"
- **WHEN** the session system prompt is built
- **THEN** the prompt includes the last committed SHA
- **AND** the prompt instructs the agent to read `git log -n5` for awareness

#### Scenario: Stale branch triggers auto-rebase before session start
- **GIVEN** the PR branch is 2 commits behind `origin/main`
- **WHEN** iteration 2 is spawned
- **THEN** `git fetch origin` and `git rebase origin/main` run before the LLM loop begins
- **AND** if rebase succeeds, the session proceeds
- **AND** if rebase fails, the session starts anyway but the prompt includes a rebase-conflict warning

### Requirement: Ralph iteration performs acceptance criteria self-check
After each ralph iteration's `append` stage, the harness SHALL verify acceptance criteria against the spec. The self-check SHALL read the spec verification section, run the build/test gate, and record whether all criteria are met. The result is recorded in the iteration record but does NOT trigger label changes or downstream sessions.

#### Scenario: All AC met
- **GIVEN** ralph iteration 7 for PR #42
- **AND** the spec's verification checkboxes are all satisfied
- **AND** build and tests pass
- **WHEN** AC self-check runs after append
- **THEN** the iteration record is updated with `ac_met: true`
- **AND** the iteration 7 status is updated to 'completed'

#### Scenario: AC still unmet
- **GIVEN** ralph iteration 5 for PR #42
- **AND** one verification checkbox remains unsatisfied
- **WHEN** AC self-check runs
- **THEN** the iteration record is updated with `ac_met: false`
- **AND** iteration 5 status is updated to 'completed'

#### Scenario: Spec file missing on branch
- **GIVEN** PR branch has no `openspec/changes/*/` directory
- **AND** the linked issue body has no acceptance criteria section
- **WHEN** AC self-check runs
- **THEN** verification falls back to build/test gate only
- **AND** the iteration record notes "no spec found, used build/test only"

### Requirement: Ralph scheduler respects hard caps
The scheduler SHALL consult `max_iterations_per_pr` and `max_cost_per_pr_usd` before spawning each iteration. If either cap is exceeded, the scheduler SHALL mark the iteration as blocked and record a failure reason in the iteration record.

#### Scenario: Max iterations exceeded
- **GIVEN** `max_iterations_per_pr` is 20
- **AND** iteration 20 just completed but AC unmet
- **WHEN** the scheduler checks before spawning iteration 21
- **THEN** iteration 21 is NOT spawned
- **AND** a failure reason `"ralph-iteration-limit-exceeded"` is recorded in the iteration record

#### Scenario: Cost cap exceeded
- **GIVEN** `max_cost_per_pr_usd` is 5.00
- **AND** accumulated ralph cost for PR #42 is 5.01
- **WHEN** the scheduler checks before spawning next iteration
- **THEN** the iteration is NOT spawned
- **AND** a failure reason `"ralph-budget-exceeded"` is recorded in the iteration record
