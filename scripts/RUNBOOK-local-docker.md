# Runbook — Local Docker Deployment (Forgejo + Fordjent + Runner)

This runbook captures the *exact* commands and config that produce a working
local deployment of **Fordjent** + **Forgejo** + **Forgejo Runner**, all in
Docker containers on the `fordjent-net` bridge network. It exists so we don't
have to rediscover any of this after a volume reset or image upgrade.

Last validated: 2026-06-19.

---

## 1. Topology

```
┌──────────────────────────────────────────────────────────────────────┐
│  Host (docker engine)                                                │
│                                                                      │
│  bridge network: fordjent-net                                        │
│                                                                      │
│  ┌──────────────┐  :3000   ┌──────────────┐  :8080   ┌─────────────┐ │
│  │ forgejo-local│←────────→│  fordjent    │←────────→│  gemma LLM  │ │
│  │  forgejo:11  │          │ (local-gpu)  │          │ llama.cpp   │ │
│  └──────┬───────┘          └──────────────┘          │  host:8181  │ │
│         │ webhooks                                    └─────────────┘ │
│         ↓  /acp/v1/events (push, issues, PR)                          │
│  ┌──────────────┐  :3000                                             │
│  │ forgejo-local│  ← also polls jobs                                 │
│  └──────┬───────┘                                                   │
│         │ actions API (gRPC, /api/actions/runner.v1.RunnerService)   │
│         ↓                                                            │
│  ┌──────────────┐  ↳ spawns ephemeral job containers on fordjent-net │
│  │forgejo-runner│    (node:20-bookworm)                             │
│  │  runner:12   │                                                    │
│  └──────────────┘                                                    │
│                                                                      │
│  Port binds (host side):                                             │
│    127.0.0.1:4230 → forgejo-local:3000   (Forgejo UI + API)         │
│    127.0.0.1:8090 → fordjent:8080       (webhook + /status)         │
│    172.18.0.1:8181 → (host) gemma llama.cpp  (LLM, host network)    │
└──────────────────────────────────────────────────────────────────────┘
```

| Container        | Image                          | Host port        | Purpose                          |
|------------------|--------------------------------|------------------|----------------------------------|
| `forgejo-local`  | `codeberg.org/forgejo/forgejo:11` | `127.0.0.1:4230` | Forgejo v11 UI + API + actions   |
| `fordjent`       | `fordjent:local-gpu`           | `127.0.0.1:8090` | Agent harness (webhook + agent)  |
| `forgejo-runner` | `code.forgejo.org/forgejo/runner:12` | — (inbound only) | Executes `.forgejo/workflows/*`  |

The LLM (gemma-4-12b) runs *on the host* (not in a container) via llama.cpp at
`172.18.0.1:8181` — `172.18.0.1` is the host gateway as seen from the
`fordjent-net` bridge.

---

## 2. Prerequisites (one-time host setup)

```bash
# Create the shared bridge network (only once)
docker network create fordjent-net

# gemma LLM server on the host (already running — this is just the reference)
# llama.cpp server, OpenAI-compatible API at :8181
# model: gemma-4-12b-it-UD-Q5_K_XL.gguf
```

---

## 3. Building the Fordjent image

The Dockerfile has a multi-stage `full` target that includes Go + Python
+ golangci-lint (the agent uses this for build/test gates):

```bash
cd /home/duke/src/fordjent

# Build the image (cache-bust arg forces a fresh Go build of the binary)
CACHE_BUST=$(date +%s) docker build \
  --build-arg CACHE_BUST=$(date +%s) \
  -t fordjent:local-gpu --target full .

# Verify ralph code is gone (sanity check the right commit got baked in)
docker run --rm --entrypoint sh fordjent:local-gpu -c \
  'strings /usr/local/bin/fordjent | grep -ciE "ralph scan|ralph_sessions" || echo 0'
# expected: 0
```

---

## 4. Forgejo setup (the part that broke and we fixed)

