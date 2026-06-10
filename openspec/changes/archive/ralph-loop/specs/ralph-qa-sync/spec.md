## ADDED Requirements

### Requirement: QA reviewer syncs spec TODOs from ralph progress
When a PR has the `ralph-completed` label, the `djent-qa` reviewer session SHALL read all `.ralph/progress/pr-{N}-iteration-*.md` files, compare completed work against the active spec's TODO list, and update the spec's checkboxes to reflect verified completions. After sync, the `ralph-completed` label is removed and the PR proceeds to normal review/merge flow.

#### Scenario: QA syncs completed TODOs from progress files
- **GIVEN** PR #42 has `ralph-completed` label
- **AND** `.ralph/progress/pr-42-iteration-3.md` states "act: implemented nil guard, assert: TestNilGuard passes"
- **AND** the active spec has TODO: "- [ ] Add nil guard for empty input"
- **WHEN** reviewer session starts
- **THEN** the reviewer reads `.ralph/progress/*.md`
- **AND** updates spec TODO to: "- [x] Add nil guard for empty input"
- **AND** commits the spec update with message: "docs: sync spec TODOs from ralph progress (PR #42)"
- **AND** pushes the commit to the PR branch
- **AND** `ralph-completed` label is removed
- **AND** normal reviewer review proceeds

#### Scenario: QA reviewer does not modify unchecked TODOs
- **GIVEN** PR #42 has `ralph-completed` label
- **AND** `.ralph/progress/*.md` documents 3 of 5 TODOs as complete
- **AND** the spec has 2 remaining unchecked TODOs
- **WHEN** reviewer syncs
- **THEN** only the 3 completed TODOs are checked
- **AND** the 2 uncompleted TODOs remain unchecked
- **AND** the reviewer prompt warns: "Only check off items confirmed in .ralph/progress/. Do not modify unchecked items."

#### Scenario: QA reviewer skips sync if no progress files
- **GIVEN** PR #42 has `ralph-completed` label
- **AND** no `.ralph/progress/` files exist on the branch
- **WHEN** reviewer session starts
- **THEN** the reviewer logs a warning
- **AND** `ralph-completed` label is removed
- **AND** normal review proceeds without spec modification

#### Scenario: QA reviewer blocked from scope changes
- **GIVEN** PR #42 has `ralph-completed` label
- **AND** the reviewer attempts to reword an acceptance criterion in the spec
- **WHEN** `write_file` is called with modified AC language
- **THEN** the commit diff gate detects the change
- **AND** returns error: "Error: reviewer sync must not modify spec scope or acceptance criteria. Only check off TODOs."

### Requirement: ralph-completed label is temporary
The `ralph-completed` label SHALL only exist between ralph completion and QA sync completion. It SHALL be added by the harness when ralph detects AC met, and removed by the harness after QA sync finishes or times out. It SHALL NOT persist on merged PRs.

#### Scenario: Label lifecycle
- **GIVEN** ralph iteration 7 verifies all AC met
- **WHEN** harness removes `ralph` and adds `ralph-completed`
- **THEN** reviewer session is queued
- **AND** after reviewer sync completes, `ralph-completed` is removed
- **AND** if reviewer session times out, a cleanup task removes `ralph-completed` after 1 hour

### Requirement: Missing spec on branch skips QA sync gracefully
If the PR branch has no active spec file (no `openspec/changes/*/spec.md`), the QA reviewer SHALL skip the TODO sync step, remove the `ralph-completed` label, and proceed with normal code review.

#### Scenario: No spec found
- **GIVEN** PR #42 has `ralph-completed` label
- **AND** `openspec/` directory does not exist on the branch
- **WHEN** reviewer session starts
- **THEN** the session logs: "No spec found for TODO sync — proceeding with code review"
- **AND** `ralph-completed` label is removed
- **AND** normal reviewer flow continues
