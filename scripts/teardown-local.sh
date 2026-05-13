#!/usr/bin/env bash
set -euo pipefail

LOCAL_DIR="${FORDJENT_LOCAL_DIR:-$HOME/fordjent-local}"

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${GREEN}[teardown]${NC} $*"; }
warn() { echo -e "${RED}[teardown]${NC} $*"; }

# Kill Forgejo
if [ -f "$LOCAL_DIR/pids/forgejo.pid" ]; then
    PID=$(cat "$LOCAL_DIR/pids/forgejo.pid")
    if kill -0 "$PID" 2>/dev/null; then
        log "Stopping Forgejo (PID $PID)..."
        kill "$PID" 2>/dev/null || true
        sleep 1
        kill -9 "$PID" 2>/dev/null || true
    else
        warn "Forgejo PID $PID is not running"
    fi
    rm -f "$LOCAL_DIR/pids/forgejo.pid"
fi

# Kill Fordjent
if [ -f "$LOCAL_DIR/pids/fordjent.pid" ]; then
    PID=$(cat "$LOCAL_DIR/pids/fordjent.pid")
    if kill -0 "$PID" 2>/dev/null; then
        log "Stopping Fordjent (PID $PID)..."
        kill "$PID" 2>/dev/null || true
        sleep 1
        kill -9 "$PID" 2>/dev/null || true
    else
        warn "Fordjent PID $PID is not running"
    fi
    rm -f "$LOCAL_DIR/pids/fordjent.pid"
fi

# Check if anything is still listening on our ports
for port in 3000 8080; do
    if lsof -i ":$port" -sTCP:LISTEN >/dev/null 2>&1; then
        PID=$(lsof -ti ":$port" 2>/dev/null | head -1)
        warn "Port $port still in use by PID $PID, killing..."
        kill "$PID" 2>/dev/null || true
    fi
done

# Optionally clean data
if [[ "${1:-}" == "--clean" ]]; then
    log "Removing $LOCAL_DIR..."
    rm -rf "$LOCAL_DIR"
    log "Clean slate."
else
    log "Data preserved in $LOCAL_DIR (use --clean to wipe)"
fi

echo -e "${CYAN}Done.${NC}"
