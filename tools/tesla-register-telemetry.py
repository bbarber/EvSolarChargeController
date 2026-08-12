#!/usr/bin/env python3
"""Registers fleet_telemetry_config on each vehicle.

This is a signed command, so it goes through the vehicle-command proxy rather than straight to
Fleet API. Post-2021 vehicles reject it otherwise.

The `ca` field must be the certificate chain that signed the telemetry server's TLS certificate —
Tesla pins against it. It is read live off the endpoint rather than pasted in, so it cannot drift
out of sync with what the server actually presents after a renewal.

Usage:
    python3 tools/tesla-register-telemetry.py [--vin VIN] [--status-only]
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import ssl
import socket
import sys
import urllib.error
import urllib.parse
import urllib.request

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
ENV_FILE = REPO_ROOT / ".secrets" / "tesla.env"

TOKEN_URL = "https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token"
API_BASE = os.environ.get("TESLA_AUDIENCE", "https://fleet-api.prd.na.vn.cloud.tesla.com")

TELEMETRY_HOST = os.environ.get("TELEMETRY_HOST", "evsolarchargecontroller-tel.duckdns.org")
TELEMETRY_PORT = int(os.environ.get("TELEMETRY_PORT", "8443"))

PROXY_BASE_URL = os.environ.get(
    "TESLA_COMMAND_PROXY_URL",
    "https://ca-evsolar-prod-teslaproxy.agreeabledune-9500fb60.centralus.azurecontainerapps.io",
)

# Only what the controller actually consumes. Every extra field is bandwidth the car spends for
# nothing. BatteryLevel/Soc drive the state-of-charge cap; without them it silently never engages.
FIELDS = {
    "DetailedChargeState": {"interval_seconds": 60},
    "ChargeAmps": {"interval_seconds": 60},
    "ChargeCurrentRequest": {"interval_seconds": 60},
    "ChargeCurrentRequestMax": {"interval_seconds": 300},
    "ChargePortLatch": {"interval_seconds": 60},
    "BatteryLevel": {"interval_seconds": 300},
    "Soc": {"interval_seconds": 300},
}


def read_env(path: pathlib.Path) -> dict[str, str]:
    if not path.exists():
        sys.exit(f"fatal: {path} not found")
    values: dict[str, str] = {}
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        values[key.strip()] = value.strip()
    return values


def access_token(env: dict[str, str]) -> str:
    body = urllib.parse.urlencode(
        {
            "grant_type": "refresh_token",
            "client_id": env["TESLA_CLIENT_ID"],
            "refresh_token": env["TESLA_REFRESH_TOKEN"],
        }
    ).encode()
    req = urllib.request.Request(
        TOKEN_URL, data=body, headers={"Content-Type": "application/x-www-form-urlencoded"}
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.loads(r.read())["access_token"]
    except urllib.error.HTTPError as e:
        sys.exit(f"fatal: token refresh HTTP {e.code}: {e.read().decode(errors='replace')[:300]}")


def fetch_server_chain(host: str, port: int) -> str:
    """Reads the CA chain the telemetry server actually presents."""
    ctx = ssl.create_default_context()
    with socket.create_connection((host, port), timeout=20) as sock:
        with ctx.wrap_socket(sock, server_hostname=host) as ss:
            der_chain = ss.get_verified_chain() if hasattr(ss, "get_verified_chain") else None
            if not der_chain:
                sys.exit(
                    "fatal: this Python cannot read the verified chain "
                    "(needs 3.10+); supply the chain manually."
                )
            # Skip the leaf; Tesla wants the issuing chain (intermediate + root).
            return "".join(ssl.DER_cert_to_PEM_cert(d) for d in der_chain[1:])


def api(method: str, url: str, token: str, payload: dict | None, secret: str | None) -> tuple[int, str]:
    data = json.dumps(payload).encode() if payload is not None else None
    headers = {"Authorization": f"Bearer {token}"}
    if data:
        headers["Content-Type"] = "application/json"
    if secret:
        headers["X-Proxy-Secret"] = secret

    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read().decode(errors="replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--vin", action="append", help="VIN to register; repeatable. Defaults to all paired.")
    parser.add_argument("--status-only", action="store_true", help="Report current config without changing it.")
    args = parser.parse_args()

    env = read_env(ENV_FILE)
    token = access_token(env)
    secret = env.get("PROXY_SHARED_SECRET") or None

    if args.vin:
        vins = args.vin
    else:
        status, body = api("GET", f"{API_BASE}/api/1/vehicles", token, None, None)
        if status != 200:
            sys.exit(f"fatal: listing vehicles returned HTTP {status}: {body[:300]}")
        vins = [v["vin"] for v in json.loads(body).get("response", [])]

    if args.status_only:
        for vin in vins:
            status, body = api("GET", f"{API_BASE}/api/1/vehicles/{vin}/fleet_telemetry_config", token, None, None)
            print(f"{vin}: HTTP {status} {body[:400]}")
        return

    print(f"Reading the certificate chain from {TELEMETRY_HOST}:{TELEMETRY_PORT} ...")
    ca_chain = fetch_server_chain(TELEMETRY_HOST, TELEMETRY_PORT)
    print(f"  got {ca_chain.count('BEGIN CERTIFICATE')} certificate(s)")

    payload = {
        "vins": vins,
        "config": {
            "hostname": TELEMETRY_HOST,
            "port": TELEMETRY_PORT,
            "ca": ca_chain,
            "fields": FIELDS,
            "prefer_typed": True,
        },
    }

    url = f"{PROXY_BASE_URL.rstrip('/')}/api/1/vehicles/fleet_telemetry_config"
    print(f"Registering {len(vins)} vehicle(s) via the command proxy ...")
    status, body = api("POST", url, token, payload, secret)
    print(f"HTTP {status}")
    try:
        print(json.dumps(json.loads(body), indent=2)[:1500])
    except json.JSONDecodeError:
        print(body[:800])

    if status == 200:
        print("\nPoll until synced is true:")
        print("  python3 tools/tesla-register-telemetry.py --status-only")


if __name__ == "__main__":
    main()
