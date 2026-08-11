#!/usr/bin/env python3
"""Reports vehicles on the account and whether each has the virtual key paired.

Read-only. Uses /api/1/vehicles and /api/1/vehicles/fleet_status, neither of which wakes a
sleeping car — unlike the vehicle_data endpoint, which does.

Usage:
    python3 tools/tesla-status.py
"""

from __future__ import annotations

import json
import os
import pathlib
import sys
import urllib.error
import urllib.parse
import urllib.request

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
ENV_FILE = REPO_ROOT / ".secrets" / "tesla.env"

TOKEN_URL = "https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token"
API_BASE = os.environ.get("TESLA_AUDIENCE", "https://fleet-api.prd.na.vn.cloud.tesla.com")


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


def request_json(url: str, *, token: str | None = None, payload: dict | None = None) -> dict:
    data = None
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"

    req = urllib.request.Request(url, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as exc:
        sys.exit(f"fatal: {url} returned HTTP {exc.code}: {exc.read().decode(errors='replace')[:400]}")


def access_token(env: dict[str, str]) -> str:
    fields = {
        "grant_type": "refresh_token",
        "client_id": env["TESLA_CLIENT_ID"],
        "refresh_token": env["TESLA_REFRESH_TOKEN"],
    }
    req = urllib.request.Request(
        TOKEN_URL,
        data=urllib.parse.urlencode(fields).encode(),
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            body = json.loads(response.read())
    except urllib.error.HTTPError as exc:
        sys.exit(f"fatal: token refresh failed HTTP {exc.code}: {exc.read().decode(errors='replace')[:400]}")

    token = body.get("access_token")
    if not token:
        sys.exit("fatal: no access_token returned")
    return token


def main() -> None:
    env = read_env(ENV_FILE)
    for key in ("TESLA_CLIENT_ID", "TESLA_REFRESH_TOKEN"):
        if not env.get(key):
            sys.exit(f"fatal: {key} is empty in {ENV_FILE}")

    token = access_token(env)

    vehicles = request_json(f"{API_BASE}/api/1/vehicles", token=token).get("response", [])
    if not vehicles:
        sys.exit("No vehicles returned. Is this the account that owns the cars?")

    vins = [v.get("vin") for v in vehicles if v.get("vin")]

    status = request_json(
        f"{API_BASE}/api/1/vehicles/fleet_status",
        token=token,
        payload={"vins": vins},
    ).get("response", {})

    paired = set(status.get("key_paired_vins") or [])
    unpaired = set(status.get("unpaired_vins") or [])

    print(f"{len(vehicles)} vehicle(s) on the account:\n")
    for vehicle in vehicles:
        vin = vehicle.get("vin", "?")
        name = vehicle.get("display_name") or "(unnamed)"
        state = vehicle.get("state", "?")
        if vin in paired:
            key = "virtual key PAIRED"
        elif vin in unpaired:
            key = "virtual key NOT paired"
        else:
            key = "virtual key status unknown"
        print(f"  {vin}  {name:<20} state={state:<8} {key}")

    print("\nFor infra/main.bicepparam:")
    print("param vins = [")
    for vin in vins:
        print(f"  '{vin}'")
    print("]")

    if unpaired:
        print(f"\n{len(unpaired)} vehicle(s) still need pairing at")
        print("  https://tesla.com/_ak/evsolarchargecontroller.duckdns.org")
        print("Open it on the phone paired to that car, near the car, with Bluetooth on.")


if __name__ == "__main__":
    main()
