#!/usr/bin/env bash
# Run a command with a running Quack server.
#
# Behavior:
#   - If a server is already listening on QUACK_PORT (default 9494), use it.
#   - Otherwise start one in the background using start-quack-server.sh,
#     remember that *we* started it, and tear it down on exit.
#   - Always restores the original state (won't kill someone else's server).
#
# Usage:
#   scripts/with-server.sh <command> [args...]
#
# Env vars (all optional):
#   QUACK_HOST, QUACK_PORT, QUACK_TOKEN — forwarded to start-quack-server.sh
#   QUACK_EXTENSION_REPO                — extension repo override (default empty)
#   SERVER_LOG                          — log file (default /tmp/quack-server.log)
set -euo pipefail

host="${QUACK_HOST:-127.0.0.1}"
port="${QUACK_PORT:-9494}"
log_file="${SERVER_LOG:-/tmp/quack-server.log}"

owned=0

cleanup() {
  local rc=$?
  if [ "$owned" = 1 ]; then
    echo "==> stopping quack server" >&2
    lsof -ti ":$port" 2>/dev/null | xargs kill 2>/dev/null || true
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

if ! lsof -ti ":$port" >/dev/null 2>&1; then
  echo "==> starting quack server (log: $log_file)" >&2
  QUACK_HOST="$host" QUACK_PORT="$port" \
    QUACK_EXTENSION_REPO="${QUACK_EXTENSION_REPO-}" \
    bash "$(dirname "$0")/start-quack-server.sh" >"$log_file" 2>&1 &
  owned=1
  for _ in $(seq 1 40); do
    if lsof -ti ":$port" >/dev/null 2>&1; then break; fi
    sleep 0.25
  done
  if ! lsof -ti ":$port" >/dev/null 2>&1; then
    echo "server failed to start within 10s; tail of $log_file:" >&2
    tail -20 "$log_file" >&2 || true
    exit 1
  fi
fi

"$@"
