#!/usr/bin/env bash
# Start a local DuckDB server with the Quack extension enabled.
#
# The server runs in-process inside the DuckDB CLI; quack_serve() returns
# immediately, so we keep the CLI process alive by piping /dev/null into its
# stdin. Ctrl-C (or SIGTERM) on the script tears it down.
#
# Env vars (all optional):
#   QUACK_HOST              default 127.0.0.1
#   QUACK_PORT              default 9494
#   QUACK_TOKEN             default super_secret
#   QUACK_LOG               non-empty enables Quack wire logging on the server
#   QUACK_EXTENSION_REPO    default core_nightly; empty = stable repo
#   DUCKDB_BIN              default duckdb (on PATH)
set -euo pipefail

host="${QUACK_HOST:-127.0.0.1}"
port="${QUACK_PORT:-9494}"
token="${QUACK_TOKEN:-super_secret}"
repo="${QUACK_EXTENSION_REPO-core_nightly}"
bin="${DUCKDB_BIN:-duckdb}"

if ! command -v "${bin}" >/dev/null 2>&1; then
  echo "${bin} not found on PATH. Install DuckDB v1.5.3+ first." >&2
  exit 1
fi

if [ "${#token}" -lt 4 ]; then
  echo "QUACK_TOKEN must be at least 4 characters." >&2
  exit 1
fi

if [ -n "${repo}" ]; then
  install_sql="INSTALL quack FROM ${repo};"
else
  install_sql="INSTALL quack;"
fi
cmd="${install_sql} LOAD quack;"
if [[ -n "${QUACK_LOG:-}" ]]; then
  cmd+=" CALL enable_logging('Quack');"
fi
cmd+=" CALL quack_serve('quack:${host}:${port}', token=>'${token}');"

echo "Starting Quack server on quack:${host}:${port} (token=${token})" >&2
exec tail -f /dev/null | "${bin}" -init /dev/null -cmd "${cmd}"