### Why this is non-trivial
The `codeberg.org/forgejo/forgejo:9` image (9.0.3) **does not have the runners
admin API** (`/admin/runners/registration-token` returns 404). That API landed
in Forgejo v10+. The runner cannot register without it. **Use the `:11` tag.**

Also: if `INSTALL_LOCK=false` in `app.ini`, *every* request (including the
API) routes through `install/install.go` and returns 404. Fordjent then logs
`API client error 404: Not Found` on every repo/issue/labels lookup. You must
write a complete `app.ini` with `INSTALL_LOCK = true` + generated secrets
*before* Forgejo boots the first time on a fresh volume.

### Recreate the forgejo-local container (fresh volume)

```bash
# 1. Create the volume if missing
docker volume create forgejo-data

# 2. Run forgejo:11 ONCE just to generate the four required secrets
docker run --rm --name forgejo-init --network fordjent-net \
  -v forgejo-data:/data \
  codeberg.org/forgejo/forgejo:11 sh -c '
    SECRET_KEY=$(forgejo generate secret SECRET_KEY)
    INTERNAL_TOKEN=$(forgejo generate secret INTERNAL_TOKEN)
    LFS_JWT=$(forgejo generate secret LFS_JWT_SECRET)
    JWT_SECRET=$(forgejo generate secret JWT_SECRET)
    echo "SECRET_KEY=$SECRET_KEY"
    echo "INTERNAL_TOKEN=$INTERNAL_TOKEN"
    echo "LFS_JWT=$LFS_JWT"
    echo "JWT_SECRET=$JWT_SECRET"
  '
# Capture the four values printed above.

# 3. Write app.ini (substitute the four secrets — see template below)
#    Critical sections: [security] INSTALL_LOCK=true, [actions] ENABLED=true
cat > /tmp/app.ini <<EOF
APP_NAME = Forgejo Local Dev
RUN_MODE = prod

[repository]
ROOT = /data/git/repositories
DEFAULT_BRANCH = main

[repository.local]
LOCAL_COPY_PATH = /data/gitea/tmp/local-repo

[repository.upload]
TEMP_PATH = /data/gitea/uploads

[server]
APP_DATA_PATH = /data/gitea
DOMAIN           = localhost
ROOT_URL         = http://localhost:4230/
HTTP_PORT        = 3000
SSH_DOMAIN       = localhost
SSH_PORT         = 22
SSH_LISTEN_PORT  = 22
DISABLE_SSH      = true
LFS_START_SERVER = true
OFFLINE_MODE     = true

[database]
PATH = /data/gitea/gitea.db
DB_TYPE = sqlite3
LOG_SQL = false

[indexer]
ISSUE_INDEXER_PATH = /data/gitea/indexers/issues.bleve

[session]
PROVIDER = memory
PROVIDER_CONFIG = /data/gitea/sessions

[picture]
AVATAR_UPLOAD_PATH = /data/gitea/avatars
REPOSITORY_AVATAR_UPLOAD_PATH = /data/gitea/repo-avatars

[attachment]
PATH = /data/gitea/attachments

[log]
MODE = console
LEVEL = info
ROOT_PATH = /data/gitea/log

[security]
INSTALL_LOCK = true
INTERNAL_TOKEN = <INTERNAL_TOKEN>
SECRET_KEY = <SECRET_KEY>
REVERSE_PROXY_LIMIT = 1
REVERSE_PROXY_TRUSTED_PROXIES = *

[service]
DISABLE_REGISTRATION = false
REQUIRE_SIGNIN_VIEW  = false

[oauth2]
JWT_SECRET = <JWT_SECRET>

[lfs]
PATH = /data/git/lfs
JWT_SECRET = <LFS_JWT>

[webhook]
ALLOWED_HOST_LIST = *
QUEUE_LENGTH = 1000
DELIVER_TIMEOUT = 30

[actions]
ENABLED = true
DEFAULT_ACTIONS_URL = https://code.forgejo.org
EOF

# 4. Start forgejo-local with the config in place
docker run -d --name forgejo-local --network fordjent-net \
  -p 127.0.0.1:4230:3000 \
  -v forgejo-data:/data \
  --restart unless-stopped \
  codeberg.org/forgejo/forgejo:11
# Wait for it to generate its default minimal app.ini, then stop it
sleep 5 && docker stop forgejo-local

# 5. Install our app.ini over the auto-generated one
docker cp /tmp/app.ini forgejo-local:/data/gitea/conf/app.ini
docker exec forgejo-local sh -c 'chown git:git /data/gitea/conf/app.ini; chmod 644 /data/gitea/conf/app.ini'

# 6. Restart and verify
docker start forgejo-local
# Wait for API (DB migration on first boot may take ~30s)
for i in $(seq 1 60); do
  curl -sf --max-time 3 http://127.0.0.1:4230/api/v1/version >/dev/null 2>&1 && break
  sleep 1
done
curl -s http://127.0.0.1:4230/api/v1/version
# → {"version":"11.0.15+gitea-1.22.0"}
```

