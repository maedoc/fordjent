## Why

Every feature change to Fordjent (steering system, compaction, role routing, model selection) is currently evaluated by anecdote — "seems faster" or "the agent looped again." Without a repeatable, quantitative benchmark, we cannot tell whether a change improves or regresses agent performance. The eval harness provides this measurement: spin up a fresh local Forgejo+Fordjent instance, run calibrated benchmark scenarios, and collect pass/fail + efficiency metrics that are compared against a committed baseline.

## What Changes

- Add `internal/eval/` package with Go test functions that run end-to-end agent benchmarks locally
- Two benchmark scenarios: greenfield CLI build (full PM→implementer→reviewer pipeline) and maintenance bug fix (single implementer, minimal change)
- Metrics collection from Fordjent `/status` and `/trace` endpoints plus Forgejo API + git verification
- Baseline comparison with committed `baseline.json` — tests fail on regressions, warn on efficiency drift
- Fast smoke test path (~5 min) for pre-commit, full benchmark path (~45 min) for pre-merge CI
- Native macOS execution via `sandbox-exec` (matching existing `bootstrap-local.sh`)
- Inter-trial isolation (reset repo between runs, delta metrics from append-only DBs)

## Capabilities

### New Capabilities
- `eval-harness`: Reusable Go test infrastructure for running calibrated agent benchmarks on local Forgejo+Fordjent. Includes harness lifecycle (start/stop services), Forgejo API helpers, trial execution, verification, metrics collection, baseline comparison, and two concrete scenarios (greenfield CLI, bug fix).
- `eval-scenarios`: Benchmark scenario definitions with known-correct solutions. Each scenario specifies seed files, issue content, timeout, and a verification function. The greenfield scenario tests full PM→implementation→review pipeline; the bug fix scenario tests targeted maintenance.

### Modified Capabilities
<!-- No existing specs are being modified -->

## Impact

- **New package**: `internal/eval/` (~8 files, ~1500 lines of Go)
- **New dependencies**: None (uses existing `internal/forgejo/` client, `os/exec` for processes, standard library only)
- **New files**: `internal/eval/baseline.json` (committed metrics baseline)
- **New test commands**: `go test ./internal/eval/... -v -run TestEvalSmoke` (5 min), `go test ./internal/eval/... -v` (45-70 min)
- **Existing code**: No changes to production code. Eval is a test-only package.
- **Infrastructure**: Reuses `scripts/bootstrap-local.sh` patterns but reimplements in Go for test control
- **CI**: The smoke test (`TestEvalSmoke`) can run in CI with a real LLM provider key