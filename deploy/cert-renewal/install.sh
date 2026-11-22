#!/usr/bin/env bash
# One-time install on the VM, as root, from /opt/evsolar/cert-renewal.
#
# Assumes the first certificate was already issued into /opt/evsolar/letsencrypt (see
# docs/SETUP.md step 9), which is what stores the DuckDNS token in certbot's renewal config.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
chmod 700 /opt/evsolar/letsencrypt   # holds the DuckDNS token and the account key
install -m 644 "$HERE/evsolar-cert-renew.service" "$HERE/evsolar-cert-renew.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now evsolar-cert-renew.timer
systemctl list-timers evsolar-cert-renew.timer --no-pager
