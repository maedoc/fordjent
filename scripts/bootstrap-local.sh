#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
FORGEJO_BIN="/opt/homebrew/opt/forgejo/bin/forgejo"
LOCAL_DIR="${FORDJENT_LOCAL_DIR:-$HOME/fordjent-local}"
FORGEJO_PORT=3000
FORDJENT_PORT=8080
ADMIN_USER="fjadmin"
ADMIN_PASS="REDACTED"
TEST_REPO="testbed"
WAFER_API_KEY="${WAFER_API_KEY:-}"
WEBHOOK_SECRET="REDACTED"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${GREEN}[bootstrap]${NC} $*"; }
warn() { echo -e "${YELLOW}[bootstrap]${NC} $*"; }
die()  { echo -e "${RED}[bootstrap]${NC} $*" >&2; exit 1; }

# ── 1. Prerequisites ──────────────────────────────────────────────────────

log "Checking prerequisites..."

command -v go >/dev/null || die "Go not found. Install: brew install go"
command -v sandbox-exec >/dev/null || die "sandbox-exec not found (macOS only)"
command -v git >/dev/null || die "git not found"

if [ ! -x "$FORGEJO_BIN" ]; then
    log "Installing Forgejo via Homebrew..."
    brew install forgejo
fi

if [ -z "$WAFER_API_KEY" ]; then
    die "WAFER_API_KEY not set. Export it before running: export WAFER_API_KEY=wfr_..."
fi

# ── 2. Check ports ────────────────────────────────────────────────────────

for port in $FORGEJO_PORT $FORDJENT_PORT; do
    if lsof -i ":$port" -sTCP:LISTEN >/dev/null 2>&1; then
        die "Port $port is already in use. Stop the existing process or change the port."
    fi
done

# ── 3. Create workdir structure ───────────────────────────────────────────

log "Setting up $LOCAL_DIR..."

mkdir -p "$LOCAL_DIR"/{forgejo-data,fordjent-work,logs,pids}

# ── 4. Generate Forgejo app.ini ───────────────────────────────────────────

cat > "$LOCAL_DIR/app.ini" << 'INIEOF'
APP_NAME = Forgejo Local Dev
RUN_MODE = prod

[server]
DOMAIN = localhost
ROOT_URL = http://localhost:3000/
HTTP_PORT = 3000
SSH_DOMAIN = localhost
SSH_PORT = 2222
DISABLE_SSH = true
LFS_START_SERVER = true

[database]
DB_TYPE = sqlite3
PATH = LOCAL_DIR/forgejo-data/forgejo.db

[service]
DISABLE_REGISTRATION = true
REQUIRE_SIGNIN_VIEW = false
DEFAULT_ALLOW_CREATE_ORGANIZATION = false

[webhook]
ALLOWED_HOST_LIST = *
QUEUE_LENGTH = 1000
DELIVER_TIMEOUT = 30

[repository]
DEFAULT_PRIVATE = public
DEFAULT_BRANCH = main

[security]
INSTALL_LOCK = true
INTERNAL_TOKEN = local-dev-internal-token-not-for-production
SECRET_KEY = local-dev-secret-key-not-for-production

[log]
MODE = file
LEVEL = Info
ROOT_PATH = LOGS_DIR

[log.file]
FILE_NAME = forgejo.log

[mailer]
ENABLED = false

[openid]
ENABLE_OPENID_SIGNIN = false
ENABLE_OPENID_SIGNUP = false

[session]
PROVIDER = memory

[cache]
ADAPTER = memory

[packages]
ENABLED = false

[actions]
ENABLED = false
INIEOF

sed -i '' "s|LOGS_DIR|$LOCAL_DIR/logs|g" "$LOCAL_DIR/app.ini"
sed -i '' "s|LOCAL_DIR|$LOCAL_DIR|g" "$LOCAL_DIR/app.ini"

# ── 5. Prepare sandbox profiles ───────────────────────────────────────────

