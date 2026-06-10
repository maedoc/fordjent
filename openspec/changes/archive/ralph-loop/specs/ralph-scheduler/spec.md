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

### Requirement: Yolo mode auto-escalates incomplete PRs to ralph
When a repo has the `fordjent-yolo` topic and an implementer session creates a PR, the harness SHALL run acceptance criteria verification before declaring the session complete. If ANY acceptance criterion is unmet (unchecked TODO in active spec, build failure, test failure, or missing required files), the harness SHALL automatically add the `ralph` label to the PR and queue the first ralph iteration.

#### Scenario: Yolo PR passes all AC — no ralph
- **GIVEN** repo `fjadmin/testbed` has topic `fordjent-yolo`
- **AND** implementer creates PR #42
- **AND** build passes, tests pass, all spec TODOs are checked
- **WHEN** the session completes
- **THEN** the `ralph` label is NOT added
- **AND** normal reviewer flow proceeds

#### Scenario: Yolo PR has unchecked spec TODO — auto-ralph
- **GIVEN** repo has topic `fordjent-yolo`
- **AND** implementer creates PR #42
- **AND** the active spec has an unchecked TODO: "Handle empty input edge case"
- **WHEN** AC verification runs
- **THEN** the `ralph` label is added to PR #42
- **AND** ralph iteration 1 is queued immediately (no cooldown for first)
- **AND** a comment is posted: "PR auto-escalated to ralph mode — acceptance criteria incomplete"

#### Scenario: Yolo PR build fails — auto-ralph
- **GIVEN** repo has topic `fordjent-yolo`
- **AND** implementer creates PR #42
- **AND** `go test ./...` fails with compilation error
- **WHEN** AC verification runs
- **THEN** the `ralph` label is added
- **AND** ralph iteration 1 is queued

### Requirement: AC verification detects completion and removes ralph label
After each ralph iteration's `append` stage, the harness SHALL verify acceptance criteria. If ALL criteria are met, the harness SHALL remove the `ralph` label, add a `ralph-completed` label briefly (for QA sync triggering), and queue a reviewer session.

#### Scenario: All AC met — ralph completes
- **GIVEN** ralph iteration 7 for PR #42
- **AND** the spec's TODO list is fully checked
- **AND** build and tests pass
- **WHEN** AC verification runs after append
- **THEN** the `ralph` label is removed
- **AND** `ralph-completed` label is added
- **AND** a reviewer session is queued for QA review
- **AND** iteration 7 status is updated to 'completed'

#### Scenario: AC still unmet — ralph continues
- **GIVEN** ralph iteration 5 for PR #42
- **AND** one TODO remains unchecked: "Add concurrent safety tests"
- **WHEN** AC verification runs
- **THEN** the `ralph` label remains
- **AND** iteration 6 is scheduled after cooldown

#### Scenario: Spec file missing on branch — skip AC verification
- **GIVEN** PR branch has no `openspec/changes/*/` directory
- **AND** the linked issue body has no acceptance criteria section
- **WHEN** AC verification runs
- **THEN** verification falls back to build/test gate only
- **AND** if build/test pass, ralph label is removed
- **AND** a comment warns: "No spec found for AC verification — used build/test only"

### Requirement: Ralph scheduler respects hard caps
The scheduler SHALL check `max_iterations_per_pr` and `max_cost_per_pr_usd` before spawning each iteration. If either cap is exceeded, the scheduler SHALL remove the `ralph` label and add the appropriate failure label.

#### Scenario: Max iterations exceeded
- **GIVEN** `max_iterations_per_pr` is 20
- **AND** iteration 20 just completed but AC unmet
- **WHEN** the scheduler checks before spawning iteration 21
- **THEN** iteration 21 is NOT spawned
- **AND** `fordjent/failed:ralph-exceeded` label is added
- **AND** `ralph` label is removed
- **AND** a PR comment is posted explaining the cap

#### Scenario: Cost cap exceeded
- **GIVEN** `max_cost_per_pr_usd` is 5.00
- **AND** accumulated ralph cost for PR #42 is 5.01
- **WHEN** the scheduler checks before spawning next iteration
- **THEN** the iteration is NOT spawned
- **AND** `fordjent/failed:ralph-budget` label is added
- **AND** `ralph` label is removed
