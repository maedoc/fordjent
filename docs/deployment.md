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

## Monitoring

- `/healthz` — HTTP 200 if alive
- `/readyz` — HTTP 200 if ready
- `/metrics` — Prometheus text format