for sb in "$PROJECT_DIR/scripts/sandbox/forgejo.sb" "$PROJECT_DIR/scripts/sandbox/fordjent.sb"; do
    if [ ! -f "$sb" ]; then
        die "Sandbox profile not found: $sb"
    fi
    sed "s|FORDJENT_LOCAL_DIR|$LOCAL_DIR|g" "$sb" > "$LOCAL_DIR/$(basename "$sb")"
done

log "Sandbox profiles written to $LOCAL_DIR/"

# ── 6. Start Forgejo ─────────────────────────────────────────────────────

log "Starting Forgejo on port $FORGEJO_PORT..."

sandbox-exec -f "$LOCAL_DIR/forgejo.sb" \
    "$FORGEJO_BIN" web \
    --work-path "$LOCAL_DIR/forgejo-data" \
    --config "$LOCAL_DIR/app.ini" \
    > "$LOCAL_DIR/logs/forgejo-stdout.log" 2>&1 &
FORGEJO_PID=$!
echo "$FORGEJO_PID" > "$LOCAL_DIR/pids/forgejo.pid"

log "Forgejo PID: $FORGEJO_PID, waiting for it to be ready..."

READY=false
for i in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:$FORGEJO_PORT/api/v1/version" >/dev/null 2>&1; then
        READY=true
        break
    fi
    sleep 1
done

if ! $READY; then
    die "Forgejo did not become ready within 60s. Check $LOCAL_DIR/logs/forgejo-stdout.log"
fi

log "Forgejo is ready (version: $(curl -sf http://127.0.0.1:$FORGEJO_PORT/api/v1/version 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("version","?"))' 2>/dev/null || echo '?'))"

# ── 7. Create admin user + tokens via API ─────────────────────────────────

# Forgejo's web server auto-migrates the DB on first start.
# We use the API (not the CLI) to avoid SQLite locking conflicts
# since forgejo web holds the DB open.

log "Creating admin user $ADMIN_USER via API..."

# First registration must go through the install API or admin API.
# On a fresh install with DISABLE_REGISTRATION=true, we need to use
# the CLI with forgejo web stopped. Alternative: stop forgejo,
# create user, restart.
log "Stopping Forgejo briefly to create admin user (SQLite lock)..."
kill "$FORGEJO_PID" 2>/dev/null || true
sleep 2

TOKEN_OUTPUT=$("$FORGEJO_BIN" admin user create \
    --work-path "$LOCAL_DIR/forgejo-data" \
    --config "$LOCAL_DIR/app.ini" \
    --username "$ADMIN_USER" \
    --password "$ADMIN_PASS" \
    --email "admin@local" \
    --admin \
    --access-token \
    --access-token-name "fordjent-bot" \
    --access-token-scopes "all" \
    --must-change-password=false \
    2>&1)

FORGEJO_TOKEN=$(echo "$TOKEN_OUTPUT" | grep -oE '[0-9a-f]{40}' | tail -1)

if [ -z "$FORGEJO_TOKEN" ]; then
    FORGEJO_TOKEN=$("$FORGEJO_BIN" admin user generate-access-token \
        --work-path "$LOCAL_DIR/forgejo-data" \
        --config "$LOCAL_DIR/app.ini" \
        --username "$ADMIN_USER" \
        --token-name "fordjent-bot" \
        --scopes "all" \
        --raw 2>&1)
fi

if [ -z "$FORGEJO_TOKEN" ]; then
    die "Failed to generate Forgejo token. Output was: $TOKEN_OUTPUT"
fi

log "Bot token: ${FORGEJO_TOKEN:0:8}..."

ADMIN_TOKEN=$("$FORGEJO_BIN" admin user generate-access-token \
    --work-path "$LOCAL_DIR/forgejo-data" \
    --config "$LOCAL_DIR/app.ini" \
    --username "$ADMIN_USER" \
    --token-name "fordjent-admin" \
    --scopes "all" \
    --raw 2>&1)

log "Admin token: ${ADMIN_TOKEN:0:8}..."

