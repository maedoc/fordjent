## ADDED Requirements

### Requirement: ralph_update tool enforces 4-A stage ordering
The `ralph_update` tool SHALL accept two parameters: `stage` (enum: awareness, act, assert, append) and `message` (string summary). The harness SHALL validate that stages are called in order: awareness must precede act, act must precede assert, assert must precede append. Calling a stage out of order SHALL return an error.

#### Scenario: Correct stage sequence succeeds
- **GIVEN** a ralph session with no stages completed
- **WHEN** `ralph_update(stage="awareness", message="read git log, found nil guard missing")` is called
- **THEN** the call succeeds
- **AND** `stage_awareness` is recorded in the session tracker
- **AND** a success acknowledgment is returned to the agent

#### Scenario: Out-of-order stage returns error
- **GIVEN** a ralph session where only `awareness` is completed
- **WHEN** `ralph_update(stage="append", message="committing work")` is called
- **THEN** the call fails with error: "Error: must complete 'assert' before 'append'. Current completed stages: [awareness]"

#### Scenario: Duplicate stage call is idempotent
- **GIVEN** a ralph session where `awareness` is already completed
- **WHEN** `ralph_update(stage="awareness", message="re-affirming awareness")` is called
- **THEN** the call succeeds
- **AND** the message overwrites the previous awareness message

#### Scenario: assert stage requires evidence
- **GIVEN** a ralph session where act is completed
- **WHEN** `ralph_update(stage="assert", message="tests pass")` is called
- **AND** no test/build tool calls exist in the session's recent tool history
- **THEN** the call fails with error: "Error: 'assert' requires evidence from test or build tool calls"

### Requirement: Turn-based nudging guides model through protocol
The harness SHALL inject escalating steering messages based on turn budget consumption. At `turn >= budget * 0.25` and awareness not done, nudge to awareness. At `turn >= budget * 0.50` and act not done, nudge to act. At `turn >= budget * 0.75` and assert not done, nudge to assert. At final two turns and append not done, urgent nudge to append.

#### Scenario: 25% threshold nudges to awareness
- **GIVEN** turn budget is 20, current turn is 5
- **AND** awareness is not yet called
- **WHEN** the turn begins
- **THEN** a steering message is injected: "[RALPH NUDGE] You are at 25% of your turn budget. Begin the protocol: call ralph_update with stage='awareness'."

#### Scenario: 50% threshold nudges to act
- **GIVEN** turn budget is 20, current turn is 10
- **AND** awareness completed but act not started
- **WHEN** the turn begins
- **THEN** a steering message is injected: "[RALPH NUDGE] 50% of budget consumed. Call ralph_update with stage='act' and proceed with implementation."

#### Scenario: 75% threshold nudges to assert
- **GIVEN** turn budget is 20, current turn is 15
- **AND** act completed but assert not started
- **WHEN** the turn begins
- **THEN** a steering message is injected: "[RALPH NUDGE] 75% budget used. Call ralph_update with stage='assert' immediately. Document what passes and what remains."

#### Scenario: Final turns urgent nudge to append
- **GIVEN** turn budget is 20, current turn is 18
- **AND** assert completed but append not started
- **WHEN** the turn begins
- **THEN** an urgent steering message is injected: "[RALPH URGENT] Only 2 turns remaining. Call ralph_update with stage='append' and commit your work NOW."

#### Scenario: No nudge when protocol is on track
- **GIVEN** turn budget is 20, current turn is 12
- **AND** awareness and act are completed
- **WHEN** the turn begins
- **THEN** no steering message is injected

### Requirement: Ralph system prompt variant guides agent behavior
When a session key matches the `-ralph-i` pattern, the implementer system prompt SHALL include a ralph-specific section that explains the 4-A protocol, acceptance criteria awareness, spec discovery, and iteration context. The prompt SHALL include the current iteration number, the last committed SHA, and the PR number.

#### Scenario: Prompt includes iteration context
- **GIVEN** session key `fjadmin/testbed/pulls/42-ralph-i7`
- **AND** last committed SHA is `8f3a2d1`
- **WHEN** the system prompt is built
- **THEN** the prompt contains: "This is iteration 7 of ralph on PR #42"
- **AND** the prompt contains: "Previous iteration ended with commit 8f3a2d1"
- **AND** the prompt contains awareness/act/assert/append instructions

#### Scenario: Prompt includes spec discovery instructions
- **GIVEN** a ralph session in a repo with `openspec/changes/active-feature/spec.md`
- **WHEN** the system prompt is built
- **THEN** the prompt instructs: "Read the active spec at openspec/changes/<name>/spec.md during the awareness phase"
- **AND** the prompt warns: "Spec files are immutable during ralph. Do not attempt to modify them."

#### Scenario: Non-ralph session gets normal implementer prompt
- **GIVEN** session key `fjadmin/testbed/issues/15`
- **WHEN** the system prompt is built
- **THEN** no ralph-specific section is included
- **AND** the normal implementer prompt is used

### Requirement: append stage commits and pushes progress
When `ralph_update(stage="append")` is called, the harness SHALL stage all changes in the workdir, write the iteration's progress file to `.ralph/progress/`, commit with a descriptive message including the iteration number and stage summaries, and push to the PR branch.

#### Scenario: Successful append creates commit and progress file
- **GIVEN** ralph iteration 3 with stages: awareness="nil guard needed", act="added nil check", assert="tests pass"
- **WHEN** `ralph_update(stage="append", message="nil guard implemented, tests green")` is called
- **THEN** a file `.ralph/progress/pr-42-iteration-3.md` is created and staged
- **AND** all modified files are staged
- **AND** a commit is created with message:
  ```
  ralph-i3: nil guard implemented, tests green

  awareness: nil guard needed
  act: added nil check
  assert: tests pass
  append: nil guard implemented, tests green
  ```
- **AND** the commit is pushed to the PR branch
- **AND** iteration 3 status is updated to 'completed'
