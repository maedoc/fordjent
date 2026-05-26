## Context

Fordjent has no quantitative way to measure whether a code change improves or regresses agent behavior. Every feature (steering system, compaction, role routing, model selection) is evaluated by anecdote. The existing `scripts/bootstrap-local.sh` provides local Forgejo+Fordjent setup but is manual and not repeatable across trials. The `/status` and `/trace` HTTP endpoints already expose runtime metrics (tokens, turns, session states, costs) but nobody collects them systematically.

Two design docs already exist (`docs/eval-harness.md` and `docs/eval-harness-local.md`) describing the desired architecture, scenarios, and metrics. This design translates those docs into implementation decisions.

## Goals / Non-Goals

**Goals:**
- Repeatable benchmark suite that runs as `go test ./internal/eval/...`
- Two calibrated scenarios (greenfield CLI, bug fix) with known-correct solutions
- Metrics collection that captures pass/fail + efficiency (tokens, turns, time, tool calls, system-role errors, false error labels)
- Baseline comparison against committed `baseline.json` with automatic regression detection
- Fast smoke test (~5 min) and full benchmark (~45-70 min)
- Native macOS execution (no Docker, matching `bootstrap-local.sh` patterns)

**Non-Goals:**
- k-sample boosting benchmark (parallel implementers per issue) — separate future work
- Cross-session memory benchmark — separate future work
- Model comparison leaderboard — same harness, separate baseline per model
- CI integration with GitHub Actions — future work, requires cloud LLM key in CI
- Benchmarking against external agent frameworks (SWE-bench, etc.) — different scope

## Decisions

### Decision 1: Native macOS, not Docker

**Choice**: Run Forgejo + Fordjent as native processes under `sandbox-exec`, same as `bootstrap-local.sh`.

**Alternate considered**: Docker Compose (as in `docker-compose.local.yaml`).

**Rationale**:
- Docker adds ~10s startup overhead per trial
- Docker networking makes Fordjent→Forgejo webhook delivery unreliable in test mode
- Docker layer caching masks source changes (Bug 18 — already experienced this)
- Native processes give direct filesystem access for `git clone` verification
- The `bootstrap-local.sh` script already works — we replicate its logic in Go
- Docker remains available for CI if needed later, but local development uses native

### Decision 2: Forgejo API for repo/issue management, not git operations

**Choice**: Create repos, seed files, create issues, and poll completion entirely through the Forgejo REST API.

**Alternate considered**: `git` CLI for repo init, `git push` for seeding.

**Rationale**:
- Forgejo API is already fully exercised by the agent — using it for setup validates the same path
- API-based seeding avoids git credential complexity in test code
- API-based polling for completion (issue state, PR state) is more reliable than git log inspection
- The `internal/forgejo/client.go` already has all needed methods: `CreateRepository`, `ListRepoFiles`, `CreateIssue`, `AddIssueLabels`, `GetIssue`

### Decision 3: Process management via `os/exec`, not Docker or systemd

**Choice**: Start Forgejo and Fordjent as child processes with `os/exec.Command`, kill them in `TearDown`.

**Alternate considered**: Docker Compose managed by Go.

**Rationale**:
- Simpler — no Docker dependency in test code
- Faster — no container startup
- `bootstrap-local.sh` already does this pattern (though with shell)
- `sandbox-exec` is macOS-native and we have profiles working
- Process cleanup is straightforward: store PIDs, kill in `TearDown`

### Decision 4: Committed `baseline.json` for regression detection

**Choice**: Store baseline metrics in `internal/eval/baseline.json`, committed to git. Tests compare current run against baseline and fail on regressions.

**Alternate considered**: Store baseline in CI artifacts or S3.

**Rationale**:
- Git-committed means baseline travels with code — any checkout has the right baseline
- Comparison is deterministic — no network call to fetch baseline
- `go test -update-baseline` flag updates baseline after confirmed improvement
- Simpler than external artifact storage

### Decision 5: Delta metrics from append-only databases

**Choice**: Record metrics snapshot at trial start and trial end. Compute delta (end - start) for per-trial metrics.

**Alternate considered**: Clear databases between trials.

**Rationale**:
- Both `costs.db` and `lifecycle.db` are append-only SQLite — schema doesn't support truncation without VACUUM
- Delta approach means we never lose historical data — useful for debugging
- Simpler — no database management, just read at two points in time
- Total metrics are still available by reading the full database for aggregate reports

### Decision 6: Two scenarios — greenfield CLI and targeted bug fix

**Choice**: Exactly two scenarios with high-contrast difficulty profiles.

