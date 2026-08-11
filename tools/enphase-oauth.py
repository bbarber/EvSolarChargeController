#!/usr/bin/env python3
"""One-time Enphase Enlighten (API v4) authorization.

Captures a refresh token and discovers the system id, writing both into .secrets/enphase.env.

Two things about Enphase's OAuth are worth knowing:
  * The refresh token expires after one month and rotates on every use, so it cannot live in an
    immutable app setting. The Function writes replacements to Key Vault itself; this script only
    seeds the first one.
  * The token endpoint takes its parameters in the query string with HTTP Basic client
    credentials, not a form body.

Usage:
    python3 tools/enphase-oauth.py
"""

from __future__ import annotations

import base64
import http.server
import json
import pathlib
import secrets
import socketserver
import sys
import threading
import urllib.error
import urllib.parse
import urllib.request
import webbrowser

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
ENV_FILE = REPO_ROOT / ".secrets" / "enphase.env"

AUTHORIZE_URL = "https://api.enphaseenergy.com/oauth/authorize"
TOKEN_URL = "https://api.enphaseenergy.com/oauth/token"
API_BASE = "https://api.enphaseenergy.com/api/v4"

REDIRECT_PORT = 8080
REDIRECT_URI = f"http://localhost:{REDIRECT_PORT}/callback"


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


def write_env_value(path: pathlib.Path, key: str, value: str) -> None:
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
    error: str | None = None

    def do_GET(self) -> None:  # noqa: N802
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path != "/callback":
            self.send_response(404)
            self.end_headers()
            return

        params = urllib.parse.parse_qs(parsed.query)
        CallbackHandler.code = params.get("code", [None])[0]
        CallbackHandler.error = params.get("error_description", params.get("error", [None]))[0]

        body = (
            b"<h2>Authorization received.</h2><p>You can close this tab.</p>"
            if CallbackHandler.code
            else b"<h2>Authorization failed.</h2><p>Check the terminal.</p>"
        )
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args) -> None:
        pass


def basic_auth(client_id: str, client_secret: str) -> str:
    raw = f"{client_id}:{client_secret}".encode()
    return "Basic " + base64.b64encode(raw).decode()


def main() -> None:
    env = read_env(ENV_FILE)
    client_id = env.get("ENPHASE_CLIENT_ID", "")
    client_secret = env.get("ENPHASE_CLIENT_SECRET", "")
    api_key = env.get("ENPHASE_API_KEY", "")

    for name, value in (
        ("ENPHASE_CLIENT_ID", client_id),
        ("ENPHASE_CLIENT_SECRET", client_secret),
        ("ENPHASE_API_KEY", api_key),
    ):
        if not value:
            sys.exit(f"fatal: {name} is empty in {ENV_FILE}")

    query = urllib.parse.urlencode(
        {"response_type": "code", "client_id": client_id, "redirect_uri": REDIRECT_URI}
    )
    auth_url = f"{AUTHORIZE_URL}?{query}"

    socketserver.TCPServer.allow_reuse_address = True
    with socketserver.TCPServer(("127.0.0.1", REDIRECT_PORT), CallbackHandler) as server:
        threading.Thread(target=server.serve_forever, daemon=True).start()

        print("Opening the Enphase authorization page.")
        print("Sign in with the Enlighten account that owns the solar system.")
        print(f"\nIf the browser does not open, visit:\n{auth_url}\n")
        webbrowser.open(auth_url)

        print(f"Waiting for the redirect to {REDIRECT_URI} ...")
        print("(If Enphase rejects the redirect URI, add it to the app's Redirect URLs first.)")
        while CallbackHandler.code is None and CallbackHandler.error is None:
            threading.Event().wait(0.3)

        server.shutdown()

    if CallbackHandler.error:
        sys.exit(f"fatal: authorization failed: {CallbackHandler.error}")

    print("Authorization code received. Exchanging for tokens...")

    token_query = urllib.parse.urlencode(
        {
            "grant_type": "authorization_code",
            "redirect_uri": REDIRECT_URI,
            "code": CallbackHandler.code or "",
        }
    )
    req = urllib.request.Request(
        f"{TOKEN_URL}?{token_query}",
        data=b"",
        headers={"Authorization": basic_auth(client_id, client_secret)},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            tokens = json.loads(response.read())
    except urllib.error.HTTPError as exc:
        sys.exit(f"fatal: token exchange failed HTTP {exc.code}: {exc.read().decode(errors='replace')[:400]}")

    refresh_token = tokens.get("refresh_token")
    access_token = tokens.get("access_token")
    if not refresh_token or not access_token:
        sys.exit(f"fatal: unexpected token response: {json.dumps(tokens)[:400]}")

    write_env_value(ENV_FILE, "ENPHASE_REFRESH_TOKEN", refresh_token)
    print(f"Refresh token written to {ENV_FILE}")

    print("Looking up the system id...")
    systems_req = urllib.request.Request(
        f"{API_BASE}/systems?{urllib.parse.urlencode({'key': api_key})}",
        headers={"Authorization": f"Bearer {access_token}"},
    )
    try:
        with urllib.request.urlopen(systems_req, timeout=30) as response:
            systems = json.loads(response.read())
    except urllib.error.HTTPError as exc:
        print(f"warning: /systems returned HTTP {exc.code}; set ENPHASE_SYSTEM_ID by hand.", file=sys.stderr)
        print(exc.read().decode(errors="replace")[:300], file=sys.stderr)
        return

    entries = systems.get("systems") or []
    if not entries:
        print("warning: no systems returned; set ENPHASE_SYSTEM_ID by hand.", file=sys.stderr)
        return

    for entry in entries:
        print(f"  system_id={entry.get('system_id')}  {entry.get('name')}  status={entry.get('status')}")

    system_id = str(entries[0].get("system_id", ""))
    if system_id:
        write_env_value(ENV_FILE, "ENPHASE_SYSTEM_ID", system_id)
        print(f"\nENPHASE_SYSTEM_ID set to {system_id}")

    if len(entries) > 1:
        print("More than one system found — confirm the right one is set in the env file.")

    # This call counts against the 1000/month Watt-plan budget, same as the timer's polls.
    print("\nNote: that lookup used one of the month's 1000 Enphase API calls.")


if __name__ == "__main__":
    main()
