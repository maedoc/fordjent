# Security Review: Fordjent

**Reviewer**: review-fiji  
**Date**: 2026-05-18  
**Scope**: Webhook HMAC, token handling, SQL injection, command injection, auth boundaries, sandboxing

---

## 1. Webhook HMAC Validation

### Finding 1.1 — Test endpoint has NO HMAC (HIGH)

`internal/webhook/router.go:534-603` — `handleTestMergeWebhook` has zero authentication. The comment on line 533 admits: "No HMAC validation." Any POST to `http://host:8080/acp/v1/test-merge-webhook` publishes synthetic `pull_request.closed` events to the event bus, which triggers the scheduler to unblock dependency issues. An attacker can:
- Forge merge events to bypass dependency scheduling
- Trigger unintended session creation
  
**Fix**: Either remove the test endpoint from production builds (build tag), or require HMAC validation on it when `FilterAgentEvents` is enabled. Minimum viable: add a config gate.

### Finding 1.2 — Empty secret disables all validation (MEDIUM)

`internal/webhook/router.go:606-608`:
```go
if r.cfg.Webhook.Secret == "" {
    return true // No secret configured, skip validation
}
```
If `webhook.secret` is empty (or explicitly set to `""` in YAML), ALL webhooks are accepted. Config validation at `internal/config/config.go:203` rejects `"change-me-in-production"` but does NOT reject an empty string — it only says it "must not be the default value". An empty string is not "change-me-in-production", so it passes validation but disables all signature checking.

**Fix**: In `config.go:validate()`, require `Secret != ""` explicitly (not just `!= "change-me-in-production"`).

### Finding 1.3 — Timing-safe comparison IS used (OK)

`internal/webhook/router.go:619` uses `hmac.Equal()` for signature comparison. This is correct — constant-time comparison prevents timing side-channels. ✅

### Finding 1.4 — No CSRF or origin validation (LOW)

All unauthenticated endpoints (`/healthz`, `/readyz`, `/metrics`, `/status`, `/tokens-per-minute`, `/activity`) accept GET requests from any origin. No CORS headers are set, so browser-based CSRF is limited, but the endpoints leak operational data (see Finding 3.1).

---

## 2. Token Handling

### Finding 2.1 — Tokens stored as plain strings in memory (LOW)

`internal/config/config.go:85-87` — `ProviderConfig.APIKey`, `ForgejoConfig.Token`, and `ForgejoConfig.AdminToken` are all `string` fields. They live in process memory for the lifetime of the process. If the process memory is dumped (e.g., via `/proc/pid/mem` or a debugger), keys are recoverable.

This is industry-standard for Go services — not a fixable issue without hardware security modules. Documented as accepted risk.

### Finding 2.2 — API keys injected via env var expansion (LOW)

`internal/config/config.go:123`:
```go
expanded := os.ExpandEnv(string(data))
```
Environment variables containing API keys are expanded into the raw config. If the config is ever logged (e.g., in a crash dump, debug log, or error trace), keys would be visible.

**Fix**: Ensure the config is never logged in its expanded form. Currently I don't see it logged, but a future developer adding `slog.Info("config loaded", "config", cfg)` would leak all keys.

### Finding 2.3 — Token() method exposes credential (LOW)

`internal/tool/forgejo_tools.go:843-846`:
```go
func (a *ForgejoAdapter) Token() string {
    return a.token
}
```
The Forgejo token is exposed via a getter. Any tool code (or LLM-invoked tool chain) has access. While this is internal Go code, it means the token passes through more layers than necessary. If a future tool logs the adapter, the token leaks.

**Fix**: Remove the `Token()` getter unless strictly needed. If needed for legacy compat, add a comment warning not to log it.

### Finding 2.4 — LLM provider tokens never logged (OK)

`internal/provider/client.go:278` — The API key is injected into the `Authorization` header. The key itself is never logged. HTTP error responses at line 294-296 and 291-296 include the response body but not the request headers. ✅

---

## 3. SQL Injection

### Finding 3.1 — All SQLite queries are parameterized (OK)

Every SQL query in the codebase uses `?` placeholders with parameterized arguments:
- `internal/cost/cost.go` — all queries ✅
- `internal/lifecycle/lifecycle.go` — all queries ✅
- `internal/webhook/router.go` — `queryCostDB`, `queryLifecycleDB`, `queryTokensPerMinute`, `handleActivity` all use `?` parameters ✅

**No SQL injection vulnerabilities found.** The project is consistent in using parameterized queries.

### Finding 3.2 — ORDER BY clause cannot be injected (OK)

The one dynamic clause is `ORDER BY occurred_at DESC LIMIT 30` etc. — these are hardcoded in the query strings, not constructed from user input. ✅

