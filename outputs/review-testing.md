# Fordjent Test Quality & Coverage Review

## Summary

| Metric | Value |
|--------|-------|
| Total test files | 22 |
| Packages with tests | 15/21 (71%) |
| Overall coverage (avg) | ~45% |
| Tests passing | ✅ All 15 packages pass |
| Race tests | ⚠️ None run in CI |

---

## 1. Untested Packages — Risk Assessment

### `internal/metrics` (0%, 119 lines) — LOW RISK
- Pure counter/setter functions using `atomic.Int64` — trivially correct
- `Snapshot()` and `Handler()` are write-once-read-many, no logic branches
- **Recommendation**: Not urgent. Add smoke test for Handler() if time permits.

### `internal/sentinel` (0%, 92 lines) — LOW RISK
- Typed error definitions + two predicates (`IsRetryable`, `IsClientError`)
- Would benefit from a 20-line test covering each predicate
- **Recommendation**: Low priority, but quick to add.

### `internal/scaffold` (0%, 120 lines) — MODERATE RISK
- Contains branching logic: empty repo detection, collaborator setup, label creation, duplicate avoidance
- Key paths untested: `CheckAndBlock` with nil client, collaborator add race, `ListRepoFiles` error
- The `adminClient vs client` dual-path has no test coverage
- **Recommendation**: Add tests for the `CheckAndBlock` branching paths.

### `internal/webui` (0%, 331 lines) — LOW RISK
- Pure HTML dashboard, no real logic beyond SQL queries
- SQL queries have error paths that are silently ignored (lines 126, 173 in `buildDashboardData`)
- **Recommendation**: Low priority for automated tests; manual smoke test sufficient.

### `cmd/fj` (0%, 1101 lines) — HIGH RISK
- Large CLI with 14 commands and extensive argument parsing
- `getClient()` with its config loading + env var fallback is a 3-way or-gate pattern (untested)
- `loadConfig()` (lines 170-213) walks up directory tree parsing `.fj` INI files — fragile
- `detectRepo()` (lines 227-243) uses regex on git remote URLs — no test for edge cases
- `cmdIssue`, `cmdPR`, `cmdFile` each have complex multi-flag parsing
- **Recommendation**: Add unit tests for argument parsing at minimum. This is the biggest coverage gap.

### `cmd/fordjent` (0%, 88 lines) — LOW RISK
- Standard main.go pattern: load config, create services, start, wait for signal
- Hard to unit test meaningfully
- **Recommendation**: Integration test only.

### `internal/forgejo` (13.2% — only `GetIssue`, `ListComments`, `AddReaction` tested) — CRITICAL GAP
- This is the most critical untested layer — all agent<->Forgejo communication goes through `internal/forgejo/client.go`
- **Untested methods**: `GetPR`, `MergePR`, `ListOpenIssues`, `ListRepoFiles`, `PostIssueComment`, `AddCollaborator`, `CreateLabel`, `EnsureLabels`, `ListPRs`, `GetPRFiles`, `CreatePR`, `ClosePR`, `ListBranches`, `DeleteBranch`, `ListWebhooks`, `CreateWebhook`, `DeleteWebhook`, `ListLabels`, `RemoveIssueLabel`, `ListDir`, `GetFile`, `CreateOrUpdateFile`, `ListCollaborators`, `SearchCode`, `GetCurrentUser`, `GetVersion`, `CreateToken`, `ListPRReviews`, `GetRepository`, `ListUserRepos`, `CreateRepository`, `ListIssues`, `CloseIssue`, `ReopenIssue`
- The `doRequest` method (lines 129-171) that ALL API calls share is only indirectly tested through the 3 tested methods
- `escapeRepoPath` is only tested indirectly through `GetIssue` (1 of 2 possible paths — only single-slash repo, never `owner/repo-with/slashes`-style repos)
- **Risk**: A regression in `doRequest` (auth, error classification, URL construction) would break everything
- **Recommendation**: HIGHEST PRIORITY. Add a shared test helper + table-driven tests for all API methods.

---

## 2. Test Quality Analysis

### What Tests Do Well
- **Fake HTTP servers**: `forgejo/client_test.go`, `forgejo_tools_test.go`, `role_gate_test.go` all use `httptest.NewServer` — proper isolation
- **Label dedup**: `TestAddIssueLabels` validates the label-add flow (through `fakeForgejo` in interaction tests)
- **FSM transitions**: 8 interaction tests cover state transitions (done→close, invalid transitions, planning→blocked)
- **Concurrent access**: `TestManagerConcurrentAccess` (20 goroutines), `TestStore_ConcurrentAccess` (10 goroutines)
- **Session persistence**: `TestManager_RestoreSessions` validates SQLite round-trip
- **Role detection**: `TestDetectRoleFromTitle` covers 20 cases across all 5 roles

