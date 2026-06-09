## ADDED Requirements

### Requirement: write_file blocks spec paths during ralph mode
During a ralph session, the `write_file` tool SHALL reject any path matching `openspec/**/spec.md` with an error. The agent SHALL NOT be able to write to spec files regardless of path traversal attempts.

#### Scenario: Direct spec write blocked
- **GIVEN** a ralph session is active
- **WHEN** `write_file(path="openspec/changes/my-feature/spec.md", content="...")` is called
- **THEN** the tool returns error: "Error: spec files are immutable during ralph mode"
- **AND** no file is written

#### Scenario: Spec write via path traversal blocked
- **GIVEN** a ralph session is active
- **WHEN** `write_file(path="../../openspec/changes/my-feature/spec.md", content="...")` is called
- **THEN** after path resolution, the tool detects the spec path
- **AND** returns error: "Error: path resolves to spec file — spec files are immutable during ralph mode"

#### Scenario: Non-spec openspec path allowed
- **GIVEN** a ralph session is active
- **WHEN** `write_file(path="openspec/changes/my-feature/notes.md", content="...")` is called
- **THEN** the write succeeds (notes are not spec files)

#### Scenario: Non-ralph session allows spec writes
- **GIVEN** a normal implementer session (not ralph)
- **WHEN** `write_file(path="openspec/changes/my-feature/spec.md", content="...")` is called
- **THEN** the write succeeds (PM can write specs during spec creation)

### Requirement: git commit gate rejects spec modifications
The `git` tool's commit handler SHALL inspect the staged diff before executing `git commit`. If any staged file path matches `openspec/**/spec.md`, the commit SHALL be rejected with an error, regardless of whether the file was modified via `write_file` or `bash`.

#### Scenario: Commit with spec diff blocked
- **GIVEN** a ralph session where `bash` was used to modify `openspec/changes/feature/spec.md`
- **AND** the file is staged
- **WHEN** `git commit -m "update spec"` is called via the git tool
- **THEN** the commit is rejected with error: "Error: commit touches spec file openspec/changes/feature/spec.md — spec scope/AC are immutable during ralph"
- **AND** the staged changes remain in the index

#### Scenario: Commit after removing spec changes succeeds
- **GIVEN** a ralph session where both `main.go` and a spec file were modified
- **AND** the spec file changes are unstaged via `git reset HEAD openspec/...`
- **WHEN** `git commit -m "update main.go"` is called
- **THEN** the commit succeeds (only non-spec files in the commit)

#### Scenario: Progress file commit allowed
- **GIVEN** a ralph session
- **WHEN** `.ralph/progress/pr-42-iteration-3.md` is staged
- **AND** `git commit -m "ralph progress"` is called
- **THEN** the commit succeeds (progress files are not spec files)

### Requirement: ralph_progress tool writes to safe path
The `ralph_progress` tool SHALL write progress summaries to `.ralph/progress/pr-{N}-iteration-{M}.md`. It SHALL reject paths outside this directory. The tool SHALL NOT be available outside ralph mode.

#### Scenario: Progress write in ralph mode
- **GIVEN** a ralph session for PR #42, iteration 3
- **WHEN** `ralph_progress(pr_number=42, iteration=3, stage="assert", message="tests pass but benchmark regressed")` is called
- **THEN** a file `.ralph/progress/pr-42-iteration-3.md` is created in the workdir
- **AND** the file contains a markdown summary of the stage and message

#### Scenario: Progress tool not available outside ralph
- **GIVEN** a normal implementer session
- **WHEN** the tool registry is enumerated
- **THEN** `ralph_progress` is NOT in the available tools list

### Requirement: Spec immutability does not block reviewer spec sync
After ralph completion, when the `djent-qa` reviewer role processes a PR with the `ralph-completed` label, spec files SHALL be temporarily writable for TODO checkbox updates only. The reviewer prompt SHALL restrict updates to checking off completed items.

#### Scenario: QA reviewer updates spec TODOs
- **GIVEN** PR #42 has `ralph-completed` label
- **AND** `.ralph/progress/` files document completed work
- **WHEN** reviewer session starts
- **THEN** `write_file` allows modifications to `openspec/**/spec.md`
- **AND** the reviewer prompt instructs: "Only check off TODO items that are confirmed complete in .ralph/progress/. Do not add, remove, or reword any TODOs."

#### Scenario: Spec write blocked again after label removal
- **GIVEN** the QA reviewer completes spec sync
- **AND** the `ralph-completed` label is removed
- **WHEN** any subsequent session runs
- **THEN** spec files are immutable again
