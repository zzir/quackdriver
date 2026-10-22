#!/usr/bin/env bash
# Run a local Quack server: the DuckDB CLI v2.0.0+ (Quack protocol v3) with
# the quack extension, serving in the foreground until Ctrl-C or SIGTERM.
#
# Env vars (all optional):
#   QUACK_HOST, QUACK_PORT   default 127.0.0.1:9494 (local hosts speak plain HTTP)
#   QUACK_TOKEN              default super_secret
#   DUCKDB_BIN               default duckdb (on PATH)
#   QUACK_TEST_INIT_SQL      statements run before quack_serve, replacing the
#                            default "INSTALL quack; LOAD quack;" (the same
#                            variable internal/livetest honours), e.g.
#                            "INSTALL quack FROM core_nightly; LOAD quack;" or
#                            "SET force_mbedtls_unsafe = 'true'; INSTALL quack; LOAD quack;"
#                            for a CLI without httpfs. Append
#                            "CALL enable_logging('Quack', storage => 'shell_log_storage');"
#                            to print every Quack request.
set -euo pipefail

host="${QUACK_HOST:-127.0.0.1}"
port="${QUACK_PORT:-9494}"
token="${QUACK_TOKEN:-super_secret}"
bin="${DUCKDB_BIN:-duckdb}"
init="${QUACK_TEST_INIT_SQL:-INSTALL quack; LOAD quack;}"

version="$("${bin}" --version 2>/dev/null)" || {
  echo "'${bin} --version' failed: install the DuckDB CLI v2.0.0+ or set DUCKDB_BIN." >&2
  exit 1
}
if ! [[ "${version}" =~ ^v?([0-9]+)\. ]] || [ "${BASH_REMATCH[1]}" -lt 2 ]; then
  echo "DuckDB ${version} is too old: Quack protocol v3 needs the DuckDB CLI v2.0.0+." >&2
  exit 1
fi

# sql_quote renders $1 as a SQL string literal.
sql_quote() { printf "'%s'" "${1//\'/\'\'}"; }
sql="${init} CALL quack_serve($(sql_quote "quack:${host}:${port}"), token => $(sql_quote "${token}"));"

echo "Quack server on quack:${host}:${port} (DuckDB ${version}, token ${token})" >&2

# quack_serve returns at once and the CLI keeps serving while it reads stdin,
# so stdin is a FIFO whose write end this script holds open: closing it on
# Ctrl-C or SIGTERM gives the CLI EOF, and it exits cleanly.
dir="$(mktemp -d "${TMPDIR:-/tmp}/quack-server.XXXXXX")"
mkfifo "${dir}/stdin"
"${bin}" -no-init -no-agent -cmd "${sql}" <"${dir}/stdin" &
pid=$!
exec 3>"${dir}/stdin"
rm -rf "${dir}"

trap 'exec 3>&-' INT TERM
wait "${pid}" || rc=$?
trap - INT TERM
if kill -0 "${pid}" 2>/dev/null; then # the first wait was interrupted by the signal
  wait "${pid}" || rc=$?
fi
exit "${rc:-0}"
