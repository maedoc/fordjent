import io
import logging
import time
import base64

import paramiko
import httpx
from rich.console import Console

from .config import Config

logger = logging.getLogger(__name__)
console = Console()


FORDJENT_REPO = "https://codeberg.org/forgejo/forgejo.git"  # placeholder, overridden by cli


def generate_cloud_init_script(config: Config, fordjent_repo: str = "") -> str:
    """Generate a cloud-init script that installs Docker + Docker Compose on the instance."""
    # If fordjent_repo is empty, skip git clone (we'll rsync)
    clone_cmd = f"git clone {fordjent_repo} /opt/fordjent-src 2>/dev/null || true" if fordjent_repo and fordjent_repo != "local-rsync" else "echo 'Source will be rsynced later'"
    return f"""#!/bin/bash
set -e

# Update and install Docker
apt-get update
apt-get install -y ca-certificates curl git gnupg lsb-release

# Add Docker GPG key and repo
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg

echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" | tee /etc/apt/sources.list.d/docker.list > /dev/null

apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

# Enable Docker
systemctl enable docker
systemctl start docker

# Install bwrap for Fordjent sandbox
apt-get install -y bubblewrap

# Clone or prepare Fordjent source directory
{clone_cmd}

# Harden SSH: disable password auth
sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
systemctl reload sshd || true

# Install UFW and configure firewall
apt-get install -y ufw
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp    # SSH
ufw allow 80/tcp    # HTTP (for Caddy TLS challenge)
ufw allow 443/tcp   # HTTPS
ufw allow 443/udp   # HTTPS (HTTP/3)
ufw --force enable

# Create deploy directory
mkdir -p /opt/fordjent-deploy

# Marker for completion
touch /opt/cloud-init.done
echo "Cloud init complete at $(date)"
"""


def generate_docker_env(config: Config) -> str:
    """Generate the .env file for docker-compose."""
    return f"""FORGEJO_DOMAIN={config.forgejo_domain}
FORGEJO_TOKEN={config._forgejo_token}
FORGEJO_ADMIN_TOKEN={config._forgejo_admin_token}
FORGEJO_SECRET_KEY={config.forgejo_secret_key}
FORGEJO_INTERNAL_TOKEN={config.forgejo_internal_token}
WAFER_API_KEY={config.wafer_api_key}
SCALEWAY_AI_KEY={config.scaleway_ai_key}
WEBHOOK_SECRET={config.webhook_secret}
LOG_LEVEL=info
FORDJENT_GIT_NAME=Fordjent Agent
FORDJENT_GIT_EMAIL=fordjent@wdmn.fr
"""


def generate_setup_script(config: Config) -> str:
    """Generate the full setup script to run on the instance after SSH is available."""
    return f"""#!/bin/bash
set -e

echo "=== Starting Fordjent Cloud Setup ==="

# Wait for cloud-init to finish
while [ ! -f /opt/cloud-init.done ]; do
    echo "Waiting for cloud-init..."
    sleep 5
done
echo "Cloud-init complete."

cd /opt/fordjent-deploy

# Write .env file
cat > .env << 'ENVEOF'
{generate_docker_env(config)}
ENVEOF

# Write Forgejo app.ini (with template substitution)
cat > forgejo.app.ini << 'INIEOF'
APP_NAME = Forgejo
RUN_MODE = prod

[server]
DOMAIN = {config.forgejo_domain}
ROOT_URL = https://{config.forgejo_domain}/
HTTP_PORT = 3000
SSH_DOMAIN = {config.forgejo_domain}
SSH_PORT = 22
DISABLE_SSH = false
LFS_START_SERVER = true
LANDING_PAGE = explore

[database]
DB_TYPE = sqlite3
PATH = /data/gitea/gitea.db

[service]
DISABLE_REGISTRATION = true
REQUIRE_SIGNIN_VIEW = false
DEFAULT_ALLOW_CREATE_ORGANIZATION = false
SHOW_REGISTRATION_BUTTON = false

[webhook]
ALLOWED_HOST_LIST = *
QUEUE_LENGTH = 1000
DELIVER_TIMEOUT = 30

[repository]
DEFAULT_PRIVATE = public
DEFAULT_BRANCH = main

[security]
INSTALL_LOCK = true
INTERNAL_TOKEN = {config.forgejo_internal_token}
SECRET_KEY = {config.forgejo_secret_key}

[log]
MODE = file
LEVEL = Info
ROOT_PATH = /data/gitea/log

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

[ui]
DEFAULT_THEME = forgejo-dark
INIEOF

# Build and start Docker Compose
docker compose up -d --build 2>&1 | tail -20

echo "Waiting for Forgejo to be ready..."
for i in $(seq 1 60); do
    if curl -sf http://localhost:3000/api/v1/version >/dev/null 2>&1; then
        echo "Forgejo is ready!"
        break
    fi
    sleep 2
done

echo "Setup complete at $(date)"
touch /opt/fordjent-setup.done
"""


