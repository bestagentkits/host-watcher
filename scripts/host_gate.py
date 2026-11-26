#!/usr/bin/env python3
"""Run a heavy command only when the host can take it.

While the verdict is hold or clean it waits (15 s, 30 s, then 60 s backoff)
up to --max-wait seconds, then runs anyway so a script never hangs.
throttle, go and unknown never block. The command runs directly (no shell)
at low priority, with this terminal's stdio, and its exit code is passed on.

  python scripts/host_gate.py -- npm run build
  python scripts/host_gate.py --max-wait 600 -- go test ./...
  python scripts/host_gate.py --max-wait 0 --no-low-priority -- make lint

Exit codes: the command's own; 2 bad arguments; 5 command could not start
or the wait was interrupted before it started.
"""

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from hostwatch import common, gate  # noqa: E402

EXIT_START = 5


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    cmd = []
    if "--" in argv:
        i = argv.index("--")
        argv, cmd = argv[:i], argv[i + 1:]
    ap = argparse.ArgumentParser(description="Wait out a saturated host, then run a command at low priority.",
                                 usage="%(prog)s [--max-wait S] [--no-low-priority] -- <cmd> [args...]")
    ap.add_argument("--max-wait", type=float, default=1800.0, help="seconds to wait before running anyway; 0 runs at once")
    ap.add_argument("--no-low-priority", action="store_true", help="run at normal priority")
    ap.add_argument("--config", help="JSON thresholds file")
    args = ap.parse_args(argv)
    if not cmd:
        ap.error("no command given after --")
    if args.max_wait < 0:
        ap.error("--max-wait must not be negative")
    try:
        cfg = common.load_config(args.config)
    except ValueError as exc:
        ap.error(str(exc))
    gate.take_own_notice()
    try:
        gate.wait_for_host(lambda: gate.current_status(cfg), args.max_wait)
    except KeyboardInterrupt:
        print("host_gate: interrupted while waiting; the command was not started", file=sys.stderr)
        return EXIT_START
    try:
        return gate.run_child(cmd, low_priority=not args.no_low_priority)
    except OSError as exc:
        print("host_gate: could not start %s: %s" % (cmd[0], exc), file=sys.stderr)
        return EXIT_START


if __name__ == "__main__":
    sys.exit(main())
