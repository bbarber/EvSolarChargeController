#!/usr/bin/env bash
#
# Registers the application with Tesla Fleet API.
#
# Tesla fetches the public key from the application domain during this call, so
# https://<domain>/.well-known/appspecific/com.tesla.3p.public-key.pem must already be serving
# over a publicly trusted certificate. Run it once per region.
#
# Usage:
#   ./tools/tesla-register-partner.sh [domain]

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$REPO_ROOT/.secrets/tesla.env"

DOMAIN="${1:-evsolarchargecontroller.duckdns.org}"
AUDIENCE="${TESLA_AUDIENCE:-https://fleet-api.prd.na.vn.cloud.tesla.com}"
TOKEN_URL="https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "fatal: $ENV_FILE not found" >&2
  exit 66
fi

# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a

for var in TESLA_CLIENT_ID TESLA_CLIENT_SECRET; do
  if [[ -z "${!var:-}" ]]; then
    echo "fatal: $var is empty in $ENV_FILE" >&2
    exit 78
  fi
done

echo "Checking the public key is reachable..."
key_url="https://${DOMAIN}/.well-known/appspecific/com.tesla.3p.public-key.pem"
if ! curl -fsS -m 20 "$key_url" | grep -q "BEGIN PUBLIC KEY"; then
  echo "fatal: $key_url did not return a PEM public key." >&2
  echo "Tesla fetches this during registration; fix it before retrying." >&2
  exit 69
fi
echo "  ok: $key_url"

echo "Requesting a partner authentication token..."
# client_credentials, not authorization_code: this token represents the application itself.
token_response=$(curl -fsS -X POST "$TOKEN_URL" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "client_id=${TESLA_CLIENT_ID}" \
  --data-urlencode "client_secret=${TESLA_CLIENT_SECRET}" \
  --data-urlencode "scope=openid vehicle_device_data vehicle_cmds vehicle_charging_cmds" \
  --data-urlencode "audience=${AUDIENCE}")

partner_token=$(printf '%s' "$token_response" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("access_token",""))')

if [[ -z "$partner_token" ]]; then
  echo "fatal: no access_token in the token response:" >&2
  printf '%s\n' "$token_response" >&2
  exit 70
fi
echo "  ok: partner token acquired"

echo "Registering domain ${DOMAIN}..."
register_response=$(curl -sS -X POST "${AUDIENCE}/api/1/partner_accounts" \
  -H "Authorization: Bearer ${partner_token}" \
  -H "Content-Type: application/json" \
  -d "{\"domain\":\"${DOMAIN}\"}")

printf '%s\n' "$register_response" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$register_response"

if printf '%s' "$register_response" | grep -q '"domain"'; then
  echo
  echo "Registered. Next: pair the virtual key to each vehicle at"
  echo "  https://tesla.com/_ak/${DOMAIN}"
  echo "Open that link on the phone paired to each car, near the vehicle, with Bluetooth on."
else
  echo
  echo "Registration did not return a domain — check the response above." >&2
  exit 71
fi
