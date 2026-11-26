#!/usr/bin/env python3
"""Generic, Node-free host-pressure hook for any coding agent.

Reads an optional JSON payload on stdin ({"cwd": ..., "session_id": ...},
the shape Claude Code, Codex and many other agents pass to hooks). It never
samples (hooks must be fast): it reads the watcher snapshot and this
worktree's inbox notice, and prints a reminder only when the verdict is
throttle, clean or hold. It always exits 0 and never blocks the agent.

Output formats (--format):
  claude  {"continue": true, "hookSpecificOutput": {...additionalContext}}  (default)
  text    plain text on stdout, empty when there is nothing to say

  echo '{"cwd": "'$PWD'"}' | python hooks/generic/host_pressure_hook.py --format text
"""

import argparse
import json
import os
import hashlib
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "..", "scripts"))

try:
    from hostwatch import common, inbox, report  # noqa: E402
except ImportError:  # copied elsewhere without scripts/: stay silent
    common = None

ADVICE = {
    "throttle": "Avoid running several builds/tests/installs at once; run heavy commands one at a time.",
    "clean": "Disk space is low. Do not start new builds/tests/installs; free space from caches you created.",
    "hold": "Do not start new builds/tests/installs now; queue them and prefer read-only work meanwhile.",
}


DEDUPE_S = 5 * 60
RANK = {"throttle": 1, "clean": 2, "hold": 3}


def should_remind(session_id, verdict, now=None):
    """Once per verdict per session, again when it gets worse or after 5 minutes."""
    now = now if now is not None else time.time()
    key = hashlib.sha256(str(session_id or "default").encode()).hexdigest()[:16]
    path = os.path.join(tempfile.gettempdir(), "host-watcher-hook-%s.json" % key)
    try:
        with open(path, encoding="utf-8") as fh:
            last = json.load(fh)
        if RANK.get(verdict, 0) <= RANK.get(last.get("verdict"), 0) and now - last.get("at", 0) < DEDUPE_S:
            return False
    except (OSError, ValueError, AttributeError):
        pass
    try:
        with open(path, "w", encoding="utf-8") as fh:
            json.dump({"verdict": verdict, "at": now}, fh)
    except OSError:
        pass
    return True


def message(payload):
    if common is None:
        return ""
    p = common.paths()
    cwd = payload.get("cwd") or os.getcwd()
    parts = []
    note = inbox.read(p["inbox"], common.worktree_root(cwd), take=True)
    if note:
        procs = ", ".join("%s pid %d" % (x["name"], x["pid"]) for x in note.get("processes", []))
        parts.append("Host watcher notice for this worktree (%s): your processes %s. %s"
                     % (note["verdict"], procs, note["action"]))
    snap = report.read_snapshot(p["snapshot"], max_age=180)
    if snap and snap.get("verdict") in ADVICE and should_remind(payload.get("session_id"), snap["verdict"]):
        reasons = "; ".join(snap.get("reasons", [])[:2])
        parts.append("Machine pressure: %s%s. %s Run heavy commands through `python scripts/host_gate.py -- <cmd>` "
                     "(or `host-watcher gate -- <cmd>`). Keep the full task scope; this only paces heavy commands."
                     % (snap["verdict"], " (%s)" % reasons if reasons else "", ADVICE[snap["verdict"]]))
    return "\n".join(parts)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--format", choices=("claude", "text"), default="claude")
    ap.add_argument("--event", default="", help="hookEventName to echo back (default: from payload)")
    args = ap.parse_args()
    if os.environ.get("HOST_WATCHER_HOOK", "").lower() in ("off", "0", "false"):
        text, payload = "", {}
    else:
        try:
            payload = json.loads(sys.stdin.read() or "{}") if not sys.stdin.isatty() else {}
            payload = payload if isinstance(payload, dict) else {}
        except ValueError:
            payload = {}
        try:
            text = message(payload)
        except Exception:  # a hook must never break the agent
            text = ""
    if args.format == "text":
        if text:
            print(text)
        return 0
    out = {"continue": True}
    if text:
        event = args.event or payload.get("hook_event_name") or "PostToolUse"
        out["hookSpecificOutput"] = {"hookEventName": event, "additionalContext": text}
    print(json.dumps(out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
