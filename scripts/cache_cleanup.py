#!/usr/bin/env python3
"""Cache inventory and safe cleanup. Dry-run by default.

Without arguments: measure known build/package caches and print the native
command that prunes each one. Shared caches are never deleted by this tool.

With --glob: list temp directories whose name matches the pattern and that
are safe to delete. A directory is only a candidate when ALL hold:
  - it is a plain directory (not a symlink or junction),
  - it belongs to the current user,
  - the process list was readable and no running process names it,
  - nothing in it changed for --min-age minutes (default 60).
Everything else is listed under "skipped" with the reason. --apply deletes
the candidates; without it nothing is removed.

  python scripts/cache_cleanup.py
  python scripts/cache_cleanup.py --dry-run --glob 'go-build*' --glob 'vitest-*'
  python scripts/cache_cleanup.py --glob 'my-agent-run-*' --apply
"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from hostwatch import caches  # noqa: E402


def too_broad(glob):
    """A pattern must contain at least three literal characters."""
    return len([c for c in glob if c not in "*?[]!"]) < 3


def main(argv=None):
    ap = argparse.ArgumentParser(description="Cache inventory and safe, dry-run-first temp-dir cleanup.")
    ap.add_argument("--glob", action="append", default=[], help="temp dir name pattern to clean (repeatable)")
    ap.add_argument("--root", action="append", default=[], help="where to look (default: the temp dir)")
    ap.add_argument("--min-age", type=float, default=60.0, help="minutes since last change (default 60)")
    ap.add_argument("--size", action="append", default=[], help="extra path to measure (repeatable)")
    ap.add_argument("--no-inventory", action="store_true", help="skip measuring caches")
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--dry-run", action="store_true", help="preview only (the default)")
    mode.add_argument("--apply", action="store_true", help="delete the listed candidates")
    ap.add_argument("--json", action="store_true", help="print JSON")
    args = ap.parse_args(argv)
    if args.apply and not args.glob:
        ap.error("--apply needs at least one --glob; shared caches are never deleted by this tool")
    bad = [g for g in args.glob if too_broad(g) or "/" in g or "\\" in g]
    if bad:
        ap.error("pattern too broad or not a bare name: %s" % ", ".join(bad))
    if args.min_age < 0:
        ap.error("--min-age must not be negative")

    out = {"mode": "apply" if args.apply else "dry-run"}
    if not args.no_inventory:
        out["caches"] = caches.inventory(args.size)
    if args.glob:
        found, skipped = caches.candidates(args.glob, args.root or None, args.min_age, caches.process_text())
        out["candidates"], out["skipped"] = found, skipped
        if args.apply:
            removed, failed = [], []
            for c in found:
                try:
                    caches.force_remove(c["path"])
                    removed.append(c["path"])
                except OSError as exc:
                    failed.append({"path": c["path"], "error": str(exc)[:200]})
            out.update(removed=removed, failed=failed,
                       freed_gb=round(sum(c["gb"] for c in found if c["path"] in removed), 3))
    if args.json:
        print(json.dumps(out, indent=2))
        return 0
    for c in out.get("caches", []):
        print("cache   %-14s %8.2f GB  %s%s" % (c["name"], c["gb"], c["path"],
                                                 "  (prune: %s)" % c["prune_with"] if c["prune_with"] else ""))
    for c in out.get("candidates", []):
        print("%s %s  %.3f GB, idle %d min" % ("removed" if c["path"] in out.get("removed", []) else "would remove",
                                               c["path"], c["gb"], c["age_min"]))
    for s in out.get("skipped", []):
        print("skip    %s  (%s)" % (s["path"], s["reason"]))
    for f in out.get("failed", []):
        print("failed  %s  (%s)" % (f["path"], f["error"]))
    if args.glob and not args.apply:
        print("dry run: nothing was deleted; rerun with --apply to remove the candidates above")
    return 0


if __name__ == "__main__":
    sys.exit(main())
