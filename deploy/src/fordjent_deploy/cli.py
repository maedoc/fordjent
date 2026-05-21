import argparse
import json
import logging
import os
import signal
import subprocess
import sys
import time

import httpx
from rich.console import Console
from rich.panel import Panel

from .config import Config, STATE_FILE, STATE_DIR, DOMAIN_FORGEJO, DOMAIN_FORDJENT, generate_password, generate_token
from .scaleway_api import ScalewayAPI
from .provision import (
    generate_cloud_init_script,
    generate_setup_script,
    wait_for_ssh,
    upload_scripts,
    ssh_exec,
    wait_for_forgejo,
    wait_for_fordjent,
    check_caddy_tls,
)

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    datefmt="%H:%M:%S",
)
logger = logging.getLogger("fordjent-deploy")
console = Console()


def cmd_up(args):
    config = Config(
        instance_type=args.type,
    )
    if args.zone:
        config.zone = args.zone
    if args.admin_pass:
        config.admin_pass = args.admin_pass

    try:
        config.validate()
    except (EnvironmentError, ValueError, FileNotFoundError) as exc:
        console.print(f"[red]Config error: {exc}[/red]")
        sys.exit(1)

    # Resolve Scaleway AI key from models.json if not set
    if not config.scaleway_ai_key:
        pi_models = os.path.expanduser("~/.pi/agent/models.json")
        if os.path.isfile(pi_models):
            try:
                data = json.loads(open(pi_models).read())
                config.scaleway_ai_key = data.get("providers", {}).get("scaleway", {}).get("apiKey", "")
            except (json.JSONDecodeError, ValueError):
                pass
        if not config.scaleway_ai_key:
            console.print("[yellow]Warning: SCALEWAY_AI_KEY not set. Fordjent will not be able to call LLM APIs.[/yellow]")

    logger.info("Instance type: %s in %s", config.instance_type, config.zone)

    api = ScalewayAPI(config)

    existing_state = config.load_state()
    if existing_state:
        console.print("[red]An instance is already active. Run [bold]fordjent-deploy down[/bold] first.[/red]")
        sys.exit(1)

    def _cleanup_on_interrupt(signum, frame):
        console.print("\n[yellow]Interrupted! Run [bold]fordjent-deploy down[/bold] to clean up.[/yellow]")
        sys.exit(130)

    signal.signal(signal.SIGINT, _cleanup_on_interrupt)

    # Step 1: Register SSH key
    logger.info("Verifying SSH key is registered in Scaleway...")
    with console.status("[bold]Verifying SSH key...[/bold]"):
        ssh_pub_key = config._read_ssh_public_key()
        api.ensure_ssh_key(ssh_pub_key)

    # Step 2: Resolve OS image
    logger.info("Resolving OS image (%s)...", config.image_label)
    with console.status("[bold]Resolving OS image...[/bold]"):
        image_uuid = api.resolve_image(config.image_label)
    logger.info("Image UUID: %s", image_uuid)

    # Step 3: Create flexible IP
    logger.info("Creating flexible IP...")
    with console.status("[bold]Creating flexible IP...[/bold]"):
        ip_id, ip_address = api.create_ip()
    logger.info("IP: %s (%s)", ip_address, ip_id)

    # Print IP immediately
    console.print(Panel(
        f"[bold green]Public IP assigned: {ip_address}[/bold green]",
        title="[bold]IP Address[/bold]",
        border_style="green",
    ))

    # Step 3b: Set up DNS via Gandi (if API key provided)
    subdomains = ["forgejo", "fordjent"]
    if config.gandi_api_key:
        logger.info("Setting up DNS records via Gandi API...")
        with console.status("[bold]Creating DNS A records via Gandi...[/bold]"):
            from .gandi_dns import GandiDNS
            dns = GandiDNS(api_key=config.gandi_api_key, domain=config.gandi_domain)
            dns.set_a_records(subdomains, ip_address, ttl=300)
        console.print(f"[green]DNS records created: forgejo.{config.gandi_domain} + fordjent.{config.gandi_domain} → {ip_address}[/green]")
    else:
        console.print(Panel(
            f"[bold yellow]No Gandi API key — set up DNS manually:[/bold yellow]\n\n"
            f"  forgejo.wdmn.fr   →  A  →  {ip_address}\n"
            f"  fordjent.wdmn.fr  →  A  →  {ip_address}\n\n"
            f"Set GANDI_API_KEY in env.local.sh to automate this.",
            title="[bold yellow]Manual DNS Required[/bold yellow]",
            border_style="yellow",
        ))

    # Store tokens on config before saving state
    config._forgejo_token = ""
    config._forgejo_admin_token = ""

    state = {
        "server_id": None,
        "ip_id": ip_id,
        "ip_address": ip_address,
        "instance_type": config.instance_type,
        "zone": config.zone,
        "admin_user": config.admin_user,
        "admin_pass": config.admin_pass,
        "webhook_secret": config.webhook_secret,
        "forgejo_domain": config.forgejo_domain,
        "fordjent_domain": config.fordjent_domain,
    }
    config.save_state(state)

    # Step 4: Create instance with cloud-init
    logger.info("Creating instance...")
    with console.status("[bold]Creating %s instance in %s...[/bold]" % (config.instance_type, config.zone)):
        server = api.create_server(image_uuid, ip_id, name="fordjent-cloud")
        server_id = server["id"]
    logger.info("Server ID: %s", server_id)

    state["server_id"] = server_id
    config.save_state(state)

    # Step 5: Wait for server running
    logger.info("Waiting for instance to boot...")
    with console.status("[bold]Waiting for instance to boot...[/bold]"):
        server = api.wait_server_running(server_id)
        ip_address = api.get_server_ip(server_id)
    logger.info("Instance running at %s", ip_address)

    state["ip_address"] = ip_address
    config.save_state(state)

    # Step 6: Inject cloud-init user data (for Docker install)
    # Scaleway SBS images don't support cloud-init well, so we use SSH instead

    # Step 7: Wait for SSH
    logger.info("Waiting for SSH access...")
    with console.status("[bold]Connecting via SSH...[/bold]"):
        ssh_client = wait_for_ssh(ip_address, config.ssh_key_path)

    # Step 8: Install Docker and prerequisites
    # Determine Fordjent source: try to rsync local source, or use git clone
    fordjent_src_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
    has_local_src = os.path.isfile(os.path.join(fordjent_src_dir, "cmd", "fordjent", "main.go"))

    logger.info("Installing Docker and hardening SSH...")
    with console.status("[bold]Installing Docker + firewall (may take 2-3 min)...[/bold]"):
        cloud_init = generate_cloud_init_script(config, fordjent_repo="local-rsync" if has_local_src else "")
        sftp = ssh_client.open_sftp()
        with sftp.file("/opt/cloud-init.sh", "w") as f:
            f.write(cloud_init)
        sftp.close()

        ssh_exec(ssh_client, "chmod +x /opt/cloud-init.sh")
        out, err, code = ssh_exec(ssh_client, "bash /opt/cloud-init.sh", timeout=300)
        if code != 0:
            logger.error("Cloud init failed: %s", err)
            console.print(f"[red]Cloud init failed. Check /opt/cloud-init.sh[/red]")
            console.print(f"stderr: {err[:500]}")
            sys.exit(1)
        console.print("[green]Docker installed, SSH hardened, UFW configured[/green]")

    # Step 8b: Copy Fordjent source to instance
    if has_local_src:
        logger.info("Rsync-ing Fordjent source to instance...")
        with console.status("[bold]Copying Fordjent source...[/bold]"):
            # Use rsync over SSH to copy the entire source tree
            rsync_cmd = [
                "rsync", "-az", "--delete",
                "--exclude", ".git",
                "--exclude", ".venv",
                "--exclude", "node_modules",
                "--exclude", "deploy/.venv",
                "-e", f"ssh -i {config.ssh_key_path} -o StrictHostKeyChecking=no",
                f"{fordjent_src_dir}/",
                f"root@{ip_address}:/opt/fordjent-src/",
            ]
            result = subprocess.run(rsync_cmd, capture_output=True, text=True, timeout=300)
            if result.returncode != 0:
                # Fall back to scp if rsync fails
                logger.warning("rsync failed (%s), trying scp...", result.stderr[:200])
                console.print("[yellow]rsync failed, using scp fallback[/yellow]")
                # Create a tar archive and copy it
                import tempfile
                with tempfile.NamedTemporaryFile(suffix=".tar.gz", delete=False) as tmp:
                    tar_cmd = ["tar", "czf", tmp.name, "-C", fordjent_src_dir,
                               "--exclude", ".git",
                               "--exclude", ".venv",
                               "--exclude", "deploy/.venv",
                               "."]
                    subprocess.run(tar_cmd, check=True)
                    # Copy tar to instance
                    scp_cmd = ["scp", "-i", config.ssh_key_path, "-o", "StrictHostKeyChecking=no",
                               tmp.name, f"root@{ip_address}:/opt/fordjent-src.tar.gz"]
                    subprocess.run(scp_cmd, check=True)
                    # Extract on instance
                    ssh_exec(ssh_client, "mkdir -p /opt/fordjent-src && tar xzf /opt/fordjent-src.tar.gz -C /opt/fordjent-src")
                    os.unlink(tmp.name)
        console.print("[green]Fordjent source copied[/green]")
    else:
        logger.info("No local source found, cloning from GitHub...")
        ssh_exec(ssh_client, "git clone https://github.com/your-org/fordjent.git /opt/fordjent-src 2>/dev/null || true", timeout=120)

    # Step 9: Find project dir for config files
    # Try to find the fordjent source repo (for building the Docker image)
    project_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..", ".."))
    # Check if we're in the fordjent repo
    if not os.path.isfile(os.path.join(project_dir, "cmd", "fordjent", "main.go")):
        # Fall back to current directory
        project_dir = os.getcwd()

    # Step 10: Upload deployment files
    # Determine the deploy directory
    deploy_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "deploy")
    if not os.path.isdir(os.path.join(deploy_dir, "cloud")):
        deploy_dir = os.path.join(os.getcwd(), "deploy")

    logger.info("Uploading deployment files from %s...", deploy_dir)
    with console.status("[bold]Uploading deployment files...[/bold]"):
        sftp = ssh_client.open_sftp()

        # Create directories
        ssh_exec(ssh_client, "mkdir -p /opt/fordjent-deploy")

        # Upload docker-compose.yaml
        local_compose = os.path.join(deploy_dir, "cloud", "docker-compose.yaml")
        if os.path.isfile(local_compose):
            sftp.put(local_compose, "/opt/fordjent-deploy/docker-compose.yaml")
        else:
            console.print(f"[red]docker-compose.yaml not found at {local_compose}[/red]")
            sys.exit(1)

        # Upload Caddyfile
        local_caddy = os.path.join(deploy_dir, "cloud", "Caddyfile")
        if os.path.isfile(local_caddy):
            sftp.put(local_caddy, "/opt/fordjent-deploy/Caddyfile")
        else:
            console.print(f"[red]Caddyfile not found at {local_caddy}[/red]")
            sys.exit(1)

        # Generate and upload .env
        config._forgejo_token = "PLACEHOLDER"  # Will be replaced after Forgejo init
        config._forgejo_admin_token = "PLACEHOLDER"
        env_content = (
            f"FORGEJO_DOMAIN={config.forgejo_domain}\n"
            f"FORGEJO_TOKEN=PLACEHOLDER\n"
            f"FORGEJO_ADMIN_TOKEN=PLACEHOLDER\n"
            f"FORGEJO_SECRET_KEY={config.forgejo_secret_key}\n"
            f"FORGEJO_INTERNAL_TOKEN={config.forgejo_internal_token}\n"
            f"WAFER_API_KEY={config.wafer_api_key}\n"
            f"SCALEWAY_AI_KEY={config.scaleway_ai_key}\n"
            f"WEBHOOK_SECRET={config.webhook_secret}\n"
            f"LOG_LEVEL=info\n"
            f"FORDJENT_GIT_NAME=Fordjent Agent\n"
            f"FORDJENT_GIT_EMAIL=fordjent@wdmn.fr\n"
        )
        with sftp.file("/opt/fordjent-deploy/.env", "w") as f:
            f.write(env_content)

        # Upload forgejo.app.ini
        local_app_ini = os.path.join(deploy_dir, "cloud", "forgejo.app.ini")
        if os.path.isfile(local_app_ini):
            # Read and template-substitute
            app_ini_content = open(local_app_ini).read()
            app_ini_content = app_ini_content.replace("{{FORGEJO_DOMAIN}}", config.forgejo_domain)
            app_ini_content = app_ini_content.replace("{{FORGEJO_SECRET_KEY}}", config.forgejo_secret_key)
            app_ini_content = app_ini_content.replace("{{FORGEJO_INTERNAL_TOKEN}}", config.forgejo_internal_token)
            with sftp.file("/opt/fordjent-deploy/forgejo.app.ini", "w") as f:
                f.write(app_ini_content)

        # Upload fordjent.yaml
        local_fordjent_yaml = os.path.join(deploy_dir, "cloud", "fordjent.yaml")
        if os.path.isfile(local_fordjent_yaml):
            yaml_content = open(local_fordjent_yaml).read()
            yaml_content = yaml_content.replace("{{WEBHOOK_SECRET}}", config.webhook_secret)
            yaml_content = yaml_content.replace("{{FORGEJO_TOKEN}}", "PLACEHOLDER")
            yaml_content = yaml_content.replace("{{FORGEJO_ADMIN_TOKEN}}", "PLACEHOLDER")
            yaml_content = yaml_content.replace("{{SCALEWAY_AI_KEY}}", config.scaleway_ai_key)
            yaml_content = yaml_content.replace("{{ADMIN_USER}}", config.admin_user)
            yaml_content = yaml_content.replace("{{TEST_REPO}}", config.test_repo)
            yaml_content = yaml_content.replace("{{LOG_LEVEL}}", "info")
            with sftp.file("/opt/fordjent-deploy/fordjent.yaml", "w") as f:
                f.write(yaml_content)

        sftp.close()
        console.print("[green]Deployment files uploaded[/green]")

    # Step 10b: Wait for DNS propagation (only if Gandi was used)
    if config.gandi_api_key:
        console.print("[cyan]Waiting 30s for DNS propagation...[/cyan]")
        try:
            time.sleep(30)
        except KeyboardInterrupt:
            console.print("[yellow]Skipping DNS wait. Caddy will retry cert provisioning.[/yellow]")
    else:
        console.print(Panel(
            f"[bold yellow]DNS must be configured before Caddy can get TLS certs.[/bold yellow]\n\n"
            f"Press Enter once DNS is set up, or Ctrl+C to skip.\n\n"
            f"  forgejo.wdmn.fr   →  A  →  {ip_address}\n"
            f"  fordjent.wdmn.fr  →  A  →  {ip_address}",
            title="[bold yellow]⏸ DNS Check[/bold yellow]",
            border_style="yellow",
        ))
        try:
            input("Press Enter to continue, or Ctrl+C to skip...")
        except KeyboardInterrupt:
            console.print("[yellow]Skipping DNS check. Caddy will retry cert provisioning.[/yellow]")

    # Step 11: Start Docker Compose (Forgejo only first)
    logger.info("Starting Forgejo container...")
    with console.status("[bold]Starting Forgejo...[/bold]"):
        out, err, code = ssh_exec(ssh_client, "cd /opt/fordjent-deploy && docker compose up -d forgejo caddy 2>&1", timeout=120)
        if code != 0:
            logger.error("Docker compose up failed: %s", err)
            console.print(f"[red]Docker compose failed. stderr: {err[:500]}[/red]")
            sys.exit(1)

    # Step 12: Wait for Forgejo
    logger.info("Waiting for Forgejo to be ready...")
    with console.status("[bold]Waiting for Forgejo...[/bold]"):
        wait_for_forgejo(ssh_client, timeout=180)

    # Step 13: Create Forgejo admin user + tokens
    logger.info("Creating Forgejo admin user and tokens...")
    with console.status("[bold]Setting up Forgejo admin...[/bold]"):
        # Create admin user via docker exec
        out, err, code = ssh_exec(
            ssh_client,
            f"docker exec forgejo gitea admin user create "
            f"--admin --username {config.admin_user} --password '{config.admin_pass}' "
            f"--email admin@wdmn.fr --must-change-password=false 2>&1 || "
            f"docker exec forgejo gitea admin user change-password "
            f"--username {config.admin_user} --password '{config.admin_pass}' 2>&1",
            timeout=30,
        )
        logger.info("Admin user creation: %s", out.strip())

        # Generate tokens
        out, err, code = ssh_exec(
            ssh_client,
            f"docker exec forgejo gitea admin user generate-access-token "
            f"--username {config.admin_user} --token-name fordjent-bot --scopes all --raw 2>&1",
            timeout=30,
        )
        forgejo_token = out.strip().split("\n")[-1].strip()

        out2, err2, code2 = ssh_exec(
            ssh_client,
            f"docker exec forgejo gitea admin user generate-access-token "
            f"--username {config.admin_user} --token-name fordjent-admin --scopes all --raw 2>&1",
            timeout=30,
        )
        forgejo_admin_token = out2.strip().split("\n")[-1].strip()

        if not forgejo_token or not forgejo_admin_token:
            console.print(f"[red]Failed to generate tokens. Output: {out} / {out2}[/red]")
            sys.exit(1)

        config._forgejo_token = forgejo_token
        config._forgejo_admin_token = forgejo_admin_token
        logger.info("Bot token: %s...", forgejo_token[:8])
        logger.info("Admin token: %s...", forgejo_admin_token[:8])

    # Step 14: Update .env and fordjent.yaml with real tokens
    logger.info("Updating config with real tokens...")
    with console.status("[bold]Updating config...[/bold]"):
        sftp = ssh_client.open_sftp()

        # Rewrite .env
        env_content = (
            f"FORGEJO_DOMAIN={config.forgejo_domain}\n"
            f"FORGEJO_TOKEN={forgejo_token}\n"
            f"FORGEJO_ADMIN_TOKEN={forgejo_admin_token}\n"
            f"FORGEJO_SECRET_KEY={config.forgejo_secret_key}\n"
            f"FORGEJO_INTERNAL_TOKEN={config.forgejo_internal_token}\n"
            f"WAFER_API_KEY={config.wafer_api_key}\n"
            f"SCALEWAY_AI_KEY={config.scaleway_ai_key}\n"
            f"WEBHOOK_SECRET={config.webhook_secret}\n"
            f"LOG_LEVEL=info\n"
            f"FORDJENT_GIT_NAME=Fordjent Agent\n"
            f"FORDJENT_GIT_EMAIL=fordjent@wdmn.fr\n"
        )
        with sftp.file("/opt/fordjent-deploy/.env", "w") as f:
            f.write(env_content)

        # Rewrite fordjent.yaml with real tokens
        yaml_content = open(local_fordjent_yaml).read()
        yaml_content = yaml_content.replace("{{WEBHOOK_SECRET}}", config.webhook_secret)
        yaml_content = yaml_content.replace("{{FORGEJO_TOKEN}}", forgejo_token)
        yaml_content = yaml_content.replace("{{FORGEJO_ADMIN_TOKEN}}", forgejo_admin_token)
        yaml_content = yaml_content.replace("{{SCALEWAY_AI_KEY}}", config.scaleway_ai_key)
        yaml_content = yaml_content.replace("{{ADMIN_USER}}", config.admin_user)
        yaml_content = yaml_content.replace("{{TEST_REPO}}", config.test_repo)
        yaml_content = yaml_content.replace("{{LOG_LEVEL}}", "info")
        with sftp.file("/opt/fordjent-deploy/fordjent.yaml", "w") as f:
            f.write(yaml_content)

        # Also write the fordjent config inside the Docker volume path for mounting
        # (docker compose reads from the mounted file)
        sftp.close()

    # Step 15: Create test repo + labels + webhook
    logger.info("Creating test repo and registering webhook...")
    with console.status("[bold]Setting up Forgejo repo...[/bold]"):
        # Use the Forgejo API from the instance (local network)
        forgejo_url = f"http://localhost:3000"

        # Create test repo
        out, _, _ = ssh_exec(
            ssh_client,
            f'curl -sf -X POST "{forgejo_url}/api/v1/user/repos" '
            f'-H "Authorization: token {forgejo_token}" '
            f'-H "Content-Type: application/json" '
            f'-d \'{{"name":"{config.test_repo}","description":"Fordjent integration test","private":false,"auto_init":true}}\' '
            f"2>&1 || true",
            timeout=15,
        )

        # Seed with go.mod + .gitignore
        import base64
        gomod_b64 = base64.b64encode(b"module testbed\n\ngo 1.26\n").decode()
        gitignore_b64 = base64.b64encode(b"*.o\n*.exe\ntestbed\n").decode()

        ssh_exec(
            ssh_client,
            f'curl -sf -X POST "{forgejo_url}/api/v1/repos/{config.admin_user}/{config.test_repo}/contents/go.mod" '
            f'-H "Authorization: token {forgejo_token}" '
            f'-H "Content-Type: application/json" '
            f'-d \'{{"message":"add go.mod","content":"{gomod_b64}"}}\' '
            f"2>&1 || true",
            timeout=10,
        )

        ssh_exec(
            ssh_client,
            f'curl -sf -X POST "{forgejo_url}/api/v1/repos/{config.admin_user}/{config.test_repo}/contents/.gitignore" '
            f'-H "Authorization: token {forgejo_token}" '
            f'-H "Content-Type: application/json" '
            f'-d \'{{"message":"add .gitignore","content":"{gitignore_b64}"}}\' '
            f"2>&1 || true",
            timeout=10,
        )

        # Create FSM labels
        labels = (
            "planning:0ea5db implementing:fbca04 ready:c2e07c review:fbca04 "
            "blocked:b60205 done:28a745 approved:28a745 rejected:b60205 "
            "scaffold:1d76db fordjent/failed:max-turns:b60205 fordjent/failed:error:b60205 "
            "automerge:28a745 needs-role:b60205 in_progress:fbca04 plan-approved:28a745 "
            "role:implementer:207de5 role:pm:a0d5e4 role:reviewer:e9d76f "
            "role:tester:bfd4f2 role:devops:f9d5cc"
        )
        for label_spec in labels.split():
            name, color = label_spec.split(":")
            ssh_exec(
                ssh_client,
                f'curl -sf -X POST "{forgejo_url}/api/v1/repos/{config.admin_user}/{config.test_repo}/labels" '
                f'-H "Authorization: token {forgejo_token}" '
                f'-H "Content-Type: application/json" '
                f'-d \'{{"name":"{name}","color":"{color}"}}\' '
                f"2>&1 || true",
                timeout=10,
            )

        # Register webhook (public URL)
        ssh_exec(
            ssh_client,
            f'curl -sf -X POST "{forgejo_url}/api/v1/repos/{config.admin_user}/{config.test_repo}/hooks" '
            f'-H "Authorization: token {forgejo_token}" '
            f'-H "Content-Type: application/json" '
            f'-d \'{{"type":"forgejo","config":{{"url":"https://{config.fordjent_domain}/acp/v1/events","content_type":"json","secret":"{config.webhook_secret}"}},"events":["issues","issue_comment","pull_request","pull_request_review_comment"],"active":true}}\' '
            f"2>&1 || true",
            timeout=10,
        )

        logger.info("Forgejo repo and webhook configured")

    # Step 15b: Build Fordjent Docker image
    logger.info("Building Fordjent Docker image on instance...")
    with console.status("[bold]Building Fordjent (may take 3-5 min)...[/bold]"):
        out, err, code = ssh_exec(ssh_client, "cd /opt/fordjent-deploy && docker compose build fordjent 2>&1", timeout=600)
        if code != 0:
            logger.error("Docker build failed: %s", err[:500])
            console.print(f"[red]Docker build failed. Check logs.[/red]")
            console.print(f"stderr: {err[:500]}")
            # Don't exit — the image might already exist from a previous build
        else:
            console.print("[green]Fordjent image built[/green]")

    # Step 16: Start all services
    logger.info("Starting all services...")
    with console.status("[bold]Starting Docker Compose stack...[/bold]"):
        out, err, code = ssh_exec(ssh_client, "cd /opt/fordjent-deploy && docker compose up -d 2>&1", timeout=120)
        if code != 0:
            logger.error("Docker compose up failed: %s", err)
            console.print(f"[red]Docker compose failed. stderr: {err[:500]}[/red]")
            sys.exit(1)

    # Wait for Fordjent
    logger.info("Waiting for Fordjent to be healthy...")
    with console.status("[bold]Waiting for Fordjent...[/bold]"):
        wait_for_fordjent(ssh_client, timeout=180)

    # Step 17: Check TLS
    logger.info("Checking TLS provisioning...")
    with console.status("[bold]Waiting for TLS certs (this takes 1-2 min)...[/bold]"):
        check_caddy_tls(ssh_client, config.forgejo_domain, timeout=120)

    # Step 18: Save final state
    state.update({
        "forgejo_token": forgejo_token,
        "forgejo_admin_token": forgejo_admin_token,
    })
    config.save_state(state)

    # Summary
    panel_content = (
        f"[bold]Instance:[/bold] {config.instance_type} in {config.zone}\n"
        f"[bold]IP:[/bold] {ip_address}\n"
        f"\n"
        f"[bold]Forgejo:[/bold] https://{config.forgejo_domain}\n"
        f"[bold]Fordjent:[/bold] https://{config.fordjent_domain}\n"
        f"[bold]Admin user:[/bold] {config.admin_user} / {config.admin_pass}\n"
        f"[bold]Test repo:[/bold] https://{config.forgejo_domain}/{config.admin_user}/{config.test_repo}\n"
        f"\n"
        f"[bold]Webhook:[/bold] https://{config.fordjent_domain}/acp/v1/events\n"
        f"[bold]Status:[/bold] https://{config.fordjent_domain}/status\n"
        f"\n"
        f"[bold]SSH:[/bold] ssh -i {config.ssh_key_path} root@{ip_address}\n"
        f"[bold]Teardown:[/bold] fordjent-deploy down"
    )
    console.print(Panel(panel_content, title="[bold green]Fordjent Cloud Deployment Ready[bold green]", border_style="green"))

    if not config.gandi_api_key:
        # Only show manual DNS reminder if Gandi wasn't used
        dns_panel = (
            f"[bold yellow]Action required:[/bold yellow] Point these DNS records to {ip_address}:\n\n"
            f"  forgejo.wdmn.fr   →  A  →  {ip_address}\n"
            f"  fordjent.wdmn.fr  →  A  →  {ip_address}\n\n"
            f"Caddy will auto-provision TLS certs once DNS propagates."
        )
        console.print(Panel(dns_panel, title="[bold yellow]DNS Setup Required[/bold yellow]", border_style="yellow"))
    else:
        console.print("[green]DNS records already set via Gandi. Caddy will auto-provision TLS certs.[/green]")

    ssh_client.close()