# Restart Forgejo
log "Restarting Forgejo..."
sandbox-exec -f "$LOCAL_DIR/forgejo.sb" \
    "$FORGEJO_BIN" web \
    --work-path "$LOCAL_DIR/forgejo-data" \
    --config "$LOCAL_DIR/app.ini" \
    > "$LOCAL_DIR/logs/forgejo-stdout.log" 2>&1 &
FORGEJO_PID=$!
echo "$FORGEJO_PID" > "$LOCAL_DIR/pids/forgejo.pid"

READY=false
for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:$FORGEJO_PORT/api/v1/version" >/dev/null 2>&1; then
        READY=true
        break
    fi
    sleep 1
done

if ! $READY; then
    die "Forgejo did not restart within 30s. Check $LOCAL_DIR/logs/forgejo-stdout.log"
fi

log "Forgejo restarted (PID: $FORGEJO_PID)"

# ── 8. Write .env ─────────────────────────────────────────────────────────

cat > "$LOCAL_DIR/.env" << ENVEOF
FORGEJO_URL=http://127.0.0.1:$FORGEJO_PORT
FORGEJO_TOKEN=$FORGEJO_TOKEN
FORGEJO_ADMIN_TOKEN=$ADMIN_TOKEN
OLLAMA_API_KEY=
OLLAMA_MODEL=
WAFER_API_KEY=$WAFER_API_KEY
WEBHOOK_SECRET=$WEBHOOK_SECRET
LOG_LEVEL=info
FORDJENT_PORT=$FORDJENT_PORT
GLM_API_KEY=placeholder
ENVEOF

# ── 9. Write Fordjent config ─────────────────────────────────────────────

cat > "$LOCAL_DIR/fordjent.yaml" << YAMLEOF
server:
  host: "0.0.0.0"
  port: $FORDJENT_PORT

webhook:
  secret: "$WEBHOOK_SECRET"

forgejo:
  url: "http://127.0.0.1:$FORGEJO_PORT"
  token: "$FORGEJO_TOKEN"
  admin_token: "$ADMIN_TOKEN"
  rate_limit: 60

agent:
  max_sessions: 25
  idle_timeout: "4h"
  workdir: "$LOCAL_DIR/fordjent-work"
  max_turns: 75
  max_turns_pm: 15
  max_turns_implementer: 50
  role_providers:
    pm: "wafer-glm"
    reviewer: "wafer-glm"
    implementer: "wafer-glm"
  commit_prefix: "[agent-automation]"
  context_window: 131072
  compaction_threshold: 0.85
  compaction_keep_turns: 8
  enable_lifecycle: true
  enable_stale_gate: true
  enable_scaffold_detection: true
  enable_session_recovery: true
  enable_context_injection: true
  enable_auto_collaborator: true
  require_role_tag: true
  session_timeout: "60m"
  git_name: "Fordjent Agent"
  git_email: "fordjent@localhost"

budget:
  enabled: true
  max_session_cost: 2.00
  max_monthly_cost: 50.00

providers:
  - name: "wafer-glm"
    api_base: "https://pass.wafer.ai/v1"
    api_key: "$WAFER_API_KEY"
    model: "glm-5.1"
    max_tokens: 32768
    request_timeout: "90s"
    max_retries: 5
    retry_base_delay: "3s"
    retry_max_delay: "60s"
    max_concurrent_llm_calls: 1
    cost_per_1m_input_tokens: 0
    cost_per_1m_output_tokens: 0

events:
  - "issues"
  - "issue_comment"
  - "pull_request"
  - "pull_request_review_comment"

session_key_template: "{{.Repository}}/issues/{{.IssueNumber}}"

security:
  protected_branches: ["main", "master"]
  require_pr_for_workflows: true
  filter_agent_events: true

memory:
  enabled: true
  compaction_cron: "0 2 * * *"
  compaction_path: "docs/issues"

database:
  path: ""

log_level: "info"
YAMLEOF

# ── 10. Build Fordjent ─────────────────────────────────────────────────────