### What Tests Do Poorly

#### a) Tests that pass but don't verify (`internal/webhook/router_test.go`)
```go
// Line 117 — TestMetricsEndpoint only checks HTTP 200, not the content
func TestMetricsEndpoint(t *testing.T) {
    // ... setup ...
    if w.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d", w.Code)
    }
    // Never checks body contains "fordjent_events_total" or any metric
}
```
**Compare with** `TestRouter_Metrics` (line 165) which DOES check body content. The e2e test is redundant and weaker.

#### b) `TestProactivelyMerge` tests shallow path
The `TestOnPRMerged` only checks HTTP request paths, not that the correct labels are sent in POST bodies. The fake server accepts everything.

#### c) Unknown "failing test" — `TestBashToolSuccess`
Per AGENTS.md: *"TestBashToolSuccess still fails due to Alpine lacking bash"* — this is a known pre-existing failure that's being ignored. It should be `t.Skip()` with a clear message or the test should be removed.

#### d) `TestAddReactionToIssue` / `TestAddReactionToComment` are thorough
These are good examples: they verify HTTP method, path, and request body content. More tests should follow this pattern.

---

## 3. Test Isolation & Parallel Safety

### Strengths
- All tests use `t.TempDir()` — automatic cleanup, no collisions
- `httptest.NewServer` ensures unique ports, no port conflicts
- `event.NewBus()` creates fresh bus per test
- Manager tests create fresh `NewManager` per test case

### Weaknesses
- **No `t.Parallel()` anywhere** — tests run sequentially. Some share global state:
  - `internal/metrics/metrics.go` uses package-level `atomic.Int64` variables. Parallel tests would produce flaky assertions.
  - `internal/config` tests may have global state (e.g., env vars via `os.Setenv`)
- **Global slog**: `cmd/fordjent/main.go` calls `slog.SetDefault()` — no test isolation for this
- **Env var leakage**: `cmd/fj/main.go`'s `loadConfig()` calls `os.Setenv("FORGEJO_TOKEN", val)` — this leaks to other tests

### Goroutine Leak Risk
- `runSession` starts a goroutine per session with `go m.runSession(sessCtx, sess)` — tests create sessions but don't always wait for goroutines to complete
- `reapIdle` runs in a goroutine via `Run()` — test `TestManagerCreatesSession` calls `getOrCreate` without calling `Run()`, so goroutine doesn't start, but `shutdownAll` is not consistently called
- `TestConcurrentAccess` in manager creates 20 sessions without calling `Run()` nor `shutdownAll` — goroutines from `getOrCreate` may leak

---

## 4. Edge Case Coverage

| Edge Case | Tested? | Location |
|-----------|---------|----------|
| HTTP 404 from API | ✅ Partial | `TestGetIssueNotFound` |
| HTTP 500 from API | ✅ | `TestToolAPIError` |
| Malformed JSON | ✅ | `TestToolBadJSON` |
| Missing webhook signature | ✅ | `TestWebhookMissingSignature` |
| Missing event header | ✅ | `TestWebhookMissingEventHeader` |
| PR closed → skip comment | ✅ | `TestClosedPRCommentGuard`, `TestOpenPRCommentNotSkipped` |
| Human comment NOT filtered | ✅ | `TestIsAgentEvent_HumanCommentNotFiltered` |
| Bot sender comment filtered | ✅ | `TestIsAgentEvent_BotSenderComment` |
| Push events NEVER filtered | ✅ | `TestWebhookLoopPrevention`, `TestIsAgentEvent_PushPassthrough` |
| Branch auto-rebase succeeds | ✅ | `TestIsStale_AutoRebaseSucceeds` |
| Branch auto-rebase conflicts | ✅ | `TestIsStale_AutoRebaseConflicts` |
| Merge queue: same branch PR | ✅ | `TestCheckGate_SelfBranch` |
| Merge queue: file overlap | ✅ | `TestCheckGate_WithConflict` |
| **NOT TESTED:** Network timeout | ❌ | `client.go:129` `doRequest` has no context cancellation test |
| **NOT TESTED:** Empty repo post-scaffold | ❌ | `scaffold.go` race condition not tested |
| **NOT TESTED:** Concurrent label creation | ❌ | `EnsureLabels` race in `handleEvent` not tested |
| **NOT TESTED:** Session recovery after crash | ❌ | `restoreSessions` paths untested |
| **NOT TESTED:** SQL injection in payload | ❌ | Webhook payload to SQL queries |
| **NOT TESTED:** Forked repo scenarios | ❌ | `detectRepo` in `cmd/fj` untested |
| **NOT TESTED:** Boundary: max_turns=1, max_turns=75 | ❌ | Only tested with default (5) |

