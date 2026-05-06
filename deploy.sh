#!/bin/bash
set -euo pipefail

# Fordjent one-command deployment script
# Usage: ./deploy.sh [--clean]

CLEAN=false
if [[ "${1:-}" == "--clean" ]]; then
    CLEAN=true
    echo "== Clean mode: wiping session DB on start"
fi

echo "== Building Fordjent..."
docker build -t fordjent:local . 2>&1 | tail -3

echo "== Stopping old container..."
docker stop fordjent 2>/dev/null || true
docker rm fordjent 2>/dev/null || true

echo "== Starting new container..."
EXTRA_ARGS=""
if $CLEAN; then
    EXTRA_ARGS="--clean"
fi

docker run -d --name fordjent --network fordjent-net \
  -p 127.0.0.1:8080:8080 \
  -p 10.67.121.201:8081:8080 \
  -v fordjent-data:/var/lib/fordjent \
  -v $(pwd)/fordjent.local.yaml:/etc/fordjent/fordjent.yaml:ro \
  --env-file $(pwd)/.env \
  fordjent:local $EXTRA_ARGS

echo "== Waiting for health check..."
sleep 5
if curl -sf http://127.0.0.1:8080/healthz > /dev/null 2>&1; then
    echo "== ✅ Fordjent is healthy"
else
    echo "== ❌ Health check failed!"
    docker logs fordjent 2>&1 | tail -20
    exit 1
fi

echo "== Done. Logs: docker logs -f fordjent"