def cmd_down(args):
    config = Config()
    state = config.load_state()

    if not state:
        console.print("[red]No active deployment found. Nothing to tear down.[/red]")
        sys.exit(1)

    if state.get("zone"):
        config.zone = state["zone"]

    api = ScalewayAPI(config)

    # Clean up DNS records via Gandi (if API key provided)
    if config.gandi_api_key:
        subdomains = ["forgejo", "fordjent"]
        logger.info("Removing DNS records via Gandi API...")
        with console.status("[bold]Removing DNS A records...[/bold]"):
            from .gandi_dns import GandiDNS
            dns = GandiDNS(api_key=config.gandi_api_key, domain=config.gandi_domain)
            dns.remove_a_records(subdomains)
        console.print("[green]DNS records removed[/green]")

    server_id = state.get("server_id")
    if server_id:
        logger.info("Deleting instance %s...", server_id)
        with console.status("[bold]Deleting instance...[/bold]"):
            try:
                api.delete_server(server_id)
            except Exception as exc:
                logger.warning("Failed to delete server: %s", exc)

    ip_id = state.get("ip_id")
    if ip_id:
        logger.info("Releasing IP %s...", ip_id)
        with console.status("[bold]Releasing IP...[/bold]"):
            try:
                api.delete_ip(ip_id)
            except Exception as exc:
                logger.warning("Failed to delete IP: %s", exc)

    if STATE_FILE.is_file():
        STATE_FILE.unlink()
        logger.info("State file removed")

    logger.info("Teardown complete")
    console.print(Panel("Instance terminated, DNS cleaned up, all resources released", title="[bold green]Teardown Complete[/bold green]", border_style="green"))


