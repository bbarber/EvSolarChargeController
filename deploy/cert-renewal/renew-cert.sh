#!/usr/bin/env bash
# Renews the telemetry certificate on the VM itself and installs it when it changes.
#
# Run daily by evsolar-cert-renew.timer, as root. certbot no-ops until the certificate is within
# 30 days of expiry, so most runs do nothing. The DuckDNS token lives only in certbot's renewal
# config under $LE (root-only); nothing here needs GitHub or a person.
#
# Exits non-zero if the certificate actually being served is within $MIN_DAYS of expiry, so a
# silently broken renewal shows up as a failed unit (and in the external GitHub check) long before
# the cars start refusing the handshake. That is exactly what happened on 2026-11-14.
set -euo pipefail

DEPLOY_DIR=${DEPLOY_DIR:-/opt/evsolar}
LE=$DEPLOY_DIR/letsencrypt
FT=$DEPLOY_DIR/fleet-telemetry
IMAGE=evsolar-certbot:local
MIN_DAYS=${MIN_DAYS:-14}
HERE=$(cd "$(dirname "$0")" && pwd)

docker build -q -t "$IMAGE" "$HERE" >/dev/null

docker run --rm -v "$LE:/le" "$IMAGE" renew --non-interactive \
  --config-dir /le/config --work-dir /le/work --logs-dir /le/logs

live=$LE/config/live/telemetry
if ! cmp -s "$live/fullchain.pem" "$FT/server-cert.pem"; then
  echo "new certificate issued; installing"
  # fleet-telemetry runs as uid 65532 and the key is 0600, so ownership must match.
  install -o 65532 -g 65532 -m 644 "$live/fullchain.pem" "$FT/server-cert.pem"
  install -o 65532 -g 65532 -m 600 "$live/privkey.pem" "$FT/server-key.pem"
  # Both read the certificate only at startup. The controller is left running.
  (cd "$DEPLOY_DIR" && docker compose restart fleet-telemetry public-key)
  sleep 5
fi

# Check what is served, not what is on disk: a restart that failed would otherwise pass.
# s_client exits non-zero here because fleet-telemetry drops a client with no certificate after
# the server certificate has already been sent, so its status is ignored and the PEM checked.
served=$(echo | openssl s_client -connect localhost:8443 2>/dev/null | openssl x509 2>/dev/null || true)
if [[ -z "$served" ]]; then
  echo "could not read the certificate served on 8443" >&2
  exit 1
fi
echo "served certificate valid until: $(openssl x509 -noout -enddate <<<"$served" | cut -d= -f2)"
if ! openssl x509 -noout -checkend $((MIN_DAYS * 86400)) <<<"$served" >/dev/null; then
  echo "served certificate expires within $MIN_DAYS days" >&2
  exit 1
fi