---

## 4. Command Injection

### Finding 4.1 — bash tool uses blacklist approach (MEDIUM)

`internal/tool/local_tools.go:49-56`:
```go
var bashBlockedPatterns = []string{
    "rm -rf /",
    "mkfs.",
    "dd if=",
    "shutdown",
    "reboot",
    "poweroff",
}
```
Blacklist is inherently incomplete. Bypasses:
- `rm -rf $HOME/../` if `$HOME=/` (unlikely but not prevented)
- `\rm -rf /` — backslash escapes alias but the pattern match is on lower-cased string, so `\rm -rf /` still contains `rm -rf /` as substring. Actually this would be caught.
- `rm -rf --no-preserve-root /` — contains `rm -rf /`, caught.
- `rm -rf "$(echo /)"` — does NOT contain `rm -rf /` (there's a space between `rf` and `/` in the pattern). Actually it does: `rf "$(echo /)` — the `/` is not adjacent to `rf`. The pattern `rm -rf /` requires a space before `/`. But `rm -rf "$(echo /)"` has `rf "$(echo /)"` which, after shell expansion, becomes `rm -rf /`. The blacklist check runs BEFORE shell expansion, so this bypasses the filter.
- `dd if=/dev/zero of=/dev/sda` — partial match on `dd if=` but `dd iflag=` or other options might slip.

**Fix**: Switch to an allowlist approach (only allow specific commands and flags), or at minimum run the command string through shell escaping. The best approach for this project's context: since the LLM already has a separate `git` tool, the `bash` tool should only allow a curated list of safe commands (`go`, `python`, `ls`, `cat`, `mkdir`, etc.) or use a command allowlist.

**Risk note**: In the current architecture, the LLM generates bash commands. If the LLM is adversarial, it can trivially bypass the blacklist. This is an accepted risk in agentic systems, but the blacklist should be strengthened.

### Finding 4.2 — git tool is properly safe (OK)

`internal/tool/local_tools.go:426` — `exec.CommandContext(ctx, "git", parts...)` uses `exec.Command` with separate arguments (no shell invocation). The args are split via `strings.Fields()`, which prevents argument injection. The commit message newline sanitization (lines 408-411) is unnecessary for this code path but harmless. ✅

### Finding 4.3 — write_file prevents path traversal (OK)

`internal/tool/local_tools.go:339-343`:
```go
absPath := filepath.Join(t.repoDir, filepath.Clean(params.Path))
repoClean := filepath.Clean(t.repoDir) + string(os.PathSeparator)
if !strings.HasPrefix(absPath, repoClean) {
    return "", fmt.Errorf("path escapes repository root: %s", params.Path)
}
```
Proper containment check. Also creates parent dirs with `0755` and writes with `0644`. ✅

### Finding 4.4 — read_file also prevents path escape (OK)

`internal/tool/local_tools.go:249-253` — same containment pattern as write_file. ✅

### Finding 4.5 — Git push blocked on protected branches (OK)

`internal/tool/local_tools.go:101-113` — bash tool blocks `git push` to protected branches (`main`, `master`). Pattern matching is reasonable but uses `strings.Contains` which could have false positives (a branch name containing "main" as a substring). Acceptable for current use. ✅

---

## 5. Authentication/Authorization Boundaries

### Finding 5.1 — Unauthenticated endpoints leak operational data (MEDIUM)

The following endpoints have no authentication and leak sensitive data:

| Endpoint | Data Leaked |
|----------|-------------|
| `/status` | Cost data per session/repo/month, active session count, failed session count, recent transitions, recent turns |
| `/tokens-per-minute` | Per-minute token consumption patterns, number of LLM calls |
| `/metrics` | Prometheus metrics: event counts, session counts, tool calls, LLM retries, cost totals |
| `/activity` | Webhook delivery history (event types, actions, senders, status), session transition timeline |

If Fordjent is exposed beyond localhost, an attacker can:
- Determine activity patterns (when agents work, how many sessions)
- Track LLM spending trajectory
- Identify the webhook delivery flow

**Fix**: Add an optional API key or token requirement for non-health endpoints. Or bind these endpoints to localhost only in production.

### Finding 5.2 — No rate limiting on any endpoint (LOW)

There's no rate limiting on `/acp/v1/events` or any other endpoint. A flood of webhooks could:
- Exhaust the session pool
- Consume disk space with SQLite data
- Trigger excessive LLM calls (and cost)
- The `isShuttingDown` check only prevents processing after graceful shutdown is initiated.

The Forgejo config has `rate_limit: 60` but this is for Forgejo API calls, not webhook ingestion.

### Finding 5.3 — Admin web UI auth unknown (LOW)

`internal/webhook/router.go:59-60` registers `/admin/` via `webui.Handler(cfg)`. The security of this handler depends on its implementation. If it has no auth, admin operations might be exposed.

---

## 6. Sandbox Profiles

### Finding 6.1 — FORDJENT_LOCAL_DIR placeholder may not be substituted (MEDIUM)

`scripts/sandbox/fordjent.sb:23-25`:
```
(allow file-write* (subpath "FORDJENT_LOCAL_DIR/fordjent-work"))
(allow file-write* (subpath "FORDJENT_LOCAL_DIR/logs"))
(allow file-write* (subpath "FORDJENT_LOCAL_DIR"))
```

Same issue in `scripts/sandbox/forgejo.sb:16-18`.

If the bootstrap script doesn't substitute `FORDJENT_LOCAL_DIR` with the actual path, these rules become no-ops (matching a directory called `FORDJENT_LOCAL_DIR` that doesn't exist). The deny regex at line 22 would then restrict ALL writes outside of `~/src/fordjent/`, which is partially protective but the intended targeted-allow rules wouldn't work.

