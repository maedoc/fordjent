# Spec System Analysis — 2026-06-05

This document preserves the full findings from a review of the OpenSpec-related
features added to Fordjent. It serves as the authoritative reference for
planning fixes and test coverage.

---

## 1. Subsystems Reviewed

| # | Package / File | Role |
|---|----------------|------|
| 1 | `internal/speccycle/` (6 files, ~550 LOC) | File-level spec lifecycle: create, list, parse tasks, mark complete, archive, PR detection |
| 2 | `internal/tool/openspec_tools.go` (5 tools) | LLM-facing tools: propose, get_tasks, read_spec, mark_task, archive_change |
| 3 | `internal/session/manager.go` (5 functions) | Spec PR lifecycle handler, parallel validation, issue creation, label transitions |
| 4 | `internal/session/agent.go` (4 roles) | Prompt integration: PM spec creation, reviewer spec-driven review, implementer spec-driven impl, archive |
| 5 | `internal/eval/` (8 files, ~800 LOC) | Eval harness with greenfield + bugfix scenarios, verification, metrics, regression detection |
| 6 | `openspec/specs/` (5 spec dirs) | Living specs: speccycle, spec-driven-implementation, spec-driven-review, pm-spec-authoring, parallel-validation-hardening |
| 7 | `.pi/skills/openspec-*/` (4 skills) | Pi skills: propose, apply-change, archive-change, explore |

---

## 2. Bugs Found

### BUG-1: `git add -A` in `openspec_archive_change` sweeps unrelated files

**File**: `internal/tool/openspec_tools.go`, line ~440  
**Severity**: Medium — latent data corruption  
**Description**: `openSpecArchiveChangeTool.gitAddAll()` runs `git add -A`, staging ALL changes in the repo. If the agent has uncommitted work from other tasks (unlikely but possible in a long implementer session), everything gets swept into the archive commit.  
**Fix**: Replace `git add -A` with `git add openspec/` to scope the commit to spec files only.

```go
// Before
cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "add", "-A")

// After
cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "add", "openspec/")
```

**Risk**: Low (rarely triggered), but the fix is trivial.

---

### BUG-2: `SpecPRManager.IsSpecPR` returns only the first change name

**File**: `internal/speccycle/prmanager.go`, lines 70–73  
**Severity**: Low — edge case  
**Description**: When a PR touches multiple changes (e.g., `openspec/changes/user-auth/` and `openspec/changes/api-v2/`), only the first change name is returned. The loop:

```go
for n := range changeNames {
    return &SpecPRInfo{IsSpecPR: true, ChangeName: n}, nil
}
```

iterates over a map (random order) and returns the first entry found. The PR is processed as if it only contains one change, and the other is silently ignored.  
**Fix**: Either (a) return a slice of change names and handle multi-change PRs, or (b) document that multi-change PRs are not supported and validate at PR creation time. Option (b) is simpler and probably correct in practice — the PM is instructed to create one change per PR.

---

### BUG-3: `MarkTaskComplete` regex replacement is fragile

**File**: `internal/speccycle/tasks.go`, line `markTaskComplete`  
**Severity**: Medium — potential data corruption  
**Description**: The replacement uses `taskLineRegex.ReplaceAllString(line, "- [x] $2$3")`. If a line contains multiple occurrences of the checkbox pattern (e.g., in a prose description embedded in a task), `ReplaceAllString` will replace ALL matches in that line, not just the first one. Since the regex anchors to `^`, this is unlikely in practice, but the regex also captures the description group `$2` which could contain special characters that interact with the replacement format string.  
**Fix**: Use `ReplaceLiteral` or manually construct the replacement string instead of relying on `$2`/`$3` expansion. Or, since we already identified the line is a task line and we know the position, use simple string replacement (`strings.Replace(line, "- [ ]", "- [x]", 1)`) instead of regex replacement.

---

### BUG-4: `handleSpecPRMerged` reads tasks.md from `main` — race window

