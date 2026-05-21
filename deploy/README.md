# Fordjent Cloud Deployment

Deploy Forgejo + Fordjent to Scaleway with automatic TLS via Caddy.

## Architecture

```
Internet → Caddy (TLS termination)
              ├── forgejo.wdmn.fr → Forgejo :3000
              └── fordjent.wdmn.fr → Fordjent :8080
```

## Prerequisites

- Python 3.12+ with `uv`
- SSH key at `~/.ssh/id_ed25519`
- Scaleway account with project ID and API keys
- **Gandi API key** (for automatic DNS setup) — get one at https://api.gandi.net/
- DNS: `forgejo.wdmn.fr` and `fordjent.wdmn.fr` (handled automatically with Gandi API key)

## Quick Start

```bash
# 1. Set environment variables (fill in GANDI_API_KEY!)
cp deploy/env.local.sh deploy/env.local.sh  # if editing fresh
$EDITOR deploy/env.local.sh                # add your Gandi API key
source deploy/env.local.sh

# 2. Install the deploy tool
cd deploy && uv sync && cd ..

# 3. Deploy (creates instance, sets DNS, provisions Docker stack, configures TLS)
uv run --directory deploy fordjent-deploy up

# 4. Check status
uv run --directory deploy fordjent-deploy status

# 5. Tear down when done
uv run --directory deploy fordjent-deploy down
```

## Instance Specs

| Setting | Value |
|---------|-------|
| Instance | DEV1-L (4 vCPU, 8GB RAM) |
| Cost | ~€31/mo |
| Zone | fr-par-2 (Paris) |
| Image | Ubuntu 22.04 (SBS) |
| LLM | Scaleway Qwen3.6-35b-a3b (free tier) |

## Files

| File | Purpose |
|------|---------|
| `cloud/docker-compose.yaml` | 3-service stack: Caddy + Forgejo + Fordjent |
| `cloud/Caddyfile` | SNI-based routing, auto-TLS |
| `cloud/forgejo.app.ini` | Forgejo configuration template |
| `cloud/fordjent.yaml` | Fordjent configuration template |
| `env.sh` | Scaleway API keys |
| `src/fordjent_deploy/` | Python deployment tool |

## Security

- SSH: key-only auth (password auth disabled)
- UFW firewall: only 22/80/443
- TLS: automatic via Caddy + Let's Encrypt
- Forgejo: registration disabled, admin-only
- Fordjent: webhook HMAC verification
- Scaleway AI: API key scoped to project

## DNS Setup

With `GANDI_API_KEY` set, DNS records are created **automatically** during `up` and removed during `down`.

Without a Gandi key, you'll be prompted to set up DNS manually:

```
forgejo.wdmn.fr   →  A  →  <INSTANCE_IP>
fordjent.wdmn.fr  →  A  →  <INSTANCE_IP>
```

Caddy will automatically provision TLS certificates once DNS resolves (1-5 minutes).

## Troubleshooting

```bash
# SSH into the instance
ssh -i ~/.ssh/id_ed25519 root@<IP>

# Check container status
docker compose -f /opt/fordjent-deploy/docker-compose.yaml ps

# View Forgejo logs
docker logs forgejo

# View Fordjent logs
docker logs fordjent

# View Caddy logs
docker logs caddy

# Force TLS cert reprovision
docker exec caddy caddy reload --config /etc/caddy/Caddyfile
```