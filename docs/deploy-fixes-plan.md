# Fordjent Pre-Deploy Fix Plan

> Written 2026-05-07 after architectural review of Dockerfile, docker-compose, go.mod, and session manager. Assumes LLM provider reliability is separately addressed.

---

## Issue Summary

| # | Issue | Severity | Category |
|---|-------|----------|----------|
| 1 | Docker builder vs runtime Go version mismatch | 🔴 Blocker | Docker |
| 2 | No Docker network in compose files | 🔴 Blocker | Docker |
| 3 | `gopkg.in/telebot.v4` dead dependency in go.mod | 🟡 Low | Code |
| 4 | Git identity hardcoded in Dockerfile | 🟡 Low | Docker |
| 5 | Health check uses `/healthz` instead of `/readyz` | 🟠 Medium | Ops |
| 6 | `--clean` flag undocumented as recovery tool | 🟡 Low | Docs/Ops |
| 7 | Budget enforcement off by default | 🟠 Medium | Config |
| 8 | No shared GOMODCACHE across sessions | 🟠 Medium | Perf |
| 9 | No Docker log rotation configured | 🟠 Medium | Ops |
| 10 | `session_timeout` enforcement unverified | 🟡 Low | Code |
| 11 | `blocked`/`ready`/`scaffold` labels not auto-created | 🟠 Medium | Ops |
| 12 | Workdir cleanup deletes audit trails | 🟡 Low | Ops |
| 13 | Docker final image unnecessarily large | 🟢 Nice-to-have | Perf |
| 14 | Dockerfile Go version tag may not exist | 🔴 Blocker | Docker |

---

## Detailed Fixes

### 1. 🔴 Docker Builder vs Runtime Go Version Mismatch

**Problem:** The multi-stage Dockerfile builds Fordjent with `golang:1.25-alpine` (builder), then installs `golang-go` from Debian Bookworm's apt in the final stage. These are different Go versions. The final stage Go is needed for `forgejo_create_pr` verify gates (`go build`, `go test`), but if it's older (e.g., 1.19 from apt) than the go.mod directive (`go 1.25.0`), verify-gate builds fail.

**Fix:**

Option A — Copy Go from builder (recommended, most reliable):
```dockerfile
# In builder stage: install Go to /goroot
FROM golang:1.25-alpine AS builder
RUN GOLANG_VERSION=$(go version | awk '{print $3}' | sed 's/go//')

# In final stage: copy Go toolchain from builder
FROM debian:bookworm-slim AS runtime
COPY --from=builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
```

Option B — Use matching Debian-based Go image:
```dockerfile
FROM golang:1.25-bookworm AS builder
# ... build ...
FROM golang:1.25-bookworm AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential git curl ca-certificates && rm -rf /var/lib/apt/lists/*
```

**Files:** `Dockerfile`

### 2. 🔴 No Docker Network in Compose Files

**Problem:** `docker-compose.yaml` and `docker-compose.local.yaml` don't declare a network. The AGENTS.md documents `fordjent-net` as the bridge network for Forgejo ↔ Fordjent communication, but neither compose file creates it. Without a shared network, webhook delivery from Forgejo to Fordjent fails.

**Fix:**
```yaml
# docker-compose.yaml and docker-compose.local.yaml
services:
  fordjent:
    # ... existing config ...
    networks:
      - fordjent-net

networks:
  fordjent-net:
    name: fordjent-net
    external: true  # Created by bootstrap script
```

Or make it self-contained:
```yaml
networks:
  fordjent-net:
    driver: bridge
```

**Files:** `docker-compose.yaml`, `docker-compose.local.yaml`

### 3. 🟡 Telebot Dead Dependency

**Problem:** `gopkg.in/telebot.v4 v4.0.0-beta.7` is in go.mod and go.sum. The Telegram code is dormant (`enabled: false` in config) and removed from all documentation. This dependency is pulled on every build unnecessarily.

**Fix:**
- Remove `internal/telegram/` directory (router.go, responder.go, topics.go, all _test.go)
- Remove `gopkg.in/telebot.v4` from go.mod and go.sum
- Remove Telegram initialization from `cmd/fordjent/main.go` (lines 76–86)
- Remove Telegram config fields from `internal/config/config.go` (or keep as no-op for backward compat)
- Run `go mod tidy`
- Remove `TELEGRAM_BOT_TOKEN` env vars from docker-compose files

**Files:** `go.mod`, `go.sum`, `cmd/fordjent/main.go`, `internal/config/config.go`, `internal/telegram/*`, `docker-compose.yaml`, `docker-compose.local.yaml`

### 4. 🟡 Git Identity Hardcoded

**Problem:** Dockerfile has hardcoded git user/email. For multi-repo or multi-tenant deployments, commits should be attributable to the project.

**Fix:** Make git identity config-driven. Add to `fordjent.yaml` and `fordjent.local.yaml`:
```yaml
agent:
  # ... existing fields ...
  git_name: "${FORDJENT_GIT_NAME:-Fordjent Agent}"
  git_email: "${FORDJENT_GIT_EMAIL:-fordjent@forgejo.local}"
```