**File**: `internal/session/manager.go`, line ~1814  
**Severity**: Low — race condition  
**Description**: After a spec PR merges, `handleSpecPRMerged` reads `tasks.md` from the `main` branch via `GetFile(ctx, repo, "main", tasksPath)`. If another PR merges to `main` between the spec PR merge event and this read, the content could differ. Additionally, if the repo uses a different default branch, the read will fail or return stale content.  
**Fix**: Read from the PR's merge commit SHA instead of `main`. The `PullRequestMerged` event could carry the merge SHA, or we can fetch it from the PR object. This guarantees the exact content that was merged.

---

### BUG-5: `extractSpecChangeRef` false positives

**File**: `internal/tool/openspec_tools.go`, line `specChangeRefRegex`  
**Severity**: Low — false positive  
**Description**: The regex `(?:Spec|spec|Change|change)\s*:\s*(?:openspec/changes/)?([a-zA-Z0-9_-]+)` matches common English phrases like "climate change", "spec file", "change management". These would incorrectly extract a change name from prose.  
**Fix**: Tighten to require `openspec/changes/` prefix or the pattern `Spec: <name>` where `<name>` matches kebab-case only. Or prefix with `^` / line-start and word boundary:

```go
var specChangeRefRegex = regexp.MustCompile(`(?:^|\n)Spec:\s*([a-z][a-z0-9-]+)`)
```

---

### BUG-6: Race condition on concurrent `MarkTaskComplete` calls

**File**: `internal/speccycle/tasks.go`, `markTaskComplete`  
**Severity**: Low — mitigated by branch isolation  
**Description**: `markTaskComplete` reads the entire `tasks.md`, modifies one line, and writes the whole file back. Two concurrent calls on the same file could lose one modification. In practice, parallel implementer sessions work on different git branches, so they each have their own copy of `tasks.md`. The race would only occur if two sessions on the SAME branch (unlikely) both call `markTaskComplete` simultaneously.  
**Fix**: Not urgent — the git branch model inherently isolates this. Document as a known limitation. If needed, add a file lock or use append-only markers instead of in-place mutation.

---

### BUG-7: `openspec_mark_task` commit fails silently in long-lived sessions

**File**: `internal/tool/openspec_tools.go`, `openSpecMarkTaskTool.Execute`  
**Severity**: Medium — task tracking accuracy  
**Description**: If `git commit` or `git push` fails after `markTaskComplete` modifies the file, the function returns a warning string but the tasks.md has already been modified locally. On the next `openspec_get_tasks` call, the task appears done, but it was never committed/pushed. If the session later crashes or the branch is reset, the mark is lost.  
**Fix**: Either (a) do a dry-run verification first, or (b) if commit fails, revert the local change before returning the error. Option (b) is safer:

```go
// If commit fails, revert the local change
if err := sm.MarkTaskComplete(...); err != nil { ... }
if err := commit(); err != nil {
    // Revert: mark the task as incomplete again
    _ = sm.MarkUncomplete(params.ChangeName, params.TaskIndex)
    return warning, nil
}
```

---

## 3. Gaps vs. Specs

### GAP-1: No end-to-end spec lifecycle integration test

**Severity**: High — biggest testing gap  
**Description**: There is no test that exercises the full lifecycle:
1. PM creates spec files → pushes `spec/<name>` branch → creates PR with `spec-proposed` label
2. PR is merged → `handleSpecPRMerged` fires → implementer issues created
3. Implementer reads spec via `openspec_read_spec` → implements → marks task → creates PR
4. Reviewer validates against spec → merges
5. PM archives change

The `specpr_test.go` tests cover `handleSpecPRMerged`, `handleSpecLifecycleLabels`, `validateParallelTasks`, and `extractPathPrefixes` individually, but the connectors between them (event dispatch, session creation, tool registration, role-based prompts) are untested together.

**Proposed approach**: Add a spec-lifecycle scenario to `internal/eval/` that seeds a repo with a merged spec PR and verifies that implementer issues are created, spec tools return correct data, and the lifecycle completes. This is a unit-level integration test, not a full LLM-driven eval.

---

### GAP-2: `spec-implementing` and `spec-complete` labels are prompt-only

