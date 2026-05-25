#!/usr/bin/env bash
# Run each example listed as a positional argument. Fail-fast.
#
#   scripts/run-examples.sh basic types nested
set -euo pipefail

QUACK_TOKEN="${QUACK_TOKEN:-super_secret}"
export QUACK_TOKEN

for ex in "$@"; do
  echo "==> examples/$ex"
  go run "./examples/$ex"
done
