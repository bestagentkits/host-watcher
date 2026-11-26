#!/bin/sh
# Pre-command check for agents or scripts without a hook system.
# Prints the verdict line and exits 0 for go/throttle/unknown, 3 for clean/hold,
# so a wrapper can decide to wait. Uses the watcher snapshot when fresh.
#   sh hooks/generic/host-pressure-check.sh && npm run build
DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
PY=$(command -v python3 || command -v python)
[ -n "$PY" ] || { echo "host-pressure-check: python not found; skipping" >&2; exit 0; }
verdict=$("$PY" "$DIR/scripts/host_status.py" --json --samples 1 | "$PY" -c 'import json,sys; print(json.load(sys.stdin)["data"]["verdict"])') || exit 0
echo "host verdict: $verdict"
case "$verdict" in clean|hold) exit 3 ;; *) exit 0 ;; esac
