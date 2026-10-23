#!/usr/bin/env bash
# Run a command against a Quack server on QUACK_HOST:QUACK_PORT (default
# 127.0.0.1:9494). A server already answering there is used as is; otherwise
# start-quack-server.sh is started in the background (output in SERVER_LOG,
# default /tmp/quack-server.log) and stopped when the command ends.
#
#   scripts/with-server.sh go run ./cmd/probe-fetch -rows 1000000
#
# QUACK_START_TIMEOUT (default 60) is the seconds to wait for a started
# server; the first start may download the extension.
set -euo pipefail

host="${QUACK_HOST:-127.0.0.1}"
port="${QUACK_PORT:-9494}"
log="${SERVER_LOG:-/tmp/quack-server.log}"
timeout="${QUACK_START_TIMEOUT:-60}"

# A Quack server answers GET / with 200.
up() { curl -fsS -o /dev/null --max-time 2 --noproxy '*' "http://${host}:${port}/" 2>/dev/null; }

if ! up; then
  echo "==> starting a Quack server on ${host}:${port} (log: ${log})" >&2
  bash "$(dirname "$0")/start-quack-server.sh" >"${log}" 2>&1 &
  pid=$!
  # Stop the server when the command ends, keeping the command's exit status.
  trap 'rc=$?; kill "${pid}" 2>/dev/null; wait "${pid}" 2>/dev/null || true; exit "${rc}"' EXIT
  for _ in $(seq 1 $((timeout * 4))); do
    up && break
    if ! kill -0 "${pid}" 2>/dev/null; then
      echo "the Quack server exited during startup; tail of ${log}:" >&2
      tail -20 "${log}" >&2
      exit 1
    fi
    sleep 0.25
  done
  if ! up; then
    echo "the Quack server did not answer within ${timeout}s; tail of ${log}:" >&2
    tail -20 "${log}" >&2
    exit 1
  fi
fi

"$@"
