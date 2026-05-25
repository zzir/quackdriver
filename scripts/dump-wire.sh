#!/usr/bin/env bash
# Capture wire-format golden fixtures into testdata/golden/.
#
# Prereqs:
#   - Quack server running (use scripts/start-quack-server.sh in another terminal)
#   - Go toolchain on PATH
#
# The captured files are tracked in git as immutable test inputs. Re-run only
# when the wire format intentionally changes (server version bump, new fixture
# added to cmd/dump-wire). Diffs in existing files are protocol drift and
# should be investigated, not silently accepted.
set -euo pipefail

cd "$(dirname "$0")/.."
exec go run ./cmd/dump-wire "$@"
