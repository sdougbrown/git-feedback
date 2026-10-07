#!/usr/bin/env bash
# watch-loop.sh — monitor loop wrapper: run wait, ack delivered events, respawn.
#
# Usage:
#   watch-loop.sh <URL> --consumer <NAME> [--account <LOGIN>] [--state-dir <DIR>] [--timeout 30m] [wait flags...]
#
# <URL> must be the first argument. That is this wrapper's rule, not the
# CLI's — the CLI accepts both URL-first and flag-first orders — because
# the wrapper hoists --consumer, --account, and --state-dir out of the
# passthrough so that wait and ack agree on the same consumer and store.
# [wait flags...] after the URL reach wait only.
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

usage() {
  cat <<'EOF'
watch-loop.sh — monitor loop wrapper: run wait, ack delivered events, respawn.

Usage:
  watch-loop.sh <URL> --consumer <NAME> [--account <LOGIN>] [--state-dir <DIR>] [--timeout 30m] [wait flags...]

<URL> must be the first argument (this wrapper's rule; the CLI accepts both
orders). --consumer, --account, and --state-dir are shared: they are passed
to both wait and ack. [wait flags...] reach wait only.
EOF
}

URL=""
CONSUMER=""
ACCOUNT=""
STATE_DIR=""
FLAGS=()

[ $# -ge 1 ] || { echo "watch-loop: missing <URL>" >&2; exit 2; }
URL=$1; shift
[ "${URL#-}" = "$URL" ] || { echo "watch-loop: <URL> must be the first argument" >&2; exit 2; }

while [ $# -gt 0 ]; do
  case $1 in
    --consumer)
      [ $# -ge 2 ] || { echo "watch-loop: --consumer requires a value" >&2; exit 2; }
      [ -z "$CONSUMER" ] || { echo "watch-loop: --consumer given more than once" >&2; exit 2; }
      CONSUMER=$2; shift 2 ;;
    --consumer=*)
      [ -z "$CONSUMER" ] || { echo "watch-loop: --consumer given more than once" >&2; exit 2; }
      CONSUMER=${1#--consumer=}; shift ;;
    --account)
      [ $# -ge 2 ] || { echo "watch-loop: --account requires a value" >&2; exit 2; }
      [ -z "$ACCOUNT" ] || { echo "watch-loop: --account given more than once" >&2; exit 2; }
      ACCOUNT=$2; shift 2 ;;
    --account=*)
      [ -z "$ACCOUNT" ] || { echo "watch-loop: --account given more than once" >&2; exit 2; }
      ACCOUNT=${1#--account=}; shift ;;
    --state-dir)
      [ $# -ge 2 ] || { echo "watch-loop: --state-dir requires a value" >&2; exit 2; }
      [ -z "$STATE_DIR" ] || { echo "watch-loop: --state-dir given more than once" >&2; exit 2; }
      STATE_DIR=$2; shift 2 ;;
    --state-dir=*)
      [ -z "$STATE_DIR" ] || { echo "watch-loop: --state-dir given more than once" >&2; exit 2; }
      STATE_DIR=${1#--state-dir=}; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      FLAGS+=("$1")
      shift ;;
  esac
done

[ -n "$CONSUMER" ] || { echo "watch-loop: --consumer is required" >&2; exit 2; }

# Shared options: passed to both wait and ack so the two agree on the
# consumer and the store.
SHARED=()
[ -n "$ACCOUNT" ] && SHARED+=(--account "$ACCOUNT")
[ -n "$STATE_DIR" ] && SHARED+=(--state-dir "$STATE_DIR")

while true; do
  rc=0
  out=$(git-feedback wait "$URL" --consumer "$CONSUMER" "${SHARED[@]+${SHARED[@]}}" "${FLAGS[@]+${FLAGS[@]}}" --json) || rc=$?
  if [ "$rc" -ne 0 ]; then
    # Operational/usage failure: surface the envelope and stop the loop.
    # A signal kill produces no envelope; don't print a stray blank line.
    [ -n "$out" ] && printf '%s\n' "$out" >&2
    exit "$rc"
  fi
  status=$(printf '%s' "$out" | jq -r '.status')

  if [ "$status" = "events" ]; then
    # shellcheck disable=SC2207
    ids=($(printf '%s' "$out" | jq -r '.events[]?.id'))
    if [ "${#ids[@]}" -gt 0 ]; then
      ack_args=()
      for id in "${ids[@]}"; do ack_args+=(--event "$id"); done
      rc=0
      ack_out=$(git-feedback ack "$URL" --consumer "$CONSUMER" "${SHARED[@]+${SHARED[@]}}" "${ack_args[@]}" --json) || rc=$?
      if [ "$rc" -ne 0 ]; then
        # Unacked events replay on restart, so exiting is safe; the point
        # is a visible diagnostic.
        echo "watch-loop: ack failed" >&2
        [ -n "$ack_out" ] && printf '%s\n' "$ack_out" >&2
        exit "$rc"
      fi
    fi
  fi
done