---

## 5. Integration vs Unit Test Balance

### e2e Tests (`internal/e2e/e2e_test.go`, 129 lines)
- Only 3 tests: `TestWebhookToEvent`, `TestHealthEndpoint`, `TestMetricsEndpoint`
- `TestWebhookToEvent` is the only real integration test — sends a POST through the router and checks the event bus
- Uses `testE2EConfig` with `FilterAgentEvents: false` — doesn't test the filtering path
- **Missing**: No integration test that exercises the full webhook→event→manager→session pipeline
- **Missing**: No integration test with a real or fake LLM provider

### Webhook Tests (`internal/webhook/router_test.go`, 780 lines)
- Focus heavily on `normalizeEvent` (10 tests) and `isAgentEvent` (9 tests)
- These are the most well-tested functions in the entire project
- But `handleWebhook`, `handleTestMergeWebhook`, `handleStatus`, `handleTokensPerMinute`, `handleActivity` have no direct tests
- The test-merge-webhook endpoint and /status endpoint are completely untested

### Session Tests (2,266 lines across 5 files)
- **Well-structured**: clear separation between store (220 lines), manager (356 lines), agent (501 lines), interaction (801 lines), role gate (388 lines)
- **Interaction tests** are the most valuable — they test real Manager.handleEvent() flow with fake Forgejo servers
- **Agent tests** are thin — mainly test `buildSystemPrompt` and `issueStateInstructions`, but don't test `ProcessEvent` at all
- **Risk**: Agent tests don't actually call the LLM, so the turn loop is never exercised in tests. The retry/backoff/compaction code in the agent is entirely untested.

---

## 6. Coverage Recommendations Prioritized by Risk

### P0 — Critical (add within 1 week)
1. **`internal/forgejo/client.go` — Table-driven test for ALL API methods** (affects everything)
   - Add a shared test helper `newClientTestServer` that validates auth header, method, path encoding
   - Parameterize endpoint paths, expected status codes, error cases
   - Current 13.2% coverage on the most critical package is unacceptable

2. **`internal/session/agent.go` — `ProcessEvent` loop** (untested core logic)
   - The turn loop with retry, stall detection, reflection checkpoints, and FSM tool blocking has zero test coverage
   - At minimum: test that stall detection fires, that FSM blocking returns correct errors, that analysis mode blocks implementation tools

### P1 — High (add within 1 sprint)
3. **`cmd/fj/main.go` — Argument parsing tests**
   - 14 CLI commands with complex flag parsing
   - Test `detectRepo`, `loadConfig`, `getClient` in isolation
   - Use `os.Args` manipulation or a `Run([]string)` wrapper

4. **`internal/scaffold/scaffold.go` — `CheckAndBlock` branching paths**
   - Test: nil client, adminClient vs client path, empty repo, already-has-go.mod, label creation failure

5. **Network error handling in `doRequest`**
   - `client.go:153` — test context cancellation mid-request
   - `client.go:159` — test response body > 10MB limit

### P2 — Medium
6. **Webhook router untested endpoints**
   - `/status`, `/tokens-per-minute`, `/activity`, `/admin`, `/acp/v1/test-merge-webhook`

7. **Session manager `handleEvent` edge cases**
   - Max sessions reached, event queue full, empty SessionKey

8. **Race condition tests**
   - `EnsureLabels` concurrent calls in `handleEvent`
   - `detectRoleFromIssue` vs label addition race
   - Add `-race` to test CI

### P3 — Nice to have
9. **`internal/metrics` smoke test** — verify `Handler()` returns Prometheus format
10. **`internal/sentinel` predicate tests** — `IsRetryable`, `IsClientError`
11. **`internal/webui` smoke test** — verify `/admin` returns 200, `buildDashboardData` handles DB errors gracefully
12. **`cmd/fordjent`** — Add a `shutdown` test (start, signal, validate clean shutdown)

