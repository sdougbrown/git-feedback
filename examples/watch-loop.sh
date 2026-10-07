#!/usr/bin/env bash
# watch-loop.sh — monitor loop wrapper: run wait, ack delivered events, respawn.
#
# Usage:
#   watch-loop.sh <URL> --consumer <NAME> [--timeout 30m] [wait flags...]
#
# <URL> must be the first argument; valued flags after it pass through to
# wait. Flags before the URL would have their values collide with positional
# parsing, so that ordering is rejected.
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

[ $# -ge 1 ] || { echo "watch-loop: missing <URL>" >&2; exit 2; }
URL=$1; shift
[ "${URL#-}" = "$URL" ] || { echo "watch-loop: <URL> must be the first argument" >&2; exit 2; }

while [ $# -gt 0 ]; do
  case $1 in
    --consumer)
      [ $# -ge 2 ] || { echo "watch-loop: --consumer requires a value" >&2; exit 2; }
      CONSUMER=$2; shift 2 ;;
    -h|--help)
      sed -n '2,8p' "$0"; exit 0 ;;
    *)
      FLAGS+=("$1")
      shift ;;
  esac
done

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
