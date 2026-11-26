"""Per-worktree inbox: the watcher leaves a notice for the agent working there."""

from __future__ import annotations

import json
import os
import time

from . import common

REWRITE_AFTER_S = 5 * 60
MAX_AGE_S = 10 * 60
SEVERITY = {"throttle": 1, "clean": 2, "hold": 3}
ACTIONS = {
    "hold": "Pause heavy builds and tests in this worktree until host status reports go; run them through "
            "`host-watcher gate -- <cmd>` (or `python scripts/host_gate.py -- <cmd>`).",
    "clean": "Free disk space before heavy work: check `host-watcher caches` (or `python scripts/cache_cleanup.py`) "
             "and clean caches you own.",
    "throttle": "Reduce parallel jobs in this worktree and run heavy commands through `host-watcher gate -- <cmd>` "
                "(or `python scripts/host_gate.py -- <cmd>`).",
}


def inbox_path(inbox_dir, worktree):
    return os.path.join(inbox_dir, common.worktree_hash(worktree) + ".json")


def _read(path):
    try:
        with open(path, encoding="utf-8") as fh:
            e = json.load(fh)
        return e if isinstance(e, dict) else None
    except (OSError, ValueError):
        return None


def _stale(e, now):
    at = common.parse_iso(e.get("sampled_at"))
    return at is None or now - at > MAX_AGE_S


def prune(inbox_dir, now=None):
    now = now if now is not None else time.time()
    try:
        names = os.listdir(inbox_dir)
    except OSError:
        return
    for name in names:
        if name.endswith(".json"):
            p = os.path.join(inbox_dir, name)
            e = _read(p)
            if e is None or _stale(e, now):
                try:
                    os.remove(p)
                except OSError:
                    pass


def read(inbox_dir, worktree, now=None, take=False):
    """Fresh entry for worktree or None. take=True deletes it after reading."""
    if not worktree:
        return None
    now = now if now is not None else time.time()
    p = inbox_path(inbox_dir, worktree)
    e = _read(p)
    if e is None:
        return None
    if _stale(e, now) or take:
        try:
            os.remove(p)
        except OSError:
            pass
        if _stale(e, now):
            return None
    return e


def update(inbox_dir, report, now=None):
    """Write one notice per worktree seen in top_io when the verdict is throttle, clean or hold."""
    now = now if now is not None else time.time()
    prune(inbox_dir, now)
    sev = SEVERITY.get(report.get("verdict"), 0)
    if not sev:
        return
    by_wt = {}
    for p in report.get("top_io") or []:
        if p.get("worktree"):
            by_wt.setdefault(p["worktree"], []).append(
                {"pid": p["pid"], "name": p["name"], "write_mb_s": p["write_mb_s"], "read_mb_s": p["read_mb_s"]})
    for wt, plist in by_wt.items():
        path = inbox_path(inbox_dir, wt)
        old = _read(path)
        if old:
            at = common.parse_iso(old.get("sampled_at"))
            if at is not None and old.get("severity", 0) >= sev and now - at < REWRITE_AFTER_S:
                continue
        entry = {"worktree": wt, "verdict": report["verdict"],
                 "sampled_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now)), "processes": plist,
                 "action": ACTIONS[report["verdict"]], "severity": sev}
        if report.get("io_scope"):
            entry["io_scope"] = report["io_scope"]
        common.write_atomic(path, json.dumps(entry, indent=2) + "\n")
