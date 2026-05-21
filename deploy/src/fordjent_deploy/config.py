import os
import json
import logging
from dataclasses import dataclass, field
from pathlib import Path

logger = logging.getLogger(__name__)

DEFAULT_ZONE = "fr-par-2"
DEFAULT_INSTANCE_TYPE = "DEV1-L"
DEFAULT_IMAGE_LABEL = "ubuntu_jammy"
DOMAIN_FORGEJO = "forgejo.wdmn.fr"
DOMAIN_FORDJENT = "fordjent.wdmn.fr"
DOMAIN_BASE = "wdmn.fr"  # base domain for Gandi DNS

STATE_DIR = Path(os.path.expanduser("~/.config/fordjent-deploy"))
STATE_FILE = STATE_DIR / "state.json"

_SSH_KEY_CANDIDATES = ["id_ed25519", "id_ecdsa", "id_rsa"]


def _detect_ssh_key() -> str:
    ssh_dir = Path(os.path.expanduser("~/.ssh"))
    for name in _SSH_KEY_CANDIDATES:
        key_path = ssh_dir / name
        if key_path.exists():
            logger.info(f"Detected SSH key: {key_path}")
            return str(key_path)
    return str(ssh_dir / "id_ed25519")


def generate_password(n: int = 16) -> str:
    import secrets
    return secrets.token_hex(n)


def generate_token(n: int = 20) -> str:
    import secrets
    return secrets.token_hex(n)


@dataclass
class Config:
    access_key: str = field(default_factory=lambda: os.environ.get("SCW_ACCESS_KEY", ""))
    secret_key: str = field(default_factory=lambda: os.environ.get("SCW_SECRET_KEY", ""))
    project_id: str = field(default_factory=lambda: os.environ.get("SCW_PROJECT_ID", ""))
    zone: str = field(default_factory=lambda: os.environ.get("SCW_DEFAULT_ZONE", DEFAULT_ZONE))
    ssh_key_path: str = field(default_factory=lambda: os.environ.get("SCW_SSH_KEY_PATH", _detect_ssh_key()))
    instance_type: str = DEFAULT_INSTANCE_TYPE
    image_label: str = DEFAULT_IMAGE_LABEL
    admin_user: str = "fjadmin"
    admin_pass: str = field(default_factory=lambda: generate_password(16))
    test_repo: str = "testbed"
    forgejo_domain: str = DOMAIN_FORGEJO
    fordjent_domain: str = DOMAIN_FORDJENT
    webhook_secret: str = field(default_factory=lambda: generate_token(16))
    forgejo_secret_key: str = field(default_factory=lambda: generate_token(20))
    forgejo_internal_token: str = field(default_factory=lambda: generate_token(20))
    scaleway_ai_key: str = field(default_factory=lambda: os.environ.get("SCALEWAY_AI_KEY", ""))
    wafer_api_key: str = field(default_factory=lambda: os.environ.get("WAFER_API_KEY", ""))
    gandi_api_key: str = field(default_factory=lambda: os.environ.get("GANDI_API_KEY", ""))
    gandi_domain: str = DOMAIN_BASE

    def _read_ssh_public_key(self) -> str:
        if hasattr(self, "_ssh_public_key_cache"):
            return self._ssh_public_key_cache
        key_path = Path(self.ssh_key_path)
        pub_path = Path(str(key_path) + ".pub")
        if pub_path.is_file():
            content = pub_path.read_text().strip()
            logger.info(f"Read SSH public key from {pub_path}")
            self._ssh_public_key_cache = content
            return content
        raise FileNotFoundError(f"No public key found at {pub_path}")

    def validate(self) -> None:
        missing = []
        for name, value in [
            ("SCW_ACCESS_KEY", self.access_key),
            ("SCW_SECRET_KEY", self.secret_key),
            ("SCW_PROJECT_ID", self.project_id),
        ]:
            if not value:
                missing.append(name)
        if missing:
            raise EnvironmentError(f"Missing required environment variables: {', '.join(missing)}")
        self._read_ssh_public_key()

    def load_state(self) -> dict:
        if STATE_FILE.is_file():
            return json.loads(STATE_FILE.read_text())
        return {}

    def save_state(self, state: dict) -> None:
        STATE_DIR.mkdir(parents=True, exist_ok=True)
        STATE_FILE.write_text(json.dumps(state, indent=2))
        logger.info("State saved to %s", STATE_FILE)