In Dockerfile, use a startup script instead of `RUN git config`:
```dockerfile
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["entrypoint.sh"]
```

where `entrypoint.sh` reads env vars and configures git before launching Fordjent:
```bash
#!/bin/sh
git config --global user.email "${FORDJENT_GIT_EMAIL:-fordjent@forgejo.local}"
git config --global user.name "${FORDJENT_GIT_NAME:-Fordjent Agent}"
git config --global push.default current
exec fordjent -config /etc/fordjent/fordjent.yaml "$@"
```

**Files:** `Dockerfile`, `fordjent.yaml`, `fordjent.local.yaml`, `internal/config/config.go` (+ new `scripts/entrypoint.sh`)

### 5. 🟠 Health Check Uses `/healthz` Instead of `/readyz`

**Problem:** docker-compose health check hits `/healthz` (liveness probe — returns `ok` immediately regardless of state). For readiness, `/readyz` is the correct endpoint (checks if service is initialized and ready). In orchestrated environments (Swarm, K8s), using the wrong one routes traffic prematurely.

**Fix:**
```yaml
services:
  fordjent:
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/readyz"]
      # ... rest unchanged ...
```

**Files:** `docker-compose.yaml`, `docker-compose.local.yaml`

### 6. 🟡 `--clean` Flag Undocumented

**Problem:** `cmd/fordjent/main.go` supports `-clean` to wipe persistent sessions on startup. This is a critical recovery tool for corrupted session state but isn't mentioned anywhere.

**Fix:** Add to README (already in Development section) and docs/deployment.md:
```markdown
### Recovering from session corruption
If SQLite reports database errors, restart with:
```bash
./fordjent -config my-config.yaml -clean
```
This wipes all persisted sessions and starts fresh.
```

**Files:** `README.md`, `docs/deployment.md`

### 7. 🟠 Budget Enforcement Off by Default

**Problem:** `budget.enabled: false` in `fordjent.yaml`. First-time users with paid LLM providers get no cost protection.

**Fix:**
- Set `budget.enabled: true` in `fordjent.yaml` with conservative defaults:
  ```yaml
  budget:
    enabled: true
    max_session_cost: 1.00
    max_monthly_cost: 25.00
  ```
- Add to README Quick Start a note:
  ```markdown
  > **Important:** If using a paid LLM provider, check `budget.enabled` in your config.
  > The default is conservative ($1/session, $25/month) — tune to your needs.
  ```

**Files:** `fordjent.yaml`

### 8. 🟠 No Shared GOMODCACHE

**Problem:** Each session clone independently downloads Go modules. For a project with 50+ dependencies, this adds 30–60s of network I/O per new session and burns disk.

**Fix:**
```yaml
services:
  fordjent:
    environment:
      - GOMODCACHE=/var/cache/go-mod
      - GOCACHE=/var/cache/go-build
    volumes:
      - fordjent-data:/var/lib/fordjent
      - go-cache:/var/cache
      - ./fordjent.yaml:/etc/fordjent/fordjent.yaml:ro

volumes:
  go-cache:
    driver: local
```

Note: The `bash` and `git` tools run `go build`/`go test` inside the container. GOPATH/GOMODCACHE must be set in the environment for those child processes to find the cache.

**Files:** `docker-compose.yaml`, `docker-compose.local.yaml`

### 9. 🟠 No Docker Log Rotation

**Problem:** Default Docker json-file driver with no size limits. JSONL structured logging at info level produces multiple MB/hour.

**Fix:**
```yaml
services:
  fordjent:
    logging:
      driver: "json-file"
      options:
        max-size: "100m"
        max-file: "5"
```

**Files:** `docker-compose.yaml`, `docker-compose.local.yaml`

### 10. 🟡 `session_timeout` Enforcement Unverified

**Problem:** `session_timeout: "30m"` exists in config struct but enforcement in session manager loop could not be confirmed during review. If it's not wired, tuning it is wasted effort. If it IS wired, a 30-minute cap kills long-running refactors.

**Action (needs code investigation before config change):**
1. Trace `session_timeout` field through `internal/config/config.go` → `internal/session/manager.go` → `runSession()` or the agent loop
2. If enforced: document clearly in README next to `idle_timeout`, explain difference
3. If NOT enforced: remove from config or implement with a warning
4. Default value: bump to 60m or match `max_turns` time rather than wall clock

**Files:** `internal/session/manager.go` (investigate), config files (tune)

### 11. 🟠 Labels Not Auto-Created

**Problem:** The scheduler (`internal/scheduler/`) adds/removes `blocked` and `ready` labels. The scaffold detection uses `scaffold`. The lifecycle system uses `fordjent/failed:max-turns` and `fordjent/failed:error`. None of these labels are auto-created — they silently fail if the labels don't exist in Forgejo.

