#!/usr/bin/env bash
#
# Starts tesla-http-proxy behind nginx.

set -euo pipefail

TESLA_KEY_FILE="${TESLA_KEY_FILE:-/etc/tesla/fleet-key.pem}"
TESLA_HTTP_PROXY_PORT="${TESLA_HTTP_PROXY_PORT:-4443}"
TLS_CERT="${TESLA_HTTP_PROXY_TLS_CERT:-/tmp/proxy-cert.pem}"
TLS_KEY="${TESLA_HTTP_PROXY_TLS_KEY:-/tmp/proxy-key.pem}"

if [[ ! -f "$TESLA_KEY_FILE" ]]; then
  echo "fatal: fleet private key not found at $TESLA_KEY_FILE" >&2
  echo "Seed it into the mounted Azure Files share — see docs/SETUP.md." >&2
  exit 78
fi

# The proxy insists on TLS even on loopback. Nothing external terminates here, so a self-signed
# certificate generated per container start is sufficient and avoids shipping one in the image.
if [[ ! -f "$TLS_CERT" || ! -f "$TLS_KEY" ]]; then
  echo "Generating a self-signed loopback certificate for the proxy listener."
  TLS_CERT=/tmp/proxy-cert.pem
  TLS_KEY=/tmp/proxy-key.pem
  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$TLS_KEY" \
    -out "$TLS_CERT" \
    -days 3650 \
    -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
    2>/dev/null
fi

if [[ -z "${PROXY_SHARED_SECRET:-}" ]]; then
  echo "fatal: PROXY_SHARED_SECRET is not set; refusing to start an unauthenticated signing proxy." >&2
  exit 78
fi

export PROXY_SHARED_SECRET
envsubst '${PROXY_SHARED_SECRET}' \
  < /etc/nginx/templates/proxy.conf.template \
  > /etc/nginx/conf.d/proxy.conf

rm -f /etc/nginx/sites-enabled/default

echo "Starting tesla-http-proxy on 127.0.0.1:${TESLA_HTTP_PROXY_PORT}"
tesla-http-proxy \
  -key-file "$TESLA_KEY_FILE" \
  -cert "$TLS_CERT" \
  -tls-key "$TLS_KEY" \
  -host 127.0.0.1 \
  -port "$TESLA_HTTP_PROXY_PORT" &
proxy_pid=$!

# If either process dies the container should restart, rather than sit there half-working.
trap 'kill -TERM "$proxy_pid" 2>/dev/null || true' TERM INT

echo "Starting nginx on :8080"
nginx -g 'daemon off;' &
nginx_pid=$!

wait -n "$proxy_pid" "$nginx_pid"
exit_code=$?
echo "A component exited with code $exit_code; shutting down." >&2
kill -TERM "$proxy_pid" "$nginx_pid" 2>/dev/null || true
exit "$exit_code"
