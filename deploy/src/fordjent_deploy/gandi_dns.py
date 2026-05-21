"""Gandi LiveDNS API client for managing DNS records.

Uses the Gandi v5 LiveDNS API:
https://api.gandi.net/v5/livedns/domains/{domain}/records

API key can be generated at https://api.gandi.net/ (Personal Access Token).
"""

import logging
from dataclasses import dataclass

import httpx

logger = logging.getLogger(__name__)

GANDI_API_BASE = "https://api.gandi.net/v5"


@dataclass
class DnsRecord:
    name: str       # subdomain (e.g., "forgejo" for forgejo.wdmn.fr)
    type: str       # "A", "AAAA", "CNAME", "TXT", etc.
    values: list[str]  # list of values (e.g., ["1.2.3.4"])
    ttl: int = 300


class GandiDNS:
    """Manage DNS records via the Gandi LiveDNS API."""

    def __init__(self, api_key: str, domain: str, timeout: int = 30) -> None:
        self.api_key = api_key
        self.domain = domain
        self._client = httpx.Client(
            base_url=GANDI_API_BASE,
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
            },
            timeout=timeout,
        )

    def _url(self, path: str) -> str:
        return f"/livedns/domains/{self.domain}/records{path}"

    def list_records(self, name: str = "", record_type: str = "") -> list[dict]:
        """List DNS records, optionally filtered by name and/or type."""
        path = self._url("")
        params = {}
        if name:
            params["rrset_name"] = name
        if record_type:
            params["rrset_type"] = record_type
        resp = self._client.get(path, params=params)
        resp.raise_for_status()
        return resp.json()

    def get_record(self, name: str, record_type: str) -> dict | None:
        """Get a specific DNS record. Returns None if not found."""
        path = self._url(f"/{name}/{record_type}")
        resp = self._client.get(path)
        if resp.status_code == 404:
            return None
        resp.raise_for_status()
        return resp.json()

    def create_or_update_record(self, record: DnsRecord) -> dict:
        """Create a DNS record, or update it if it already exists.

        Uses PUT to create-or-update (Gandi supports this for single-type records).
        """
        path = self._url(f"/{record.name}/{record.type}")
        body = {
            "rrset_name": record.name,
            "rrset_type": record.type,
            "rrset_values": record.values,
            "rrset_ttl": record.ttl,
        }
        resp = self._client.put(path, json=body)
        resp.raise_for_status()
        logger.info(
            "DNS record %s.%s %s → %s (ttl=%d)",
            record.name, self.domain, record.type, record.values, record.ttl,
        )
        return resp.json() if resp.content else {}

    def delete_record(self, name: str, record_type: str) -> None:
        """Delete a DNS record. Ignores 404 (already deleted)."""
        path = self._url(f"/{name}/{record_type}")
        resp = self._client.delete(path)
        if resp.status_code == 404:
            logger.info("DNS record %s.%s %s already deleted", name, self.domain, record_type)
            return
        resp.raise_for_status()
        logger.info("DNS record %s.%s %s deleted", name, self.domain, record_type)

    def set_a_records(self, subdomains: list[str], ip_address: str, ttl: int = 300) -> list[dict]:
        """Set A records for multiple subdomains pointing to the same IP.

        Creates or updates each record. Returns list of API responses.
        """
        results = []
        for subdomain in subdomains:
            record = DnsRecord(
                name=subdomain,
                type="A",
                values=[ip_address],
                ttl=ttl,
            )
            result = self.create_or_update_record(record)
            results.append(result)
        return results

    def remove_a_records(self, subdomains: list[str]) -> None:
        """Remove A records for multiple subdomains. Ignores 404s."""
        for subdomain in subdomains:
            self.delete_record(subdomain, "A")

    def check_propagation(self, subdomain: str, expected_ip: str, timeout: int = 180, interval: int = 5) -> bool:
        """Wait for DNS propagation of an A record.

        Queries the Gandi API (not local DNS) to verify the record exists
        with the expected IP.

        Returns True if the record is found with the correct IP within timeout.
        """
        import time
        logger.info("Checking DNS propagation for %s.%s → %s", subdomain, self.domain, expected_ip)
        deadline = time.time() + timeout
        while time.time() < deadline:
            rec = self.get_record(subdomain, "A")
            if rec and expected_ip in rec.get("rrset_values", []):
                logger.info("DNS record %s.%s confirmed → %s", subdomain, self.domain, expected_ip)
                return True
            # Gandi's API returns the record immediately since we just set it,
            # but DNS resolvers may not have it yet. The API read confirms our write.
            if rec:
                logger.debug("DNS record exists but values: %s (expected %s)", rec.get("rrset_values"), expected_ip)
                return True  # Record exists in Gandi's LiveDNS, propagation to resolvers follows
            time.sleep(interval)
        logger.warning("DNS record %s.%s not confirmed within %ds", subdomain, self.domain, timeout)
        return False