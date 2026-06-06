## Why

The spec tools have two usability gaps: `openspec_propose` returns instructions but doesn't create the directory (forcing an extra round-trip via `write_file`), and there's no tool for reading spec artifacts within a change (proposal.md, design.md) — agents must use `read_file` with constructed paths. These are additive improvements that don't change existing behavior.

## What Changes

- `openspec_propose` will auto-create the change directory when called (via `speccycle.CreateChange`), so the PM can immediately start writing files.
- New `openspec_read_change` tool allows reading any file within a change directory (proposal.md, design.md, tasks.md, spec files) with path traversal protection.
- Reviewer prompt will check merge policy (NoAutoMerge/RequireReview) before attempting to merge spec-driven PRs.

## Capabilities

### New Capabilities

- `spec-change-reader`: Tool for reading spec artifacts within a change directory

### Modified Capabilities

- `spec-driven-implementation`: `openspec_propose` auto-creates change directory
- `spec-driven-review`: Reviewer prompt includes merge policy check

## Impact

- `internal/tool/openspec_tools.go` — `openspec_propose` calls `speccycle.CreateChange`; new `openspec_read_change` tool
- `internal/session/agent.go` — reviewer prompt enhancement
- `internal/session/agent.go` — register `openspec_read_change` for implementer and reviewer roles
