#!/usr/bin/env python3
"""Keep one machine-wide watcher writing the host snapshot and worktree inbox.

Foreground by default (Ctrl-C to stop). --detach starts it in the background,
--stop asks a running watcher to exit. It exits by itself after --idle-exit
seconds with no coding-agent process alive (0 disables). No daemon is
required: every other command samples directly when no watcher runs.

  python scripts/host_watch.py
  python scripts/host_watch.py --detach
  python scripts/host_watch.py --stop
"""

import argparse
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from hostwatch import common, report, watcher  # noqa: E402


def detach(p, args):
    pid = watcher.running_pid(p)
    if pid:
        print("host watcher already-running (pid %s)" % pid)
        return 0
    os.makedirs(p["dir"], exist_ok=True)
    cmd = [sys.executable, os.path.abspath(__file__), "--interval", str(args.interval), "--idle-exit", str(args.idle_exit)]
    if args.config:
        cmd += ["--config", os.path.abspath(args.config)]
    kwargs = {"cwd": p["dir"], "stdin": subprocess.DEVNULL, "stdout": subprocess.DEVNULL,
              "stderr": open(os.path.join(p["dir"], "watch.log"), "a")}  # noqa: SIM115
    if os.name == "nt":
        kwargs["creationflags"] = 0x00000008 | 0x00000200  # DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
    else:
        kwargs["start_new_session"] = True
    child = subprocess.Popen(cmd, **kwargs)
    deadline = time.monotonic() + args.interval + 15
    while time.monotonic() < deadline:
        if report.read_snapshot(p["snapshot"]) is not None:
            print("host watcher started (pid %d)\n  snapshot %s" % (child.pid, p["snapshot"]))
            return 0
        if child.poll() is not None:
            other = watcher.running_pid(p)
            if other:
                print("host watcher already-running (pid %s)" % other)
                return 0
            print("background watcher exited at once; see %s" % os.path.join(p["dir"], "watch.log"), file=sys.stderr)
            return 1
        time.sleep(0.2)
    print("host watcher running (pid %d); first snapshot not written yet" % child.pid)
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(description="Machine-wide host watcher (snapshot + worktree inbox).")
    ap.add_argument("--interval", type=float, default=5.0, help="seconds between snapshots, 1-300 (default 5)")
    ap.add_argument("--idle-exit", type=float, default=1800.0,
                    help="exit after this many seconds with no agent process alive; 0 never (default 1800)")
    ap.add_argument("--config", help="JSON thresholds file")
    g = ap.add_mutually_exclusive_group()
    g.add_argument("--detach", action="store_true", help="start in the background and return")
    g.add_argument("--stop", action="store_true", help="stop the running watcher")
    args = ap.parse_args(argv)
    if not 1 <= args.interval <= 300 or args.idle_exit < 0:
        ap.error("--interval must be 1-300 and --idle-exit >= 0")
    p = common.paths()
    if args.stop:
        try:
            pid = watcher.stop(p)
        except TimeoutError as exc:
            print(exc, file=sys.stderr)
            return 1
        print("host watcher stopped (pid %s)" % pid if pid else "host watcher not-running")
        return 0
    if args.detach:
        return detach(p, args)
    try:
        cfg = common.load_config(args.config)
    except ValueError as exc:
        ap.error(str(exc))
    try:
        print("host watcher running (pid %d); snapshot %s; Ctrl-C to stop" % (os.getpid(), p["snapshot"]), file=sys.stderr)
        reason = watcher.watch(p, cfg, args.interval, args.idle_exit)
    except watcher.Busy:
        print("host watcher already-running (pid %s); stop it with --stop" % watcher.running_pid(p))
        return 0
    print("host watcher exited (%s)" % reason, file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
