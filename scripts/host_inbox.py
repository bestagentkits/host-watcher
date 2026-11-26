#!/usr/bin/env python3
"""Show (or --take) the watcher's notice for the current worktree.

Agent hooks call this to learn that this worktree's processes are part of
the host pressure. Prints nothing and exits 0 when there is no notice, so
it is safe in any hook.

  python scripts/host_inbox.py
  python scripts/host_inbox.py --take --json
"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from hostwatch import common, inbox  # noqa: E402


def main(argv=None):
    ap = argparse.ArgumentParser(description="Per-worktree host-pressure notice.")
    ap.add_argument("--take", action="store_true", help="delete the notice after printing it")
    ap.add_argument("--dir", default=os.getcwd(), help="directory inside the worktree (default: cwd)")
    ap.add_argument("--json", action="store_true", help="print JSON (null when no notice)")
    args = ap.parse_args(argv)
    p = common.paths()
    inbox.prune(p["inbox"])
    e = inbox.read(p["inbox"], common.worktree_root(args.dir), take=args.take)
    if args.json:
        print(json.dumps(e))
    elif e:
        procs = ", ".join("%s pid %d write %.1f MB/s" % (x["name"], x["pid"], x["write_mb_s"]) for x in e["processes"])
        print("host-watcher: %s (%s): %s [%s]" % (e["verdict"], e["sampled_at"], e["action"], procs))
    return 0


if __name__ == "__main__":
    sys.exit(main())
