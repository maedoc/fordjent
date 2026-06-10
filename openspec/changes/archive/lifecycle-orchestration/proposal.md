# Proposal: Spec-Driven Lifecycle Orchestration

## Problem

Fordjent's agent roles (PM, implementer, reviewer/djent-qa, Ralph) are defined in isolation. Each has a craft spec describing HOW it works, but no spec defines WHEN each role activates, what events trigger handoffs, or what artifacts flow between roles. This causes:

- **Conflicting completion claims**: PM thinks it archives when "all tasks are done"; Ralph thinks it removes its label when "AC are met". Neither links to the other.
- **Ambiguous event routing**: The same `issue_comment.created` event could mean "PM should revise a spec", "Ralph should start a new iteration", or "reviewer should post feedback". The system has no explicit routing rules.
- **Shadow activation logic**: Activation triggers are scattered across PM spec, Ralph scheduler spec, and ralph-protocol spec, creating overlapping and contradictory requirements.

## What Changes

Introduce a single **Spec-Driven Lifecycle** spec that defines:
1. A unambiguous stage machine (`spec-proposed → spec-approved → implementing → reviewing → merging → archived`)
2. An event-to-role routing table (priority-ordered rules by PR labels)
3. Explicit handoff rules with artifacts
4. PM archival trigger conditions
5. Ralph's place as an implementation harness extension within `implementing`, not a lifecycle stage

## What Does Not Change

- PM's craft for writing specs, decomposition, verification contracts (`pm-spec-authoring`)
- Ralph's iteration mechanics, 4-A protocol, progress files (`ralph-scheduler`, `ralph-protocol`)
- Implementer's code-writing workflow (`spec-driven-implementation`)
- Reviewer's spec compliance checks (`spec-driven-review`)

## Impact

- Removes activation trigger duplication across PM and Ralph specs
- Makes event routing testable and explicit
- Prevents PM from archiving a change while Ralph is still iterating
- Gives reviewers a clear signal (`.ralph/progress/` files) when a PR completed Ralph mode

## Non-Goals

- New tool definitions (uses existing `forgejo_create_pr`, `write_file`, etc.)
- New DB schemas (uses existing lifecycle DB)
- New webhook event types (uses existing Forgejo events)
- Modifying Ralph's 4-A protocol or turn mechanics