def cmd_status(args):
    config = Config()
    state = config.load_state()

    if not state:
        console.print("[red]No active deployment found.[/red]")
        sys.exit(1)

    if state.get("zone"):
        config.zone = state["zone"]

    api = ScalewayAPI(config)

    try:
        server = api.get_server(state["server_id"])
        server_state = server.get("state", "unknown")
    except Exception as exc:
        server_state = f"error: {exc}"

    # Try to reach Forgejo via public URL
    forgejo_ok = False
    forgejo_domain = state.get("forgejo_domain", DOMAIN_FORGEJO)
    try:
        resp = httpx.get(f"https://{forgejo_domain}/api/v1/version", timeout=5)
        forgejo_ok = resp.status_code == 200
    except Exception:
        pass

    # Try to reach Fordjent via public URL
    fordjent_ok = False
    fordjent_domain = state.get("fordjent_domain", DOMAIN_FORDJENT)
    try:
        resp = httpx.get(f"https://{fordjent_domain}/healthz", timeout=5)
        fordjent_ok = resp.status_code == 200
    except Exception:
        pass

    panel_content = (
        f"[bold]Instance:[/bold] {state.get('instance_type', '?')} in {state.get('zone', '?')}\n"
        f"[bold]Server ID:[/bold] {state.get('server_id', '?')}\n"
        f"[bold]IP:[/bold] {state.get('ip_address', '?')}\n"
        f"[bold]Server state:[/bold] {server_state}\n"
        f"[bold]Forgejo ({forgejo_domain}):[/bold] {'healthy' if forgejo_ok else 'unreachable'}\n"
        f"[bold]Fordjent ({fordjent_domain}):[/bold] {'healthy' if fordjent_ok else 'unreachable'}\n"
        f"\n"
        f"[bold]SSH:[/bold] ssh -i {config.ssh_key_path} root@{state.get('ip_address', '?')}\n"
        f"[bold]Admin:[/bold] {state.get('admin_user', 'fjadmin')} / {state.get('admin_pass', '?')}"
    )
    console.print(Panel(panel_content, title="[bold]Deployment Status[/bold]", border_style="blue"))


def app():
    parser = argparse.ArgumentParser(
        prog="fordjent-deploy",
        description="Deploy Forgejo + Fordjent to Scaleway with TLS",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    up_parser = subparsers.add_parser("up", help="Create and provision a Scaleway instance")
    up_parser.add_argument(
        "-t", "--type",
        default="DEV1-L",
        help="Instance type (default: DEV1-L, 4 vCPU / 8GB RAM)",
    )
    up_parser.add_argument(
        "-z", "--zone",
        default=None,
        help="Availability zone (default: fr-par-2)",
    )
    up_parser.add_argument(
        "--admin-pass",
        default=None,
        help="Admin password (default: auto-generated)",
    )
    up_parser.set_defaults(func=cmd_up)

    down_parser = subparsers.add_parser("down", help="Tear down the deployment")
    down_parser.set_defaults(func=cmd_down)

    status_parser = subparsers.add_parser("status", help="Check deployment status")
    status_parser.set_defaults(func=cmd_status)

    args = parser.parse_args()
    args.func(args)