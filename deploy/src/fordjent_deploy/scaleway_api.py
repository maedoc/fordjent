import time
import logging
import httpx

from .config import Config

logger = logging.getLogger(__name__)

BASE_URL = "https://api.scaleway.com"


class ScalewayAPI:
    def __init__(self, config: Config) -> None:
        self.config = config
        self._client = httpx.Client(
            base_url=BASE_URL,
            headers={
                "X-Auth-Token": config.secret_key,
                "Content-Type": "application/json",
            },
            timeout=30.0,
        )

    def resolve_image(self, image_label: str) -> str:
        logger.info("Resolving image label '%s' in zone '%s'", image_label, self.config.zone)
        resp = self._client.get(
            "/marketplace/v2/local-images",
            params={"image_label": image_label, "zone": self.config.zone},
        )
        resp.raise_for_status()
        data = resp.json()
        # Prefer SBS images for DEV1 compatibility
        for img in data.get("local_images", []):
            if img.get("arch") == "x86_64" and img.get("type") == "instance_sbs":
                compat = img.get("compatible_commercial_types", [])
                if self.config.instance_type in compat:
                    logger.info("Selected SBS image: %s", img["id"])
                    return img["id"]
        # Fallback: local images for DEV1
        for img in data.get("local_images", []):
            if img.get("arch") == "x86_64" and img.get("type") == "instance_local":
                compat = img.get("compatible_commercial_types", [])
                if self.config.instance_type in compat:
                    logger.info("Selected local image: %s", img["id"])
                    return img["id"]
        # Last resort: any x86_64 SBS
        for img in data.get("local_images", []):
            if img.get("arch") == "x86_64" and img.get("type") == "instance_sbs":
                logger.warning("Fallback SBS image (may not be DEV1-compatible): %s", img["id"])
                return img["id"]
        raise ValueError(
            f"No x86_64 image found for label '{image_label}' in zone '{self.config.zone}' "
            f"compatible with {self.config.instance_type}"
        )

    def create_ip(self) -> tuple[str, str]:
        logger.info("Creating flexible IPv4...")
        resp = self._client.post(
            f"/instance/v1/zones/{self.config.zone}/ips",
            json={
                "project": self.config.project_id,
                "type": "routed_ipv4",
            },
        )
        resp.raise_for_status()
        data = resp.json()["ip"]
        return data["id"], data["address"]

    def create_server(self, image_uuid: str, ip_id: str, name: str) -> dict:
        logger.info("Creating server '%s' (type=%s, image=%s)", name, self.config.instance_type, image_uuid)
        server_spec = {
            "name": name,
            "commercial_type": self.config.instance_type,
            "image": image_uuid,
            "project": self.config.project_id,
            "public_ip": ip_id,
        }

        # DEV1 instances use local storage, not SBS volumes or scratch
        # No scratch volume needed for non-GPU instances

        resp = self._client.post(
            f"/instance/v1/zones/{self.config.zone}/servers",
            json=server_spec,
        )
        if resp.status_code == 403:
            body = resp.json()
            raise RuntimeError(f"Instance quota exceeded: {body.get('message')}")
        resp.raise_for_status()
        server = resp.json().get("server", resp.json())

        self._poweron_with_retry(server["id"], timeout=300, interval=15)

        return server

    def _poweron_with_retry(self, server_id: str, timeout: int = 300, interval: int = 15) -> None:
        deadline = time.time() + timeout
        attempt = 0
        while time.time() < deadline:
            attempt += 1
            resp = self._client.post(
                f"/instance/v1/zones/{self.config.zone}/servers/{server_id}/action",
                json={"action": "poweron"},
            )
            if resp.status_code == 202:
                logger.info("Server %s powering on", server_id)
                return
            if resp.status_code == 412:
                body = resp.json()
                reason = body.get("message", "out of stock")
                if attempt % 4 == 1:
                    logger.info("Server %s out of stock (attempt %d): %s — retrying in %ds...", server_id, attempt, reason, interval)
                time.sleep(interval)
                continue
            resp.raise_for_status()
            return
        raise RuntimeError(f"Server {server_id} could not power on within {timeout}s")

    def wait_server_running(self, server_id: str, timeout: int = 300, interval: int = 10) -> dict:
        logger.info("Waiting for server %s to reach 'running' state...", server_id)
        deadline = time.time() + timeout
        stopped_count = 0
        while time.time() < deadline:
            server = self.get_server(server_id)
            state = server.get("state", "")
            if state == "running":
                return server
            if state in ("stopped", "stopped in place"):
                stopped_count += 1
                if stopped_count > 3:
                    raise RuntimeError(f"Server {server_id} entered unexpected state: {state}")
                logger.info("Server %s in state '%s', may still be booting", server_id, state)
            elif state == "error":
                raise RuntimeError(f"Server {server_id} entered error state")
            time.sleep(interval)
        raise TimeoutError(f"Server {server_id} did not reach 'running' state within {timeout}s")

    def get_server(self, server_id: str) -> dict:
        resp = self._client.get(
            f"/instance/v1/zones/{self.config.zone}/servers/{server_id}",
        )
        resp.raise_for_status()
        return resp.json().get("server", resp.json())

    def get_server_ip(self, server_id: str) -> str:
        server = self.get_server(server_id)
        pub_ip = server.get("public_ip")
        if pub_ip and isinstance(pub_ip, dict):
            return pub_ip["address"]
        for ip in server.get("ips", []):
            if isinstance(ip, dict) and ip.get("address"):
                return ip["address"]
        raise ValueError(f"No public IP found for server {server_id}")

    def ensure_ssh_key(self, public_key: str) -> str:
        resp = self._client.get("/iam/v1alpha1/ssh-keys", params={"project_id": self.config.project_id})
        resp.raise_for_status()
        for key in resp.json().get("ssh_keys", []):
            registered = key.get("public_key", "").strip()
            incoming = " ".join(public_key.strip().split()[:2])
            if registered == incoming or registered == public_key.strip():
                logger.info("SSH key already registered: %s (%s)", key.get("name"), key["id"])
                return key["id"]
        key_name = "fordjent-deploy"
        resp = self._client.post(
            "/iam/v1alpha1/ssh-keys",
            json={"name": key_name, "public_key": public_key, "project_id": self.config.project_id},
        )
        resp.raise_for_status()
        key_id = resp.json()["id"]
        logger.info("Registered SSH key: %s (%s)", key_name, key_id)
        return key_id

    def delete_server(self, server_id: str, timeout: int = 300, interval: int = 10) -> None:
        logger.info("Ensuring server %s is stopped before deletion...", server_id)
        server = self.get_server(server_id)
        state = server.get("state", "")
        if state == "running":
            logger.info("Powering off server %s...", server_id)
            self._client.post(
                f"/instance/v1/zones/{self.config.zone}/servers/{server_id}/action",
                json={"action": "poweroff"},
            )
            deadline = time.time() + timeout
            while time.time() < deadline:
                s = self.get_server(server_id)
                if s.get("state") in ("stopped", "stopped in place"):
                    break
                time.sleep(interval)
            else:
                logger.warning("Server %s did not stop within %ds, forcing deletion", server_id, timeout)

        logger.info("Deleting server %s...", server_id)
        self._client.delete(
            f"/instance/v1/zones/{self.config.zone}/servers/{server_id}",
        ).raise_for_status()
        logger.info("Server %s deleted", server_id)

    def delete_ip(self, ip_id: str) -> None:
        logger.info("Releasing IP %s...", ip_id)
        self._client.delete(
            f"/instance/v1/zones/{self.config.zone}/ips/{ip_id}",
        ).raise_for_status()
        logger.info("IP %s released", ip_id)