---

## Per-File Line Number Findings

### Critical Untested Code Paths (by line number)

**`internal/forgejo/client.go`:**
- L104-116: `GetPR` — no test (should verify head branch, user, state)
- L118-126: `MergePR` — no test (should verify merge style, `allow_unrelated_histories`)
- L129-171: `doRequest` — only tested through 3 methods, never for auth types, large body, timeout
- L176-182: `escapeRepoPath` — only tested with single-slash, not with special chars
- L244-304: `AddIssueLabels` — partial test through interaction fakes, not direct
- L306-311: `RemoveIssueLabel` — no direct test
- L327-339: `ListOpenIssues` — no test
- L341-364: `ListRepoFiles` — no test
- L366-371: `PostIssueComment` — no direct test, only through `interactionForgejo` fake
- L380-417: `CreateLabel`, `EnsureLabels` — no test for 422 conflict handling
- L459-475: `CreateRepository` — no test
- L479-500: `ListIssues`, `CloseIssue`, `ReopenIssue` — no tests
- L546-557: `GetPRFiles` — no test
- L559-579: `CreatePR` — no test
- L598-624: `ListBranches` — no test for nested commit.id field
- L644-682: `ListWebhooks`, `CreateWebhook` — no tests
- L693-712: `ListLabels` — no test
- L726-748: `ListDir` — no test for single-file fallback
- L769-781: `CreateOrUpdateFile` — no test for sha handling
- L814-830: `SearchCode` — no test for nested response
- L835-845: `GetCurrentUser` — no test
- L872-884: `CreateToken` — no test
- L899-911: `ListPRReviews` — no test

**`internal/webhook/router.go`:**
- L131-148: `handleTokensPerMinute` — untested
- L150-204: `handleActivity` — untested
- L206-242: `queryCostDB` — untested
- L244-306: `queryLifecycleDB` — untested
- L308-381: `queryTokensPerMinute` — untested
- L532-603: `handleTestMergeWebhook` — untested
- L710-789: `isAgentEvent` — well tested (9 tests)

**`internal/session/manager.go`:**
- L165-220: `restoreSessions` — partially tested (happy path only)
- L747-766: `reapIdle` — no test for actual idle timeout
- L770-808: `cleanupOldWorkDirs` — no test
- L810-836: `evictOldest` — partially tested through max sessions test
- L838-847: `shutdownAll` — tested in `TestManagerShutdownAll`
- L851-876: `Drain` — no test
- L998-1038: `handleRoleAssignment` — indirectly tested but synthetic event recursion not tested

**`internal/session/agent.go`:**
- L174-341: Full `ProcessEvent` turn loop — NOT tested at all
- L346-357: `addReaction` — no test for error handling
- L374-530: `buildSystemPrompt` — tested (string contains checks), but no test for full prompt construction
- L542-576: `buildContext` — no test for error handling when Forgejo API fails
- L578-611: `eventToUserMessage` — no test
- L614-640: `detectAnalysisMode` — no test
- L642-656: `detectIssueState` — tested
- L667-721: `buildRoleRegistry` — tested

**`internal/lifecycle/lifecycle.go`:** 35.5% coverage — the failure labeling/commenting paths are mostly untested

---

## Recommendations Summary

| Priority | What | Why |
|----------|------|-----|
| **P0** | `forgejo/client.go` table-driven tests | 13.2% coverage on the most critical package; regressions here break everything |
| **P0** | `session/agent.go ProcessEvent` tests | The entire agent turn loop is untested — compaction, stall detection, FSM blocking, retry |
| **P0** | Add `-race` to CI tests | No race detection anywhere; concurrent session access has known SQLite BUSY errors |
| **P1** | `cmd/fj` argument parsing | 1101 lines, 0% coverage; CLI flag parsing is fragile |
| **P1** | `scaffold/scaffold.go` branching paths | Empty-repo detection; adminClient vs client dual-path |
| **P1** | Network timeout/error in `doRequest` | All Forgejo calls share this code path |
| **P2** | Webhook handler endpoint tests | /status, /tokens-per-minute, /activity, /test-merge-webhook are completely untested |
| **P2** | Session recovery and `restoreSessions` | Key production feature; only happy path tested |
| **P2** | `TestBashToolSuccess` — skip or fix | Known failure undermines confidence in test suite |
| **P3** | metrics, sentinel, webui smoke tests | Low risk but quick to add |
