#!/usr/bin/env python3
"""One-time Tesla Fleet API authorization.

Grants this application access to your Tesla account and captures the resulting refresh token.

This must run *before* pairing a virtual key to a vehicle — the car refuses to add a key for an
application the account has not authorized.

Opens a browser, catches the redirect on localhost:8080, exchanges the code, and writes the
refresh token into .secrets/tesla.env. The token rotates on every use after this; the app writes
replacements to Key Vault itself.

Usage:
    python3 tools/tesla-oauth.py
"""

from __future__ import annotations

import base64
import hashlib
import http.server
import json
import os
import pathlib
import secrets
import socketserver
import sys
import threading
import urllib.parse
import urllib.request
import webbrowser

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
ENV_FILE = REPO_ROOT / ".secrets" / "tesla.env"

AUTHORIZE_URL = "https://auth.tesla.com/oauth2/v3/authorize"
TOKEN_URL = "https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token"
AUDIENCE = os.environ.get("TESLA_AUDIENCE", "https://fleet-api.prd.na.vn.cloud.tesla.com")

REDIRECT_PORT = 8080
REDIRECT_URI = f"http://localhost:{REDIRECT_PORT}/callback"

# offline_access is what makes the response include a refresh token; without it the grant expires
# in hours and the timer breaks overnight.
# vehicle_location is required to subscribe to the Location telemetry field, which drives the
# at-home gate. Tesla returns 403 "missing scopes vehicle_location" on registration without it.
SCOPES = "openid offline_access vehicle_device_data vehicle_location vehicle_cmds vehicle_charging_cmds"


def read_env(path: pathlib.Path) -> dict[str, str]:
    values: dict[str, str] = {}
    if not path.exists():
        sys.exit(f"fatal: {path} not found")
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        values[key.strip()] = value.strip()
    return values


def write_env_value(path: pathlib.Path, key: str, value: str) -> None:
    """Replaces one key in place, preserving comments and ordering."""
    lines = path.read_text().splitlines()
    for i, line in enumerate(lines):
        if line.startswith(f"{key}="):
            lines[i] = f"{key}={value}"
            break
    else:
        lines.append(f"{key}={value}")
    path.write_text("\n".join(lines) + "\n")


class CallbackHandler(http.server.BaseHTTPRequestHandler):
    code: str | None = None
    state: str | None = None
    error: str | None = None

    def do_GET(self) -> None:  # noqa: N802 - required name
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path != "/callback":
            self.send_response(404)
            self.end_headers()
            return

        params = urllib.parse.parse_qs(parsed.query)
        CallbackHandler.code = params.get("code", [None])[0]
        CallbackHandler.state = params.get("state", [None])[0]
        CallbackHandler.error = params.get("error_description", params.get("error", [None]))[0]

        body = (
            b"<h2>Authorization received.</h2><p>You can close this tab and return to the terminal.</p>"
            if CallbackHandler.code
            else b"<h2>Authorization failed.</h2><p>Check the terminal for details.</p>"
        )
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args) -> None:
        pass  # Keep the console output to just our own progress lines.


def post_form(url: str, fields: dict[str, str]) -> dict:
    data = urllib.parse.urlencode(fields).encode()
    request = urllib.request.Request(
        url,
        data=data,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as exc:
        sys.exit(f"fatal: {url} returned HTTP {exc.code}: {exc.read().decode(errors='replace')}")


def main() -> None:
    env = read_env(ENV_FILE)
    client_id = env.get("TESLA_CLIENT_ID", "")
    client_secret = env.get("TESLA_CLIENT_SECRET", "")

    if not client_id or not client_secret:
        sys.exit(f"fatal: TESLA_CLIENT_ID and TESLA_CLIENT_SECRET must be set in {ENV_FILE}")

    state = secrets.token_urlsafe(24)

    # PKCE: harmless with a confidential client, and required if the app is ever switched to public.
    verifier = secrets.token_urlsafe(64)
    challenge = base64.urlsafe_b64encode(
        hashlib.sha256(verifier.encode()).digest()
    ).decode().rstrip("=")

    query = urllib.parse.urlencode(
        {
            "response_type": "code",
            "client_id": client_id,
            "redirect_uri": REDIRECT_URI,
            "scope": SCOPES,
            "state": state,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
        }
    )
    auth_url = f"{AUTHORIZE_URL}?{query}"

    socketserver.TCPServer.allow_reuse_address = True
    with socketserver.TCPServer(("127.0.0.1", REDIRECT_PORT), CallbackHandler) as server:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        print("Opening the Tesla authorization page in your browser.")
        print("Sign in with the account that owns the vehicles, and approve the requested scopes.")
        print(f"\nIf the browser does not open, visit:\n{auth_url}\n")
        webbrowser.open(auth_url)

        print(f"Waiting for the redirect to {REDIRECT_URI} ...")
        while CallbackHandler.code is None and CallbackHandler.error is None:
            threading.Event().wait(0.3)

        server.shutdown()

    if CallbackHandler.error:
        sys.exit(f"fatal: authorization failed: {CallbackHandler.error}")

    if CallbackHandler.state != state:
        sys.exit("fatal: state mismatch on the callback; aborting rather than trusting the response")

    print("Authorization code received. Exchanging for tokens...")

    tokens = post_form(
        TOKEN_URL,
        {
            "grant_type": "authorization_code",
            "client_id": client_id,
            "client_secret": client_secret,
            "code": CallbackHandler.code or "",
            "audience": AUDIENCE,
            "redirect_uri": REDIRECT_URI,
            "code_verifier": verifier,
        },
    )

    refresh_token = tokens.get("refresh_token")
    if not refresh_token:
        sys.exit(f"fatal: no refresh_token in the response: {json.dumps(tokens)[:400]}")

    write_env_value(ENV_FILE, "TESLA_REFRESH_TOKEN", refresh_token)

    print(f"\nRefresh token written to {ENV_FILE}")
    print(f"  scopes granted: {tokens.get('scope', '(not reported)')}")
    print(f"  access token expires in: {tokens.get('expires_in', '?')}s")
    print("\nNext: pair the virtual key on each car —")
    print("  https://tesla.com/_ak/evsolarchargecontroller.duckdns.org")


if __name__ == "__main__":
    main()