**Fix**: Use a sentinel value that would fail loudly (e.g., `__REPLACE_ME__`) instead of something that looks like a valid variable name. Add a validation check in the bootstrap script.

### Finding 6.2 — Fordjent profile allows all outbound network (INFO)

`scripts/sandbox/fordjent.sb:6`: `(allow network-outbound)` — no restrictions. This is required for LLM API access. In a production deployment, you'd want to restrict this to known LLM API endpoints.

### Finding 6.3 — Forgejo profile restricts outbound to local (GOOD)

`scripts/sandbox/forgejo.sb:6`: `(allow network-outbound (local tcp))` — Forgejo can only reach local services, which is correct for a local dev setup (it only needs to deliver webhooks to Fordjent on the same machine). ✅

### Finding 6.4 — Sandbox-exec profiles are macOS-only (INFO)

`sandbox-exec` is macOS-specific. These profiles don't apply to Linux/Docker deployments. The Docker deployment has no sandbox beyond the container boundary. Documented in AGENTS.md as a local deployment feature.

---

## Summary

| # | Finding | Severity | File | Line(s) |
|---|---------|----------|------|---------|
| 1.1 | Test merge webhook has no HMAC | HIGH | router.go | 534-603 |
| 1.2 | Empty webhook secret passes validation | MEDIUM | config.go | 203-205 |
| 1.3 | Timing-safe comparison ✅ | — | router.go | 619 |
| 2.1 | Tokens in memory (accepted risk) | LOW | config.go, client.go | multiple |
| 2.2 | Env expansion in config (no current leak) | LOW | config.go | 123 |
| 2.3 | Token() getter exposes credential | LOW | forgejo_tools.go | 843-846 |
| 2.4 | Provider tokens not logged ✅ | — | client.go | 278 |
| 3.1 | All SQL parameterized ✅ | — | cost.go, lifecycle.go, router.go | all |
| 4.1 | bash tool blacklist bypassable | MEDIUM | local_tools.go | 49-56 |
| 4.2 | git tool proper exec ✅ | — | local_tools.go | 426 |
| 4.3-4.4 | Path containment ✅ | — | local_tools.go | 249-253, 339-343 |
| 5.1 | Unauthenticated endpoints leak data | MEDIUM | router.go | 107-204 |
| 5.2 | No rate limiting on webhooks | LOW | router.go | 401 |
| 5.3 | Admin web UI auth unknown | LOW | router.go | 59-60 |
| 6.1 | Sandbox placeholder substitution risk | MEDIUM | forgejo.sb, fordjent.sb | 23-25 |
| 6.2 | Fordjent allows all outbound | INFO | fordjent.sb | 6 |
| 6.3 | Forgejo restricted outbound ✅ | — | forgejo.sb | 6 |
| 6.4 | Sandbox is macOS-only | INFO | *.sb | all |

### Critical Fixes (Priority Order)

1. **HIGH**: Remove or HMAC-protect `/acp/v1/test-merge-webhook` in production builds
2. **MEDIUM**: Reject empty webhook secret in config validation
3. **MEDIUM**: Add auth or rate limiting to `/status`, `/tokens-per-minute`, `/metrics`, `/activity`
4. **MEDIUM**: Replace bash blacklist with allowlist approach, or at minimum expand the blacklist
5. **MEDIUM**: Fix `FORDJENT_LOCAL_DIR` substitution — use a sentinel like `__REPLACE_ME__`
6. **LOW**: Remove `Token()` getter or mark with a no-logging warning