log "Building Fordjent..."
cd "$PROJECT_DIR"
go build -o "$LOCAL_DIR/fordjent" ./cmd/fordjent 2>&1

if [ ! -x "$LOCAL_DIR/fordjent" ]; then
    die "Fordjent build failed"
fi

log "Fordjent binary: $LOCAL_DIR/fordjent"

# ── 11. Start Fordjent ─────────────────────────────────────────────────────

log "Starting Fordjent on port $FORDJENT_PORT..."

sandbox-exec -f "$LOCAL_DIR/fordjent.sb" \
    "$LOCAL_DIR/fordjent" \
    -config "$LOCAL_DIR/fordjent.yaml" \
    > "$LOCAL_DIR/logs/fordjent-stdout.log" 2>&1 &
FORDJENT_PID=$!
echo "$FORDJENT_PID" > "$LOCAL_DIR/pids/fordjent.pid"

log "Fordjent PID: $FORDJENT_PID, waiting for it to be ready..."

READY=false
for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:$FORDJENT_PORT/healthz" >/dev/null 2>&1; then
        READY=true
        break
    fi
    sleep 1
done

if ! $READY; then
    die "Fordjent did not become ready within 30s. Check $LOCAL_DIR/logs/fordjent-stdout.log"
fi

log "Fordjent is healthy"

# ── 12. Create test repo ──────────────────────────────────────────────────

log "Creating test repo '$TEST_REPO'..."

curl -sf -X POST "http://127.0.0.1:$FORGEJO_PORT/api/v1/user/repos" \
    -H "Authorization: token $FORGEJO_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$TEST_REPO\",\"description\":\"Fordjent integration test repo\",\"private\":false,\"auto_init\":true}" \
    >/dev/null

# Wait for repo to be fully initialized (git clone needs it)
sleep 2

log "Repo $ADMIN_USER/$TEST_REPO created"

# Seed the repo with go.mod + .gitignore so scaffold detection
# doesn't treat it as an empty repo (threshold: 3 files)
log "Seeding repo with go.mod + .gitignore..."

BASE64_GOMOD=$(echo -n 'module testbed\n\ngo 1.26' | base64)
BASE64_GITIGNORE=$(echo -n '*.o\n*.exe\ntestbed' | base64)

curl -sf -X POST "http://127.0.0.1:$FORGEJO_PORT/api/v1/repos/$ADMIN_USER/$TEST_REPO/contents/go.mod" \
    -H "Authorization: token $FORGEJO_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"message\":\"add go.mod\",\"content\":\"$BASE64_GOMOD\"}" >/dev/null 2>&1 || true

curl -sf -X POST "http://127.0.0.1:$FORGEJO_PORT/api/v1/repos/$ADMIN_USER/$TEST_REPO/contents/.gitignore" \
    -H "Authorization: token $FORGEJO_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"message\":\"add .gitignore\",\"content\":\"$BASE64_GITIGNORE\"}" >/dev/null 2>&1 || true

sleep 1
log "Repo seeded"

# ── 13. Create FSM labels ─────────────────────────────────────────────────

log "Creating FSM labels..."

LABELS="planning:0ea5db implementing:fbca04 ready:c2e07c review:fbca04 blocked:b60205 done:28a745 approved:28a745 rejected:b60205 scaffold:1d76db fordjent/failed:max-turns:b60205 fordjent/failed:error:b60205 automerge:28a745"

for label_spec in $LABELS; do
    name="${label_spec%%:*}"
    color="${label_spec##*:}"
    curl -sf -X POST "http://127.0.0.1:$FORGEJO_PORT/api/v1/repos/$ADMIN_USER/$TEST_REPO/labels" \
        -H "Authorization: token $FORGEJO_TOKEN" \
        -H "Content-Type: application/json" \
        -d "{\"name\":\"$name\",\"color\":\"$color\"}" \
        >/dev/null 2>&1 || true
done

log "Labels created"

# ── 14. Register webhook ──────────────────────────────────────────────────

