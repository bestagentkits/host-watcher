#!/bin/sh
# Run the same test command in several git worktrees without saturating the host.
# Each run waits while the host is in hold/clean, then runs at low priority.
#   sh examples/parallel-worktrees.sh ../wt-a ../wt-b ../wt-c -- npm test
HW=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
PY=$(command -v python3 || command -v python)
dirs=""
while [ $# -gt 0 ] && [ "$1" != "--" ]; do dirs="$dirs $1"; shift; done
[ "$1" = "--" ] && shift
[ $# -gt 0 ] || { echo "usage: $0 <worktree>... -- <cmd> [args...]" >&2; exit 2; }
"$PY" "$HW/scripts/host_watch.py" --detach >/dev/null   # optional: shared snapshot for all runs
status=0
for d in $dirs; do
  ( cd "$d" && "$PY" "$HW/scripts/host_gate.py" --max-wait 900 -- "$@" ) &
  sleep 5   # stagger starts so the gate sees the previous job's load
done
for job in $(jobs -p); do wait "$job" || status=1; done
exit $status