def wait_for_ssh(
    ip_address: str,
    ssh_key_path: str,
    timeout: int = 300,
    interval: int = 5,
) -> paramiko.SSHClient:
    logger.info("Attempting SSH to root@%s (key=%s), timeout=%ds", ip_address, ssh_key_path, timeout)
    deadline = time.time() + timeout
    while time.time() < deadline:
        client = paramiko.SSHClient()
        client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        try:
            client.connect(
                hostname=ip_address,
                username="root",
                key_filename=ssh_key_path,
                timeout=10,
                banner_timeout=10,
                auth_timeout=10,
            )
            console.print(f"[green]SSH connected to {ip_address}[/green]")
            logger.info("SSH connected to %s", ip_address)
            return client
        except (paramiko.SSHException, OSError):
            client.close()
            time.sleep(interval)
    raise TimeoutError(f"Could not establish SSH to {ip_address} within {timeout}s")


def upload_scripts(
    ssh_client: paramiko.SSHClient,
    config: Config,
    project_dir: str,
) -> None:
    """Upload docker-compose, Caddyfile, and fordjent config to the instance."""
    logger.info("Uploading deployment files...")

    # Create directories
    sftp = ssh_client.open_sftp()
    ssh_exec(ssh_client, "mkdir -p /opt/fordjent-deploy")

    # Upload docker-compose.yaml
    compose_path = f"{project_dir}/cloud/docker-compose.yaml"
    try:
        sftp.put(compose_path, "/opt/fordjent-deploy/docker-compose.yaml")
    except FileNotFoundError:
        # If running from source, the path might be different
        logger.warning("docker-compose.yaml not found at %s, will generate inline", compose_path)

    # Upload Caddyfile
    caddy_path = f"{project_dir}/cloud/Caddyfile"
    try:
        sftp.put(caddy_path, "/opt/fordjent-deploy/Caddyfile")
    except FileNotFoundError:
        logger.warning("Caddyfile not found at %s", caddy_path)

    # Upload fordjent.yaml template
    fordjent_yaml_path = f"{project_dir}/cloud/fordjent.yaml"
    try:
        sftp.put(fordjent_yaml_path, "/opt/fordjent-deploy/fordjent.yaml")
    except FileNotFoundError:
        logger.warning("fordjent.yaml not found at %s", fordjent_yaml_path)

    sftp.close()
    logger.info("Deployment files uploaded")


def ssh_exec(ssh_client: paramiko.SSHClient, cmd: str, timeout: int = 300) -> tuple[str, str, int]:
    """Execute a command via SSH and return stdout, stderr, exit_code."""
    stdin, stdout, stderr = ssh_client.exec_command(cmd, timeout=timeout)
    exit_code = stdout.channel.recv_exit_status()
    out = stdout.read().decode()
    err = stderr.read().decode()
    return out, err, exit_code


def wait_for_setup(ssh_client: paramiko.SSHClient, timeout: int = 600, interval: int = 10) -> bool:
    logger.info("Waiting for setup script to complete (timeout=%ds)...", timeout)
    deadline = time.time() + timeout
    while time.time() < deadline:
        out, err, code = ssh_exec(ssh_client, "test -f /opt/fordjent-setup.done && echo done || echo running", timeout=10)
        output = out.strip()
        if output == "done":
            console.print("[green]Setup completed successfully[/green]")
            return True
        # Show logs for progress
        out, _, _ = ssh_exec(ssh_client, "tail -3 /var/log/fordjent-setup.log 2>/dev/null || echo 'no log yet'", timeout=5)
        if out.strip() and out.strip() != "no log yet":
            logger.debug("Setup progress: %s", out.strip())
        time.sleep(interval)
    raise TimeoutError(f"Setup did not complete within {timeout}s")


def wait_for_forgejo(ssh_client: paramiko.SSHClient, timeout: int = 120, interval: int = 5) -> bool:
    logger.info("Waiting for Forgejo container to be healthy...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        out, _, code = ssh_exec(ssh_client, "curl -sf http://localhost:3000/api/v1/version", timeout=5)
        if code == 0:
            console.print("[green]Forgejo is healthy[/green]")
            return True
        time.sleep(interval)
    raise RuntimeError(f"Forgejo not healthy within {timeout}s")


def wait_for_fordjent(ssh_client: paramiko.SSHClient, timeout: int = 120, interval: int = 5) -> bool:
    logger.info("Waiting for Fordjent container to be healthy...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        out, _, code = ssh_exec(ssh_client, "curl -sf http://localhost:8080/healthz", timeout=5)
        if code == 0:
            console.print("[green]Fordjent is healthy[/green]")
            return True
        time.sleep(interval)
    raise RuntimeError(f"Fordjent not healthy within {timeout}s")


def check_caddy_tls(ssh_client: paramiko.SSHClient, forgejo_domain: str, timeout: int = 180, interval: int = 10) -> bool:
    """Check if Caddy has provisioned TLS certs."""
    logger.info("Waiting for Caddy to provision TLS certs (this may take a few minutes)...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        out, _, code = ssh_exec(
            ssh_client,
            f"curl -sf -o /dev/null -w '%{{http_code}}' https://{forgejo_domain}/api/v1/version",
            timeout=10,
        )
        if code == 0 and out.strip() in ("200", "301", "302", "401"):
            console.print("[green]TLS certs provisioned and Forgejo accessible via HTTPS[/green]")
            return True
        time.sleep(interval)
    console.print("[yellow]TLS provisioning timeout — Caddy may still be obtaining certs. Check manually.[/yellow]")
    return False