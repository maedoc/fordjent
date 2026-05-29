## Why

Fordjent's PM, implementer, and reviewer roles operate on ad-hoc issue bodies — there is no structured specification layer between "what to build" and "code to write." The human+pi workflow already uses OpenSpec (proposal → design → specs → tasks → archive) to great effect in this repo, but Fordjent agents cannot participate in or benefit from that process. Every feature change to Fordjent is evaluated by anecdote; adding OpenSpec gives both agents and humans a shared, version-controlled contract for what gets built, reviewed, and archived.

## What Changes

- **New subsystem `internal/speccycle/`**: `SpecManager` (create/list/archive changes, parse tasks.md, sync delta specs), `SpecPRManager` (detect spec PRs, generate implementer issues on merge), and enhanced scheduler rules for change lifecycle.
- **PM role extended to create specs**: PM writes `openspec/changes/<name>/{proposal,design,tasks}.md` and `specs/*.md` via `write_file`, commits to a spec branch, and opens a PR for human review (non-yolo) or commits directly to main (yolo).
- **Implementer role reads specs**: New `openspec_get_tasks` and `openspec_read_spec` tools let implementers consume the merged spec. `openspec_mark_task` updates `tasks.md` checkboxes as work progresses.
- **Reviewer role checks against specs**: Reviewer compares implementation diff against `specs/*.md` requirements. New verification contract section in specs gives reviewers a concrete rubric.
- **Scheduler gains spec lifecycle rules**: On spec PR merge → parse `tasks.md` → create `[implementer]` issues with milestones. On all tasks complete → trigger PM follow-up → archive change. On file-disjoint tasks → fan out parallel implementer sessions.
- **Lifecycle states extended**: `spec-proposed` / `spec-approved` / `spec-implementing` / `spec-complete` labels track change progress through the Forgejo UI.
- **OpenSpec CLI mechanics re-implemented in Go**: No Node.js dependency. File scaffolding, task parsing, and archive moves are done natively via Go file ops.
- **No changes to existing production behavior**: Spec cycle is additive. Issues without spec references continue to work exactly as today.

## Capabilities

### New Capabilities
- `speccycle`: Spec lifecycle management — create, list, read, update, and archive OpenSpec changes. Includes `SpecManager` for file ops, `SpecPRManager` for PR-based review, scheduler integration for task→issue generation, and label-based lifecycle tracking.
- `pm-spec-authoring`: PM role extensions for creating structured specs (proposal, design, specs/*.md, tasks.md) with parallel-task awareness, pushing to spec branches, and handling spec PR review cycles.
- `spec-driven-implementation`: Implementer role extensions for reading specs, executing tasks from `tasks.md`, marking completion, and validating against verification contracts.
- `spec-driven-review`: Reviewer role extensions for checking implementation diffs against spec requirements and verification contracts.

### Modified Capabilities
<!-- No existing specs are being modified. This is a purely additive change. -->

## Impact

- **New packages**: `internal/speccycle/` (~6 files: `manager.go`, `prmanager.go`, `tasks.go`, `archive.go`, `label.go`, `speccycle_test.go`)
- **Modified packages**: `internal/session/agent.go` (PM/implementer/reviewer prompts, new tools), `internal/session/manager.go` (scheduler spec rules, spec PR handling), `internal/tool/forgejo_tools.go` (minor — no new Forgejo API tools needed; spec ops use existing `write_file`/`read_file`/`git`)
- **New tools registered**: `openspec_propose`, `openspec_get_tasks`, `openspec_read_spec`, `openspec_mark_task`, `openspec_archive_change` (5 new LLM-visible tools)
- **New labels**: `spec-proposed`, `spec-approved`, `spec-implementing`, `spec-complete`
- **New webhook processing**: `pull_request.merged` detection for spec PRs (checking for `openspec/changes/` in file list)
- **Existing code**: Zero breaking changes. All existing issue→PR→merge flows remain untouched.
- **Dependencies**: None. All operations use Go stdlib (`os`, `path/filepath`, `strings`, `regexp`) plus existing `internal/forgejo/` client.
