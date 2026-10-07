#!/usr/bin/env bash
# watch-loop.sh — monitor loop wrapper: run wait, ack delivered events, respawn.
#
# Usage:
#   watch-loop.sh <URL> --consumer <NAME> [--timeout 30m] [wait flags...]
#
# In monitor mode (docs/contract.md), `wait` delivers pending events and
# acknowledges nothing; the harness records delivery by calling `ack` with the
# delivered event IDs. Doing that by hand leaves a gap: acking without
# respawning stalls the loop until the next external trigger. This wrapper
# closes it — each iteration runs one bounded `wait`, acknowledges what it
# delivered, and loops. `status: "timeout"` is a normal outcome and simply
# starts the next iteration.
#
# Requires: git-feedback on PATH, jq.
set -euo pipefail

URL=""
CONSUMER=""
FLAGS=()

while [ $# -gt 0 ]; do
  case $1 in
    --consumer)
      [ $# -ge 2 ] || { echo "watch-loop: --consumer requires a value" >&2; exit 2; }
      CONSUMER=$2; shift 2 ;;
    -h|--help)
      sed -n '2,8p' "$0"; exit 0 ;;
    *)
      if [ -z "$URL" ] && [ "${1#-}" = "$1" ]; then
        URL=$1
      else
        FLAGS+=("$1")
      fi
      shift ;;
  esac
done

[ -n "$URL" ] || { echo "watch-loop: missing <URL>" >&2; exit 2; }
[ -n "$CONSUMER" ] || { echo "watch-loop: --consumer is required" >&2; exit 2; }

while true; do
  rc=0
  out=$(git-feedback wait "$URL" --consumer "$CONSUMER" "${FLAGS[@]+${FLAGS[@]}}" --json) || rc=$?
  if [ "$rc" -ne 0 ]; then
    # Operational/usage failure: surface the envelope and stop the loop.
    printf '%s\n' "$out" >&2
    exit "$rc"
  fi
  status=$(printf '%s' "$out" | jq -r '.status')

  if [ "$status" = "events" ]; then
    # shellcheck disable=SC2207
    ids=($(printf '%s' "$out" | jq -r '.events[]?.id'))
    if [ "${#ids[@]}" -gt 0 ]; then
      ack_args=()
      for id in "${ids[@]}"; do ack_args+=(--event "$id"); done
      git-feedback ack "$URL" --consumer "$CONSUMER" "${ack_args[@]}" --json >/dev/null
    fi
  fi
done