### Recovering fjadmin if the password is unknown / `must_change_password` flag is stuck

After an upgrade, the `must_change_password` flag on the admin user can force
a "Update your password" page that blocks API access. Reset it directly:

```bash
# Set a known password via CLI (runs as git user, not root)
docker exec --user git forgejo-local sh -c \
  "forgejo admin user change-password --username fjadmin --password 'fordjent-local-v11'"
# Clear the force-change flag in the DB
docker exec forgejo-local sh -c \
  "sqlite3 /data/gitea/gitea.db \"UPDATE user SET must_change_password=0 WHERE lower_name='fjadmin';\""
```

The current `fjadmin` password is `fordjent-local-v11` (rotate it if you care
about the local instance, though it's only on `127.0.0.1`).

### Authenticating tokens across the v9→v11 upgrade

The `access_token` table survived the v9→v11 migration — tokens in `.env`
(`FORGEJO_TOKEN`, `FORGEJO_ADMIN_TOKEN`, `ROLE_TOKEN_*`) all still work
without regeneration. They authenticate as `fjadmin` / `djent-pm` /
`djent-dev` / `djent-qa` respectively. Verify with:

```bash
TOK=$(grep ^FORGEJO_ADMIN_TOKEN /home/duke/src/fordjent/.env | cut -d= -f2)
curl -s -H "Authorization: token $TOK" http://127.0.0.1:4230/api/v1/user
# → {"login":"fjadmin","is_admin":true,...}

for var in ROLE_TOKEN_PM ROLE_TOKEN_IMPLEMENTER ROLE_TOKEN_REVIEWER; do
  tok=$(grep ^$var /home/duke/src/fordjent/.env | cut -d= -f2)
  curl -s -H "Authorization: token $tok" http://127.0.0.1:4230/api/v1/user \
    | python3 -c "import sys,json; print('$var →', json.load(sys.stdin)['login'])"
done
```

If you ever need to mint a *new* admin token with runner-management scope,
note that v11 refuses to do this via the API using an existing token ("auth
method not allowed"). Instead, get a runner registration token directly (the
runner endpoint doesn't need a new token — see §5).

### Backing up the volume before an upgrade

```bash
docker run --rm -v forgejo-data:/data -v /tmp:/backup alpine \
  sh -c 'tar czf /backup/forgejo-data-preupgrade-$(date +%Y%m%d-%H%M%S).tgz -C /data .'
```

---

## 5. Deploying the Forgejo Runner

### Get the runner registration token

In Forgejo v11 the runner registration token is a **GET** (not POST) on the
admin endpoint. The old `:9` image 404s here — another reason to be on `:11`.

```bash
TOK=$(grep ^FORGEJO_ADMIN_TOKEN /home/duke/src/fordjent/.env | cut -d= -f2)
REGTOKEN=$(curl -s --max-time 5 -H "Authorization: token $TOK" \
  http://127.0.0.1:4230/api/v1/admin/runners/registration-token \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "runner registration token: $REGTOKEN"
# → wI3i2bxWADrbhdPpHvkbvmQtEHyATGjh7bDqeThf  (current value)
```

### Write the runner config

The runner config goes in a persistent host directory so registration
credentials survive container restarts:

```bash
mkdir -p /home/duke/fordjent-runner
cat > /home/duke/fordjent-runner/config.yaml <<'EOF'
# Forgejo Runner config — local docker deployment
log:
  level: info

runner:
  capacity: 2
  timeout: 30m
  insecure: false
  fetch_timeout: 5s
  fetch_interval: 2s
  labels:
    - "ubuntu-latest:docker://node:20-bookworm"
    - "docker:docker://node:20-bookworm"

cache:
  enabled: true
  dir: "/data/cache"

container:
  network: "fordjent-net"          # join job containers to our bridge
  privileged: false
  options: ""
  workdir_parent: "/data/workspace"
  valid_volumes: []
  docker_host: "-"

host:
  workdir_parent: "/data/workspace"
EOF
```

### Register the runner (one-time)

```bash
REGTOKEN="<token from above>"
docker run --rm --network fordjent-net \
  -v /home/duke/fordjent-runner:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  code.forgejo.org/forgejo/runner:12 \
  forgejo-runner register --no-interactive \
    --instance "http://forgejo-local:3000" \
    --token "$REGTOKEN" \
    --name "fordjent-local-runner" \
    --config /data/config.yaml
# → "Runner registered successfully." writes /home/duke/fordjent-runner/.runner
```

### Start the runner daemon

The runner must run **as root** (or with the docker GID) to access
`/var/run/docker.sock`:

```bash
docker run -d --name forgejo-runner --network fordjent-net \
  --restart unless-stopped \
  --user root \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /home/duke/fordjent-runner:/data \
  code.forgejo.org/forgejo/runner:12 \
  forgejo-runner --config /data/config.yaml daemon

# Verify
docker logs forgejo-runner 2>&1 | tail -3
# → level=info msg="runner: fordjent-local-runner, with version: v12.11.1,
#    with labels: [ubuntu-latest docker], ephemeral: false, declared successfully"
# → level=info msg="[poller] launched"
```

### Smoke-test the runner with a workflow

```bash
TOK=$(grep ^FORGEJO_ADMIN_TOKEN /home/duke/src/fordjent/.env | cut -d= -f2)
# Create a repo
curl -s -X POST -H "Authorization: token $TOK" -H "Content-Type: application/json" \
  -d '{"name":"runner-test","private":false,"auto_init":true,"default_branch":"main"}' \
  http://127.0.0.1:4230/api/v1/user/repos -o /dev/null

# Add a workflow
WF=$(cat <<'YAML'
name: Smoke Test
on: [push, pull_request]
jobs:
  smoke:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo "Hello from Forgejo Runner"
YAML
)
curl -s -X POST -H "Authorization: token $TOK" -H "Content-Type: application/json" \
  -d "{\"message\":\"add workflow\",\"content\":\"$(echo "$WF" | base64 -w0)\"}" \
  http://127.0.0.1:4230/api/v1/repos/fjadmin/runner-test/contents/.forgejo/workflows/smoke.yaml -o /dev/null

# Wait for the run, then check
sleep 10
curl -s -H "Authorization: token $TOK" \
  http://127.0.0.1:4230/api/v1/repos/fjadmin/runner-test/actions/tasks?limit=5 \
  | python3 -c "
import sys,json
d=json.load(sys.stdin)
for r in d.get('workflow_runs',[]):
    print('run #%s: %s' % (r.get('run_number'), r.get('status')))
"
# → run #1: success
```

---

## 6. Fordjent configuration + redeploy

### The fordjent.local.yaml (key sections)

```yaml
forgejo:
  url: http://forgejo-local:3000          # internal network name
  token: ${FORGEJO_TOKEN}                  # fjadmin's token from .env
  admin_token: ${FORGEJO_ADMIN_TOKEN}
  role_tokens:                             # per-role Forgejo tokens
    pm: ${ROLE_TOKEN_PM}
    implementer: ${ROLE_TOKEN_IMPLEMENTER}
    devops: ${ROLE_TOKEN_DEVOPS}
    reviewer: ${ROLE_TOKEN_REVIEWER}
    tester: ${ROLE_TOKEN_TESTER}
  role_users:                              # Forgejo user each role posts as
    pm: djent-pm
    implementer: djent-dev
    devops: djent-dev
    reviewer: djent-qa
    tester: djent-qa

agent:
  # All roles use the LOCAL gemma-4-12b model.
  # NeuralWatt/Wafer are intentionally disabled (not referenced).
  role_providers:
    pm: local-gpu
    reviewer: local-gpu
    implementer: local-gpu
    tester: local-gpu
    devops: local-gpu
  fallback_provider: local-gpu
  max_turns: 75
  max_turns_implementer: 50
  max_turns_reviewer: 20

providers:
  - name: local-gpu
    api_base: http://172.18.0.1:8181/v1    # host gemma via bridge gateway
    model: gemma-4-12b-it-UD-Q5_K_XL.gguf
    max_tokens: 16384
    request_timeout: 60s                   # bump if LLM timeouts persist
    max_retries: 3

scanner:
  enabled: true
  interval: 5m
  repo: fjadmin/runner-test                # MUST point at a real repo or 404 spam
```

**Critical config gotchas**:
- `scanner.repo` must reference a repo that actually exists on the instance.
  The pre-reset config pointed at `fjadmin/sklearn-bench` (a leftover from
  cloud deployment) and caused constant `404: Not Found` spam every 5 min.
- The `ralph:` block at the end of the old config is dead — ralph was removed
  in commit `02e2aa2`. The config field is no longer read; leave it out.
- Gemma 4 12B is a **reasoning model**: normal prose answers land in
  `reasoning_content` while `content` is empty. Fordjent already handles this
  via the fallback in `internal/provider/client.go:420` (`if content == "" &&
  no tool_calls → use reasoning_content`). Do not "fix" empty comments by
  removing this fallback.
- Tool-calling works correctly: gemma emits `finish_reason: "tool_calls"` with
  proper `tool_calls` payloads.

### Deploy/redeploy Fordjent

```bash
cd /home/duke/src/fordjent

# Rebuild (see §3) if source changed, then:
docker stop fordjent && docker rm fordjent
docker run -d --name fordjent --network fordjent-net \
  -p 127.0.0.1:8090:8080 \
  -v fordjent-data:/var/lib/fordjent \
  -v /home/duke/src/fordjent/fordjent.local.yaml:/etc/fordjent/fordjent.yaml:ro \
  --env-file /home/duke/src/fordjent/.env \
  fordjent:local-gpu

# Verify clean startup (no ralph scan, no 404 spam)
docker logs fordjent 2>&1 | tail -8
# → "fordjent agent harness started", "scanner started" repo=fjadmin/runner-test,
#    "session manager started", "starting webhook server" addr=0.0.0.0:8080
```

---

## 7. Wiring a repo for Fordjent + the runner

A repo needs four things before Fordjent will productively work issues on it:
role users as collaborators, FSM labels, a webhook back to Fordjent, and
either a `go.mod`/`README.md` seed (so scaffold detection doesn't block) or
the `fordjent-yolo` topic (also skips the role-tag requirement).

```bash
TOK=$(grep ^FORGEJO_ADMIN_TOKEN /home/duke/src/fordjent/.env | cut -d= -f2)
REPO="fjadmin/runner-test"
WEBHOOK_SECRET="local-webhook-secret-12345"   # must match fordjent.local.yaml

# 1. Add role users as collaborators (write permission)
for user in djent-pm djent-dev djent-qa; do
  curl -s -X PUT -H "Authorization: token $TOK" -H "Content-Type: application/json" \
    -d '{"permission":"write"}' \
    http://127.0.0.1:4230/api/v1/repos/$REPO/collaborators/$user -o /dev/null
done

# 2. Create FSM + role + fordjent labels
LABELS="planning:0ea5db implementing:fbca04 ready:c2e07c review:fbca04 \
blocked:b60205 done:28a745 approved:28a745 rejected:b60205 scaffold:1d76db \
automerge:28a745 needs-role:b60205 in_progress:fbca04 plan-approved:28a745 \
role:implementer:207de5 role:pm:a0d5e4 role:reviewer:e9d76f role:tester:bfd4f2 \
role:devops:f9d5cc fordjent/failed:max-turns:b60205 fordjent/failed:error:b60205"
for spec in $LABELS; do
  name="${spec%%:*}"; color="${spec##*:}"
  curl -s -X POST -H "Authorization: token $TOK" -H "Content-Type: application/json" \
    -d "{\"name\":\"$name\",\"color\":\"$color\"}" \
    http://127.0.0.1:4230/api/v1/repos/$REPO/labels -o /dev/null
done

# 3. Register the webhook → Fordjent
curl -s -X POST -H "Authorization: token $TOK" -H "Content-Type: application/json" \
  -d "{
    \"type\": \"forgejo\",
    \"config\": {\"url\": \"http://fordjent:8080/acp/v1/events\", \"content_type\": \"json\", \"secret\": \"$WEBHOOK_SECRET\"},
    \"events\": [\"issues\", \"issue_comment\", \"pull_request\", \"pull_request_review_comment\", \"push\"],
    \"active\": true
  }" http://127.0.0.1:4230/api/v1/repos/$REPO/hooks -o /dev/null

# 4. Seed a go.mod (avoids scaffold-detection blocking the first issue)
GOMOD_B64=$(echo -n 'module runner-test

go 1.26' | base64 -w0)
curl -s -X POST -H "Authorization: token $TOK" -H "Content-Type: application/json" \
  -d "{\"message\":\"add go.mod\",\"content\":\"$GOMOD_B64\"}" \
  http://127.0.0.1:4230/api/v1/repos/$REPO/contents/go.mod -o /dev/null

# 5. Set the fordjent-yolo topic (skips require_role_tag, enables full automation)
#    v11 uses PUT, not POST, for the topics endpoint
curl -s -X PUT -H "Authorization: token $TOK" -H "Content-Type: application/json" \
  -d '{"topics":["fordjent-yolo","test","runner"]}' \
  http://127.0.0.1:4230/api/v1/repos/$REPO/topics -o /dev/null
```

### Triggering the agent

```bash
TOK=$(grep ^FORGEJO_TOKEN /home/duke/src/fordjent/.env | cut -d= -f2)
curl -s -X POST -H "Authorization: token $TOK" -H "Content-Type: application/json" \
  -d '{
    "title": "Write a hello world Go program",
    "body": "Create main.go that prints hello world. Add a test. Commit to a feature branch and open a PR."
  }' http://127.0.0.1:4230/api/v1/repos/fjadmin/runner-test/issues
```

Watch the agent: `docker logs -f fordjent`. The session key is
`fjadmin/runner-test/issues/N` → then `pulls/N` (reviewer) and
`pulls/N-fix` (review fix).

---

## 8. The fordjent ↔ runner interaction design

This is the productive split between the two systems. They run on the **same**
Forgejo instance and both react to repo events, but at different layers:

### Fordjent = fast in-container verify gate (pre-PR)
- Runs inside the `fordjent` container where it has Go, Python, golangci-lint,
  and a clone of the repo.
- The `forgejo_create_pr` tool runs a **build/test gate before posting the PR**:
  - Go repos: `go build ./...` and `go test ./...` (BLOCKING)
  - Python repos: `pytest` (BLOCKING on real failures, non-blocking if
    `pytest` is missing or no tests exist — distinguishes "no tests" from
    "tests failed")
  - golangci-lint (non-blocking — logs a warning, doesn't fail PR creation)
- Purpose: stop the agent from opening a PR whose code doesn't even compile.
  This is *fast* (~5s) and runs in the agent's own container — no orchestrator
  round-trip.
- After the gate passes, the agent creates the PR (with the `automerge`
  label if the repo is yolo).

### Runner = authoritative CI (post-push, post-PR)
- Reacts to `push` and `pull_request` events via `.forgejo/workflows/*.yaml`.
- Runs in isolated, ephemeral `node:20-bookworm` job containers on the
  `fordjent-net` network.
- Authoritative status checks appear on the commit/PR. Forgejo's native
  `automerge` label (set by Fordjent) merges automatically once the runner
  goes green.
- Purpose: independent verification that the code works in a clean
  environment, not the agent's deviated container.

### How they cooperate (the actual flow)
```
1. Issue filed → Fordjent implementer session starts
2. Agent writes code, runs `go build`/`go test` locally (fast gate)
3. Agent pushes branch → Forgejo dispatches push workflow (runner picks up)
4. Agent calls forgejo_create_pr (block-gate runs again, then PR created
   with `automerge` label)
5. Forgejo dispatches pull_request workflow (runner runs it)
6. Runner goes green → Forgejo native auto-merge merges the PR
7. Scheduler detects PR merge → unblocks dependent issues
```

### When to use which
| Need | Use |
|-----|-----|
| "Does it compile before I even open a PR?" | Fordjent's pre-PR gate (automatic, in `forgejo_create_pr`) |
| "Independent CI in a clean container" | Runner workflows (`.forgejo/workflows/*.yaml`) |
| "Block merge until tests pass" | Runner status check + `automerge` label (Forgejo native) |
| "Run on every push, not just PR" | Runner (Fordjent only gates at PR time) |
| "Lint / coverage / multi-OS matrix" | Runner (Fordjent's gate is build+test only) |

The two systems are **complementary, not redundant**: Fordjent's gate is a
fast, guaranteed local compile check that prevents the most embarrassing PRs;
the runner is the authoritative CI that catches environment-specific issues
and gates merges.

---

## 9. Verification (the "is it alive?" checklist)

```bash
# All three containers up
docker ps --format '{{.Names}}\t{{.Status}}' | grep -E 'fordjent|forgejo'
# → forgejo-local Up, forgejo-runner Up, fordjent Up

# Forgejo API reachable, version v11
curl -s http://127.0.0.1:4230/api/v1/version
# → {"version":"11.0.15+gitea-1.22.0"}

# Fordjent healthy, no 404 spam
curl -s http://127.0.0.1:8090/healthz
docker logs fordjent --since 1m 2>&1 | grep -c '404'
# → 0

# Gemma LLM reachable + tool-calling works
curl -s http://172.18.0.1:8181/v1/models | python3 -c \
  "import sys,json; print(json.load(sys.stdin)['data'][0]['id'])"
# → gemma-4-12b-it-UD-Q5_K_XL.gguf

# Runner connected + executing
docker logs forgejo-runner 2>&1 | grep 'declared successfully'
TOK=$(grep ^FORGEJO_ADMIN_TOKEN /home/duke/src/fordjent/.env | cut -d= -f2)
curl -s -H "Authorization: token $TOK" \
  http://127.0.0.1:4230/api/v1/repos/fjadmin/runner-test/actions/tasks?limit=3
# → workflow_runs with status:"success"
```

---

## 10. Known issues / gotchas (documented so they don't surprise again)

| Issue | Symptom | Workaround |
|-------|---------|------------|
| Forgejo `:9` has no runner API | `/admin/runners/registration-token` → 404 | Use `:11` |
| `INSTALL_LOCK=false` | All API → 404 via `install/install.go` | Write complete `app.ini` with secrets before first boot |
| Volume reset regenerates `app.ini` | `INSTALL_LOCK` reverts to false, secrets blank | Re-install `app.ini` after each fresh-volume boot |
| v9→v11 upgrade sets `must_change_password` | API auth works but UI blocks on password-change page | `UPDATE user SET must_change_password=0 ...` |
| Runner can't access docker socket | `permission denied ... /var/run/docker.sock` | Run runner container `--user root` (or `--group-add $(getent group docker | cut -d: -f3)`) |
| Runner image tag `:latest` doesn't exist | `manifest unknown` | Use `code.forgejo.org/forgejo/runner:12` |
| Token API mints new tokens → "auth method not allowed" | Can't create new admin-scoped token via API | Use the existing tokens (they survived the upgrade); runner registration doesn't need a new token |
| `scanner.repo` points at deleted repo | `404: Not Found` spam every scanner interval | Set `scanner.repo` to a real repo in `fordjent.local.yaml` |
| Gemma empty `content` on prose answers | Agent posts empty/comment-with-reasoning | Already handled by `client.go:420` fallback to `reasoning_content` |
| golangci-lint version mismatch | `go1.24 used to build golangci-lint is lower than targeted go1.25` | Non-blocking (logs a warning, PR still created). Rebuild image with newer golangci-lint if you care |
| Non-fast-forward push then 409 PR exists | `forgejo_create_pr` warns but recovers | Expected race when push event + implementer session both run; Fordjent logs "PR created, stopping implementer session" |
| Runner v11 admin UI page 404s | `/admin/runners` HTML page returns 404 | Known — use the API (`/api/v1/admin/runners/registration-token`) instead; the runner itself works fine |
| Topics endpoint | `POST /repos/.../topics` → 405 in v11 | Use `PUT` instead |

### Teardown (when you want to start completely fresh)

```bash
docker stop fordjent forgejo-runner forgejo-local
docker rm fordjent forgejo-runner forgejo-local
docker volume rm fordjent-data forgejo-data
rm -rf /home/duke/fordjent-runner   # runner registration credentials
# Leave fordjent-net + images in place for next bootstrap
```

---

## 11. Quick reference — current credentials

| Item | Value | Notes |
|------|-------|-------|
| Forgejo URL | `http://127.0.0.1:4230` | Host-side; internally `http://forgejo-local:3000` |
| Fordjent URL | `http://127.0.0.1:8090` | Host-side; internally `http://fordjent:8080` |
| Admin user / pass | `fjadmin` / `fordjent-local-v11` | Local-only, rotate if exposed |
| Admin token | in `.env` as `FORGEJO_ADMIN_TOKEN` | `e764...8eb47bf8`, all scopes |
| Bot token | in `.env` as `FORGEJO_TOKEN` | Same as admin token currently |
| Role users | `djent-pm`, `djent-dev`, `djent-qa` | Tokens in `.env` as `ROLE_TOKEN_*` |
| Webhook secret | `local-webhook-secret-12345` | In `fordjent.local.yaml` + repo webhook config |
| Runner registration token | `wI3i2bxWADrbhdPpHvkbvmQtEHyATGjh7bDqeThf` | One-shot; runner already registered |
| Runner name | `fordjent-local-runner` | Labels: `ubuntu-latest`, `docker` |
| Runner data dir | `/home/duke/fordjent-runner` | `.runner` (registration creds) + `config.yaml` |
| Gemma endpoint | `http://172.18.0.1:8181/v1` | Host llama.cpp server |
| Gemma model | `gemma-4-12b-it-UD-Q5_K_XL.gguf` | Reasoning model, ~140 tok/s |