**Rationale from `docs/eval-harness.md`**:
- Greenfield tests the full pipeline (PM → implementer → reviewer → merge)
- Bug fix tests targeted maintenance (read code, understand bug, minimal fix)
- These are the two most common real-world development tasks
- Each catches different regression types
- A third "integration test" scenario would add ~30 min but provide little additional signal

## Data Flow

```
TestEvalSmoke / TestEvalBenchGreenfield / TestEvalBenchBugfix
        │
        ▼
  NewHarness(t)
        │
        ├── startForgejo() ─→ sandbox-exec forgejo web ... (PID stored)
        ├── createAdmin()  ─→ forgejo admin user create ...
        ├── startFordjent() ─→ sandbox-exec fordjent -config ... (PID stored)
        └── waitForHealthy() ─→ GET /healthz, GET /api/v1/version
        │
        ▼
  RunTrial(scenario)
        │
        ├── CreateRepo(name) ─→ POST /api/v1/user/repos
        ├── SeedFiles(repo, files) ─→ POST .../contents/{path} (base64)
        ├── CreateLabels(repo) ─→ POST .../labels (FSM + role labels)
        ├── CreateWebhook(repo) ─→ POST .../hooks (→ fordjent :8080)
        ├── RecordBaseline() ─→ GET /status ─→ MetricsSnapshot{t0}
        ├── CreateIssue(repo, title, body) ─→ POST .../issues
        ├── WaitForCompletion(repo, issueNum, timeout)
        │       │
        │       ├── poll every 5s: GET /status (active sessions)
        │       ├── poll every 5s: GET .../issues/{N} (state)
        │       ├── poll every 5s: lifecycle state transitions
        │       └── timeout → hard failure
        │
        ├── RecordMetrics() ─→ GET /status ─→ MetricsSnapshot{t1}
        ├── ComputeDelta(baseline, metrics) ─→ MetricsDelta
        ├── Verify(repoDir) ─→ git clone + go build + go test + functional tests
        ├── CountSystemRoleErrors() ─→ grep fordjent logs
        └── CountFalseErrorLabels() ─→ GET .../issues?labels=fordjent/failed:error
        │
        ▼
  TrialResult{Success, Metrics, Verification, Errors}

  (for multi-trial benchmarks: ResetRepo → RunTrial → ... → AggregateResults)
  (compare against LoadBaseline() → Report)
        │
        ▼
  TearDown() ─→ kill Forgejo PID, kill Fordjent PID, rm -rf workdir
```

## Risks / Trade-offs

### Risk: LLM non-determinism makes results noisy
**Mitigation**: N=5 trials with median reporting. Binary signals (system-role errors, false error labels) are deterministic. Continuous signals (tokens, turns) require median comparison. If variance is too high, increase N or use a deterministic mock provider for some scenarios.

### Risk: Provider outages during benchmark run
**Mitigation**: Mark trials as `provider_failure: true` and exclude from median calculations. The test logs which provider was used and the error received. A trial with provider failure is not counted as an agent failure.

### Risk: Test order effects (leftover state from previous trial)
**Mitigation**: `ResetRepo()` between trials closes all issues, resets main to seed commit, waits for Fordjent to go idle. Additionally, `RecordBaseline()` captures metrics at the start of each trial so deltas are independent.

### Risk: Forgejo startup time on CI
**Mitigation**: `EVAL_SKIP_SETUP` flag reuses an already-running instance. In local dev, Forgejo starts in ~3 seconds. The 30-second health check timeout is generous.

### Risk: Port conflicts (3000, 8080 already in use)
**Mitigation**: Check port availability before starting. Use `EVAL_FORGEJO_PORT` and `EVAL_FORDJENT_PORT` env vars for alternative ports. TearDown always kills processes on configured ports.

### Risk: Baseline gaming (overfitting to specific scenarios)
**Mitigation**: Scenarios use deterministic expected outcomes (code compiles, tests pass). Baseline tracks efficiency metrics, not pass/fail — a change that hardcodes the answer would still need to produce working code via the agent loop. No scenario exposes its known-solution content to the agent.

## Open Questions

1. **Should we use `Wafer` or `Scaleway` as the default eval provider?** Wafer (qwen3.5) is free but slower; Scaleway (devstral) is faster but costs money. Recommendation: Wafer for CI, configurable via `EVAL_LLM_PROVIDER`.

2. **Should the harness build Fordjent from source or use a pre-built binary?** Building from source ensures the binary matches the current commit (critical for regression testing). But it adds ~30s to setup. Recommendation: build from source, with `EVAL_SKIP_BUILD` flag to reuse an existing binary.

3. **Should we add a third scenario for integration/cross-issue work?** The design spec mentioned this as future work. A "build then extend" scenario (build a CLI in issue #1, add a feature in issue #2) would test cross-session memory. Recommendation: defer to next iteration.