**Severity**: Medium — observability gap  
**Description**: The `speccycle/spec.md` spec defines four lifecycle labels: `spec-proposed`, `spec-approved`, `spec-implementing`, `spec-complete`. Only `spec-proposed → spec-approved` is automated (via `handleSpecLifecycleLabels`). The remaining transitions depend on the PM adding labels via prompt instructions. There is no code hook that adds `spec-implementing` when the first implementer starts, or `spec-complete` when all tasks finish.  
**Fix options**:
- (a) Add `spec-implementing` in `handleSpecPRMerged` after creating implementer issues (easy, deterministic)
- (b) Add `spec-complete` auto-detection: when the milestone reaches 100% closed, transition the label (requires a `PullRequestMerged` hook that re-checks milestone progress)
- (c) Accept as-is and document that these labels are approximate/PM-managed

---

### GAP-3: Verification contracts not enforced

**Severity**: Medium — spec/implementation alignment  
**Description**: The PM is instructed to include a `## Verification` section in each capability spec. The implementer is told to run those checks before creating a PR. The reviewer is told to verify independently. However, there is no tool that extracts and runs verification criteria, no gate that blocks PR creation if verification fails, and no way to programmatically check whether a spec has a `## Verification` section at all.  
**Fix options**: Out of scope for now — this is a "v2" feature. Document as deferred.

---

### GAP-4: Review round cap is prompt-only

**Severity**: Low — by design  
**Description**: The spec explicitly acknowledges this: "The review round cap is prompt-level enforcement only. If the LLM ignores the instruction, nothing blocks round 4."  
**Fix options**: Could add a session metadata counter that increments on each reviewer session for the same PR, then hard-block after 3. This requires tracking reviewer visits per PR across different sessions. Deferred.

---

### GAP-5: `extractPathPrefixes` is Go-centric

**Severity**: Medium — Python/JS repos get no overlap detection  
**Description**: The whitelist (`cmd`, `internal`, `pkg`, `api`, `web`, `docs`, `configs`, `scripts`) only covers Go project conventions. Python repos with `src/`, `app/`, `tests/`, `migrations/` and JS repos with `lib/`, `test/`, `public/`, `pages/`, `components/` won't get overlap detection, meaning parallel tasks that share files won't be downgraded.  
**Fix**: Make the whitelist dynamic by calling `scaffold.DetectProjectLang()` and choosing prefixes per language:

```go
var langPrefixes = map[string][]string{
    "go":     {"cmd", "internal", "pkg", "api", "web", "docs", "configs", "scripts"},
    "python": {"src", "app", "tests", "migrations", "scripts", "configs", "docs"},
    "javascript": {"lib", "test", "public", "pages", "components", "src", "scripts", "docs"},
    "unknown": {},  // no overlap detection
}
```

---

### GAP-6: No spec-lifecycle eval scenario

**Severity**: Medium — eval coverage gap  
**Description**: The eval harness has `GreenfieldScenario` (PM→implementer→reviewer) and `BugfixScenario` (implementer-only), but no scenario that tests the spec-driven workflow. A spec-lifecycle scenario would:
1. Seed a repo with a merged spec (proposal + tasks in openspec/)
2. Fire an issue that references the spec
3. Verify the implementer reads the spec, implements, and marks the task
4. Verify the reviewer checks against the spec  
**Fix**: Add `SpecLifecycleScenario` to `internal/eval/scenarios.go`.

---

## 4. Minor Polish Items

| # | Item | Location | Fix |
|---|------|----------|-----|
| P-1 | `openspec_archive_change` doesn't check `ChangeExists` before archiving | `openspec_tools.go` | Add `speccycle.ChangeExists()` check for better error messages |
| P-2 | `handleSpecPRMerged` logs "spec PR: implementer issues created" even if all issues fail to create | `manager.go` | Count actual successes vs failures |
| P-3 | `handleSpecLifecycleLabels` errors on `RemoveIssueLabel`/`AddIssueLabels` are silently ignored | `manager.go` | Log errors, don't just `_ =` them |
| P-4 | `openspec_propose` returns instructions but doesn't create the directory | `openspec_tools.go` | Consider calling `speccycle.CreateChange()` automatically so the PM doesn't have to create it via write_file |
| P-5 | `speccycle.Change` has a `Schema` field that's never populated | `types.go` | Remove or populate from `.openspec.yaml` |
| P-6 | `ReadChangeFile` doesn't have a corresponding tool — agents can't read their own spec artifacts through a spec-specific tool | — | Consider adding `openspec_read_change` for reading proposal.md/design.md within a change |

