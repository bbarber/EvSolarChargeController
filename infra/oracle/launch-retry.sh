#!/usr/bin/env bash
# Retries the instance launch until Oracle has A1 capacity.
#
# Always Free A1 capacity in a given region is claimed as soon as it is released, so a first
# launch attempt failing with "Out of host capacity" says nothing about whether the shape is
# obtainable — it usually just means someone else got there first. Cycling the availability
# domains and retrying is the normal way to get one.
#
# Stops immediately on any error that is not a capacity error, because those do not resolve by
# waiting: a 404 means the shape is not offered in the region at all, and retrying it for hours
# would just hide the real problem.
#
# Usage: ./launch-retry.sh          # 12 hours, 60s between attempts
#        MAX_SECONDS=3600 SLEEP_SECONDS=30 ./launch-retry.sh

set -uo pipefail
cd "$(dirname "$0")" || exit 1

ADS=(
  "GoOx:US-CHICAGO-1-AD-1"
  "GoOx:US-CHICAGO-1-AD-2"
  "GoOx:US-CHICAGO-1-AD-3"
)

MAX_SECONDS="${MAX_SECONDS:-43200}"
SLEEP_SECONDS="${SLEEP_SECONDS:-60}"
LOG=/tmp/oci-launch-attempt.log
DEADLINE=$(( $(date +%s) + MAX_SECONDS ))
attempt=0

echo "$(date '+%F %T')  starting; deadline in $((MAX_SECONDS / 60)) minutes"

while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  for AD in "${ADS[@]}"; do
    attempt=$((attempt + 1))
    short="${AD##*-}"

    if terraform apply -input=false -auto-approve -var "availability_domain=$AD" >"$LOG" 2>&1; then
      echo "$(date '+%F %T')  attempt $attempt: LAUNCHED in AD-$short after $attempt tries"
      terraform output
      exit 0
    fi

    if grep -q "Out of host capacity" "$LOG"; then
      echo "$(date '+%F %T')  attempt $attempt: AD-$short out of capacity"
    else
      echo "$(date '+%F %T')  attempt $attempt: AD-$short failed for a non-capacity reason, stopping"
      grep -oE '[0-9]{3}-[A-Za-z]+, [^"]*' "$LOG" | head -3
      exit 1
    fi

    sleep "$SLEEP_SECONDS"
  done
done

echo "$(date '+%F %T')  deadline reached after $attempt attempts without capacity"
exit 2