**Fix:**
- Add label existence check to `internal/scheduler/scheduler.go` and `internal/scaffold/scaffold.go` before label operations
- Add a bootstrap step to the Forgejo Setup section:
  ```markdown
  ### Required Labels
  The scheduler and lifecycle systems expect these labels in your Forgejo repo:
  - `blocked` — dependency not yet merged
  - `ready` — all dependencies merged, ready for agent
  - `scaffold` — project scaffold issue
  - `fordjent/failed:max-turns` — session hit turn limit
  - `fordjent/failed:error` — session hit unrecoverable error

  Create them in **Repository → Settings → Labels → New Label**,
  or use the Forgejo API:
  ```bash
  for label in blocked ready scaffold "fordjent/failed:max-turns" "fordjent/failed:error"; do
    curl -X POST http://localhost:3000/api/v1/repos/owner/repo/labels \
      -u fjadmin:password \
      -H "Content-Type: application/json" \
      -d "{\"name\":\"$label\",\"color\":\"#cccccc\"}"
  done
  ```
  ```

**Files:** `internal/scheduler/scheduler.go`, `internal/scaffold/scaffold.go`, `README.md`

### 12. 🟡 Workdir Cleanup vs Audit Preservation

**Problem:** The `-clean` flag and idle reaper delete workdirs, including JSONL audit logs. These logs are the only reasoning trace for debugging agent failures.

**Fix:**
- Add archive step before deletion: move to `/var/lib/fordjent/archive/{session_key}/` instead of `rm -rf`
- Add retention config: `cleanup_archive_days: 30` to eventually purge archives
- In the lifecycle failure handler, copy the JSONL log content into the failure comment on the issue (so the trace is preserved in the Forgejo issue)

**Files:** `internal/session/manager.go`, `internal/lifecycle/lifecycle.go`, `internal/config/config.go`

### 13. 🟢 Docker Final Image Optimization

**Problem:** 700 MB image for a service that primarily makes HTTP calls. The Go toolchain + build-essential + golangci-lint are needed only for verify gates, which run occasionally on feature branches.

**Fix (lower priority, evaluate with real usage metrics):**
- Move Go toolchain to layered approach: slim base image for event processing, install toolchain only when needed (complex)
- OR: accept 700 MB as cost of having verify gates (simpler)
- OR: offload verify gates to Forgejo Actions runner (roadmap item)

**Current recommendation:** Accept the image size for now and revisit if it becomes a deployment pain point.

**Files:** `Dockerfile`

### 14. 🔴 Dockerfile Go Version Tag May Not Exist

**Problem:** `golang:1.25-alpine` — if Go 1.25 was released August 2025, the alpine tag exists by May 2026. But if the release was delayed, this tag doesn't exist and Docker build fails.

**Fix:**
- Check `docker manifest inspect golang:1.25-alpine` to verify tag exists
- If not: use `golang:1.24-alpine` (Feb 2025 release, definitely exists)
- Update go.mod to match: `go 1.24`
- Document minimum Go version requirement

**Files:** `Dockerfile`, `go.mod`

---

## Execution Order

### Wave 1 — Blockers (must fix before deploy)
1. [#14] Verify `golang:1.25-alpine` exists; if not, switch to `golang:1.24-alpine`
2. [#1] Fix Docker builder ↔ runtime Go version mismatch
3. [#2] Add Docker network to compose files

### Wave 2 — High-Impact Config (quick wins)
4. [#8] Mount shared GOMODCACHE + GOCACHE volumes
5. [#9] Configure Docker log rotation
6. [#7] Enable budget with conservative defaults
7. [#5] Fix health check to use `/readyz`

### Wave 3 — Cleanup & Documentation
8. [#3] Remove Telegram dependency and code
9. [#4] Make git identity config-driven
10. [#6] Document `--clean` flag
11. [#11] Add label auto-creation or bootstrap docs

### Wave 4 — Verification & Polish
12. [#10] Investigate `session_timeout` enforcement
13. [#12] Audit trail preservation on cleanup
14. [#13] Image size optimization (defer pending real metrics)

---

## Files Changed Summary

| File | Waves |
|------|-------|
| `Dockerfile` | 1, 2, 4 (major changes) |
| `docker-compose.yaml` | 1, 2 |
| `docker-compose.local.yaml` | 1, 2 |
| `fordjent.yaml` | 2, 3 |
| `fordjent.local.yaml` | 3 |
| `go.mod` | 3 |
| `go.sum` | 3 |
| `cmd/fordjent/main.go` | 3 |
| `internal/config/config.go` | 3 | — git identity fields |
| `internal/session/manager.go` | 4 | — audit preservation, timeout investigation |
| `internal/lifecycle/lifecycle.go` | 4 | — audit preservation |
| `internal/scheduler/scheduler.go` | 3 | — label existence check |
| `internal/scaffold/scaffold.go` | 3 | — label existence check |
| `README.md` | 3 | — clean flag, labels bootstrap |
| `docs/deployment.md` | 3 | — clean flag, labels bootstrap |
| `internal/telegram/*` | 3 | — **delete 5 files** |
| `scripts/entrypoint.sh` | 3 | — **new file** |