---

## 5. Proposed Spec Changes

To avoid a single massive change, I propose organizing into **3 focused changes**:

### Change A: `spec-fixes-hardening`
**Scope**: All bugs (BUG-1 through BUG-7) + 2 polish items (P-1, P-3)  
**Risk**: Low — all are targeted fixes with no architectural impact  
**Deliverables**:
- `git add openspec/` instead of `git add -A`
- Single-change PR detection (document, don't fix — see BUG-2)
- Simpler MarkTaskComplete replacement (BUG-3)
- Read from merge SHA instead of `main` (BUG-4)
- Tighter specChangeRefRegex (BUG-5)
- Document MarkTaskComplete race as known limitation (BUG-6)
- Rollback on commit failure (BUG-7)
- Better error logging for label transitions (P-3)
- ChangeExists check in archive tool (P-1)

### Change B: `spec-coverage`
**Scope**: Integration tests + eval scenario + lifecycle labels  
**Risk**: Medium — new test code, no production changes except `spec-implementing` label addition  
**Deliverables**:
- `TestSpecLifecycleIntegration` in `internal/session/` — exercises handleSpecPRMerged → verify issues, milestone, labels, comment
- `SpecLifecycleScenario` in `internal/eval/` — seed repo with merged spec, fire implementer issue, verify
- Auto-add `spec-implementing` label in `handleSpecPRMerged` (GAP-2 partial fix)
- Language-aware `extractPathPrefixes` (GAP-5)
- Populate `speccycle.Change.Schema` field (P-5)

### Change C: `spec-tool-enhancements`
**Scope**: Tool improvements + new tools  
**Risk**: Low — additive only, doesn't change existing behavior  
**Deliverables**:
- `openspec_propose` auto-creates the change directory (P-4)
- New `openspec_read_change` tool for reading proposal.md/design.md (P-6)
- Verification contract extraction helper (stretch goal, GAP-3 partial)
- Reviewer prompt: check merge policy before attempting merge (noted in analysis)

---

## 6. Dependency Order

```
Change A (spec-fixes-hardening)
    ↓  no dependency, but nice to fix bugs before adding tests
Change B (spec-coverage)
    ↓  integration tests should exist before tool enhancements
Change C (spec-tool-enhancements)
```

Changes A and B could be done in parallel if needed, but A is quick (patches) and B is slower (test writing), so sequential is cleaner.

---

## 7. What NOT to Spec

These items are explicitly out of scope or deferred:

| Item | Reason |
|------|--------|
| Review round cap hard enforcement (GAP-4) | Requires cross-session state tracking — invasive |
| Verification contract enforcement (GAP-3) | Requires parsing spec sections and running arbitrary commands — v2 feature |
| `spec-complete` auto-label (GAP-2 partial) | Requires milestone progress tracking on every PR merge — needs design |
| Multi-change PR support (BUG-2 fix option a) | Not a real use case — PM creates one change per PR by convention |

---

## 8. Test Baseline (Current)

Tests exist and were passing (per AGENTS.md, May 13, 2026) for:
- `internal/speccycle/` — CreateChange, ListChanges, ParseTasks, MarkTaskComplete, ArchiveChange, SpecPRManager, ReadChangeFile, ReadSpecFile, isSpecFilePath, extractChangeName
- `internal/session/specpr_test.go` — handleSpecPRMerged (3 tests), handleSpecLifecycleLabels (2 tests), validateParallelTasks (4 tests), extractPathPrefixes (2 tests), pathsOverlap (1 test), parallel overlap integration (1 test)

**Note**: Tests could not be re-run locally (Go 1.19 can't parse `go 1.25.0` in go.mod). All findings above are from static analysis.
