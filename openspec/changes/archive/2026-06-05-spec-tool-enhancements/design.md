## Context

The existing spec tools cover the full lifecycle (propose, get_tasks, read_spec, mark_task, archive), but they have a gap: the PM cannot directly read the artifacts they've written within a change directory, and `openspec_propose` is a pure instruction tool that doesn't create any filesystem state. These are low-risk additive changes.

## Goals / Non-Goals

**Goals:**
- Make `openspec_propose` create the change directory automatically so the PM can use `write_file` immediately.
- Add a tool for reading any file within a change directory (`openspec_read_change`).
- Improve reviewer prompt to check merge policy before attempting to merge.

**Non-Goals:**
- Verification contract extraction (GAP-3 — requires parsing spec sections and running commands).
- `spec-complete` auto-label (GAP-2 — requires milestone progress tracking).
- Review round cap hard enforcement (GAP-4 — requires cross-session state).

## Decisions

### D1: `openspec_propose` auto-creates directory

Currently `openspec_propose` returns instructions but the PM must call `write_file` to create even the directory structure. The fix: `openspec_propose` calls `speccycle.NewSpecManager(repoDir).CreateChange(changeName)` before returning instructions. If the directory already exists, the tool returns an error (same as `CreateChange`'s existing behavior). The instructions are still returned after creation so the PM knows which files to write.

### D2: `openspec_read_change` tool

New tool that reads a file relative to a change directory. Parameters:
- `repository` — owner/repo (standard)
- `change_name` — the change to read from
- `file_path` — relative path within the change (e.g., `proposal.md`, `design.md`, `specs/auth-core/spec.md`)

Implementation: delegates to `speccycle.ReadChangeFile()`, which already has path traversal protection. The tool is registered for implementer and reviewer roles — implementer reads their spec, reviewer reads the spec during review.

### D3: Reviewer merge-policy check in prompt

The reviewer prompt currently says "If the PR is mergeable, call forgejo_merge_pr" without checking policy. The fix: add a sentence after the Spec-Driven Review section:

```
Before calling forgejo_merge_pr, check the merge policy:
- If repo has no-auto-merge policy: do NOT merge, post review as comment.
- If repo requires human review: do NOT merge unless PR has 'approved' label.
```

This is prompt-only — the tool's existing policy enforcement in `forgejoMergePRTool` is the hard gate.

## Risks / Trade-offs

- **`openspec_propose` with existing directory**: If the PM calls `openspec_propose` twice with the same name, the second call fails. This is correct behavior — the change already exists and the PM should use existing files.
- **`openspec_read_change` path traversal**: `ReadChangeFile` already guards against `../../` escapes. No additional risk.
- **Prompt-only merge check**: The reviewer might ignore the prompt instruction, but the tool-level gate prevents unauthorized merges regardless.
