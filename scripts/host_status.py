#!/usr/bin/env python3
"""Print one host-pressure verdict: go, throttle, unknown, clean or hold.

Samples CPU, memory, the busiest physical disk and free space (median of
--samples). Reuses the watcher's snapshot when it is under 15 s old.
Standard library only. Every verdict exits 0; bad arguments exit 2.

  python scripts/host_status.py
  python scripts/host_status.py --json
  python scripts/host_status.py --caches --min-free-gb 50 --path /data
"""

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from hostwatch import common, inbox, report  # noqa: E402


def main(argv=None):
    ap = argparse.ArgumentParser(description="Host pressure verdict for parallel coding agents.")
    ap.add_argument("--json", action="store_true", help="print the JSON envelope")
    ap.add_argument("--samples", type=int, default=3, help="sampling intervals, 1-30 (default 3)")
    ap.add_argument("--interval", type=float, default=1.0, help="seconds per interval, 0.1-60 (default 1)")
    ap.add_argument("--fresh", action="store_true", help="ignore the watcher snapshot and sample now")
    ap.add_argument("--caches", action="store_true", help="also measure known build/package caches (walks trees)")
    ap.add_argument("--path", action="append", default=[], help="volume to check for free space (repeatable)")
    ap.add_argument("--min-free-gb", type=float, help="free-space floor in GB (default 25)")
    ap.add_argument("--config", help="JSON thresholds file (default $HOST_WATCHER_HOME/config.json)")
    ap.add_argument("--write-cache", help="also write the JSON envelope atomically to this file")
    args = ap.parse_args(argv)
    if not 1 <= args.samples <= 30 or not 0.1 <= args.interval <= 60:
        ap.error("--samples must be 1-30 and --interval 0.1-60")
    try:
        cfg = common.load_config(args.config)
    except ValueError as exc:
        ap.error(str(exc))
    if args.min_free_gb is not None:
        if args.min_free_gb < 0:
            ap.error("--min-free-gb must not be negative")
        cfg["min_free_gb"] = args.min_free_gb

    p = common.paths()
    r = None
    if not (args.fresh or args.caches or args.path or args.min_free_gb is not None or args.config):
        r = report.read_snapshot(p["snapshot"])
        if r is not None:
            report.add_caller_space(r, os.getcwd(), cfg)
    if r is None:
        r = report.collect(cfg, args.samples, args.interval, args.path or None, args.caches)
    r["snapshot_path"] = p["snapshot"]
    inbox.prune(p["inbox"])
    r["inbox"] = inbox.read(p["inbox"], common.worktree_root(os.getcwd()))
    if args.write_cache:
        common.write_atomic(args.write_cache, common.envelope(common.SNAPSHOT_KIND, r))
    sys.stdout.write(common.envelope(common.SNAPSHOT_KIND, r) if args.json else report.render(r))
    return 0


if __name__ == "__main__":
    sys.exit(main())
