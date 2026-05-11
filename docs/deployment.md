# Deployment Guide

## Docker Compose (Recommended)

```bash
cp .env.example .env
# Edit .env with your secrets
docker compose up -d
```

## systemd (Lightweight)

```bash
# Create user
sudo useradd -r -d /var/lib/fordjent -s /bin/false fordjent

# Install binary
sudo cp fordjent /usr/local/bin/
sudo chmod +x /usr/local/bin/fordjent

# Create directories
sudo mkdir -p /var/lib/fordjent/work /etc/fordjent
sudo chown -R fordjent:fordjent /var/lib/fordjent

# Install config and service
sudo cp fordjent.yaml /etc/fordjent/
sudo cp scripts/fordjent.service /etc/systemd/system/

# Start
sudo systemctl daemon-reload
sudo systemctl enable --now fordjent
sudo journalctl -u fordjent -f
```

## Backup

The only persistent state is `/var/lib/fordjent/work` (clones + JSONL memory).

```bash
sudo tar czf /backups/fordjent-$(date +%F).tar.gz /var/lib/fordjent/work
```

## Required Forgejo Labels

The scheduler and lifecycle systems expect these labels to exist in your Forgejo repository:

| Label | Purpose |
|-------|---------|
| `blocked` | Dependency not yet merged |
| `ready` | All dependencies merged, ready for agent |
| `scaffold` | Project scaffold issue |
| `fordjent/failed:max-turns` | Session hit turn limit |
| `fordjent/failed:error` | Session hit unrecoverable error |

Create them via **Repository → Settings → Labels → New Label** in the Forgejo UI, or use the API:

```bash
for label in blocked ready scaffold "fordjent/failed:max-turns" "fordjent/failed:error"; do
  curl -X POST http://localhost:3000/api/v1/repos/owner/repo/labels \
    -u fjadmin:password \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$label\",\"color\":\"#cccccc\"}"
done
```

Fordjent can also auto-create these labels on first use if `agent.enable_scaffold_detection: true` is set.

## Monitoring

- `/healthz` — Liveness probe, returns HTTP 200 when the process is running
- `/readyz` — Readiness probe, returns HTTP 200 when the instance is ready to accept events
- `/metrics` — Prometheus text format

### Recovering from session corruption

If SQLite reports database errors or sessions are stuck in a bad state, restart with:

```bash
./fordjent -config my-config.yaml -clean
```

This wipes all persisted sessions and restarts fresh. Use only as a last resort — session history and audit data will be lost.
