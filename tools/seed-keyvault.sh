#!/usr/bin/env bash
#
# Seeds third-party credentials into Key Vault. Run once after the infrastructure deploy, and
# again whenever a credential is rotated.
#
# Secrets are deliberately kept out of Bicep parameter files and out of git: the parameters file
# is committed, this input is not.
#
# Usage:
#   ./tools/seed-keyvault.sh <key-vault-name> [env-file ...]
#
# Env files are plain KEY=VALUE. By default .secrets/enphase.env and .secrets/tesla.env are read
# if they exist.

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <key-vault-name> [env-file ...]" >&2
  exit 64
fi

VAULT="$1"
shift

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ $# -gt 0 ]]; then
  ENV_FILES=("$@")
else
  ENV_FILES=("$REPO_ROOT/.secrets/enphase.env" "$REPO_ROOT/.secrets/tesla.env")
fi

# Maps the env-file variable name to the Key Vault secret name the Function App expects.
declare -A SECRET_NAMES=(
  [ENPHASE_API_KEY]=enphase-api-key
  [ENPHASE_CLIENT_ID]=enphase-client-id
  [ENPHASE_CLIENT_SECRET]=enphase-client-secret
  [ENPHASE_REFRESH_TOKEN]=enphase-refresh-token
  [ENPHASE_SYSTEM_ID]=enphase-system-id
  [TESLA_CLIENT_ID]=tesla-client-id
  [TESLA_CLIENT_SECRET]=tesla-client-secret
  [TESLA_REFRESH_TOKEN]=tesla-refresh-token
  [INGEST_SHARED_SECRET]=ingest-shared-secret
  [PROXY_SHARED_SECRET]=proxy-shared-secret
)

seeded=0
skipped=0

for env_file in "${ENV_FILES[@]}"; do
  if [[ ! -f "$env_file" ]]; then
    echo "note: $env_file not found, skipping" >&2
    continue
  fi

  echo "Reading $env_file"

  while IFS='=' read -r key value; do
    # Skip comments and blanks.
    [[ -z "${key// }" || "$key" == \#* ]] && continue

    key="${key// }"
    secret_name="${SECRET_NAMES[$key]:-}"

    if [[ -z "$secret_name" ]]; then
      echo "  ? $key has no Key Vault mapping, ignoring"
      continue
    fi

    if [[ -z "${value// }" ]]; then
      echo "  - $key is empty, leaving $secret_name untouched"
      ((skipped++)) || true
      continue
    fi

    az keyvault secret set \
      --vault-name "$VAULT" \
      --name "$secret_name" \
      --value "$value" \
      --output none

    echo "  + $secret_name"
    ((seeded++)) || true
  done < "$env_file"
done

echo
echo "Seeded $seeded secret(s), skipped $skipped empty value(s) in vault '$VAULT'."

if [[ $skipped -gt 0 ]]; then
  echo "Empty values are expected before the one-time OAuth flows have been completed."
  echo "See docs/SETUP.md."
fi