log "Registering webhook..."

curl -sf -X POST "http://127.0.0.1:$FORGEJO_PORT/api/v1/repos/$ADMIN_USER/$TEST_REPO/hooks" \
    -H "Authorization: token $FORGEJO_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{
        \"type\": \"forgejo\",
        \"config\": {
            \"url\": \"http://127.0.0.1:$FORDJENT_PORT/acp/v1/events\",
            \"content_type\": \"json\",
            \"secret\": \"$WEBHOOK_SECRET\"
        },
        \"events\": [\"issues\", \"issue_comment\", \"pull_request\", \"pull_request_review_comment\"],
        \"active\": true
    }" >/dev/null

log "Webhook registered → http://127.0.0.1:$FORDJENT_PORT/acp/v1/events"

# ── 15. Smoke test: create an issue ───────────────────────────────────────

log "Creating test issue..."

ISSUE_RESP=$(curl -sf -X POST "http://127.0.0.1:$FORGEJO_PORT/api/v1/repos/$ADMIN_USER/$TEST_REPO/issues" \
    -H "Authorization: token $FORGEJO_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{
        "title": "[implementer] Write a hello world program",
        "body": "Create a simple Go program that prints hello world. Include a Makefile that builds it. Put everything in the root of the repo."
    }')

ISSUE_NUM=$(echo "$ISSUE_RESP" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("number","?"))' 2>/dev/null || echo "?")

log "Test issue #$ISSUE_NUM created"

# ── 16. Wait for agent activity ───────────────────────────────────────────

log "Waiting for Fordjent to pick up the issue (up to 5min)..."

FOUND=false
for i in $(seq 1 60); do
    STATUS=$(curl -sf "http://127.0.0.1:$FORDJENT_PORT/status" 2>/dev/null || echo '{}')
    ACTIVE=$(echo "$STATUS" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('lifecycle',{}).get('active_sessions',0))" 2>/dev/null || echo "0")

    if [ "$ACTIVE" -gt 0 ] 2>/dev/null; then
        FOUND=true
        break
    fi
    sleep 5
done

if $FOUND; then
    log "Agent is active! Session running."
else
    warn "No active session detected after 5min. Check logs:"
    warn "  Fordjent: $LOCAL_DIR/logs/fordjent-stdout.log"
    warn "  Forgejo: $LOCAL_DIR/logs/forgejo-stdout.log"
fi

# ── Summary ───────────────────────────────────────────────────────────────

echo ""
echo -e "${CYAN}═══ Fordjent Local Deployment Ready ═══${NC}"
echo ""
echo "  Forgejo:    http://127.0.0.1:$FORGEJO_PORT"
echo "  Admin user: $ADMIN_USER / $ADMIN_PASS"
echo "  Test repo:  http://127.0.0.1:$FORGEJO_PORT/$ADMIN_USER/$TEST_REPO"
echo "  Test issue: http://127.0.0.1:$FORGEJO_PORT/$ADMIN_USER/$TEST_REPO/issues/$ISSUE_NUM"
echo ""
echo "  Fordjent:   http://127.0.0.1:$FORDJENT_PORT"
echo "  Status:     http://127.0.0.1:$FORDJENT_PORT/status"
echo "  Metrics:    http://127.0.0.1:$FORDJENT_PORT/metrics"
echo "  Admin UI:   http://127.0.0.1:$FORDJENT_PORT/admin"
echo ""
echo "  PIDs:       Forgejo=$FORGEJO_PID  Fordjent=$FORDJENT_PID"
echo "  Workdir:    $LOCAL_DIR"
echo "  Config:     $LOCAL_DIR/fordjent.yaml"
echo "  Teardown:   $PROJECT_DIR/scripts/teardown-local.sh"
echo ""
echo -e "  ${YELLOW}Logs:${NC}"
echo "    tail -f $LOCAL_DIR/logs/fordjent-stdout.log"
echo "    tail -f $LOCAL_DIR/logs/forgejo-stdout.log"
echo ""
