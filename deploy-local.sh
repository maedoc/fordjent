#!/bin/bash
set -euo pipefail

echo "== Checking prerequisites..."
command -v go >/dev/null || { echo "Go not found. Install: brew install go"; exit 1; }

echo "== Building fordjent..."
mkdir -p bin
go build -o bin/fordjent ./cmd/fordjent
go build -o bin/fj ./cmd/fj

echo "== Checking config..."
CONFIG="${FORDJENT_CONFIG:-fordjent.local.yaml}"
if [ ! -f "$CONFIG" ]; then
    echo "$CONFIG not found. Copy from fordjent.yaml and edit."
    exit 1
fi

if [ -f .env ]; then
    set -a
    source .env
    set +a
fi

WORKDIR="${FORDJENT_WORKDIR:-$HOME/.fordjent/work}"
mkdir -p "$WORKDIR"

CLEAN_FLAG=""
if [[ "${1:-}" == "--clean" ]]; then
    CLEAN_FLAG="--clean"
fi

echo "== Starting fordjent (config: $CONFIG, workdir: $WORKDIR)..."
exec ./bin/fordjent -config "$CONFIG" $CLEAN_FLAG
