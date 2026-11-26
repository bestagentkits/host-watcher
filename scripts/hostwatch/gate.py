"""Resource-aware command execution: wait out hold/clean, then run at low priority."""

from __future__ import annotations

import os
import platform
import shutil
import signal
import subprocess
import sys
import time

from . import common, inbox, report

BACKOFF_S = [15, 30, 60]  # last value repeats
BLOCKING = ("hold", "clean")


def current_status(cfg):
    p = common.paths()
    r = report.read_snapshot(p["snapshot"])
    return r if r is not None else report.collect(cfg, samples=3, interval=1.0)


def detail(r):
    out = " (%s)" % "; ".join(r["reasons"]) if r.get("reasons") else ""
    top = (r.get("top_io") or [None])[0]
    if top:
        out += "; top I/O %s pid %d write %.1f MB/s" % (top["name"], top["pid"], top["write_mb_s"])
        if top.get("worktree"):
            out += " in " + top["worktree"]
    if r.get("verdict") == "clean":
        out += "; free space: see `python scripts/cache_cleanup.py`"
    return out


def wait_for_host(status, max_wait, sleep=time.sleep, now=time.monotonic, err=sys.stderr):
    """Block while the verdict is hold or clean, up to max_wait seconds. Never fails."""
    if max_wait == 0:
        return
    start = now()
    for attempt in range(10 ** 6):
        try:
            r = status()
        except Exception as exc:  # an unreadable status never blocks
            err.write("host_gate: warning: host status unavailable (%s); running without waiting\n" % exc)
            return
        if r.get("verdict") not in BLOCKING:
            return
        waited = now() - start
        remaining = max_wait - waited
        if remaining <= 0:
            err.write("host_gate: warning: still %s after waiting %ds (--max-wait); running anyway%s\n"
                      % (r["verdict"], waited, detail(r)))
            return
        pause = min(BACKOFF_S[min(attempt, len(BACKOFF_S) - 1)], remaining)
        err.write("host_gate: %s%s; waiting %ds (%ds of %ds used); skip with --max-wait 0\n"
                  % (r["verdict"], detail(r), pause, waited, max_wait))
        sleep(pause)


def take_own_notice(err=sys.stderr):
    p = common.paths()
    inbox.prune(p["inbox"])
    e = inbox.read(p["inbox"], common.worktree_root(os.getcwd()), take=True)
    if e:
        err.write("host_gate: notice for this worktree (%s, %s): %s\n" % (e["verdict"], e["sampled_at"], e["action"]))


def low_priority_spawn(argv):
    """(argv, popen kwargs, warnings) to start argv at low CPU and I/O priority."""
    kwargs, warnings = {}, []
    system = platform.system()
    if system == "Windows":
        kwargs["creationflags"] = 0x00004000  # BELOW_NORMAL_PRIORITY_CLASS, inherited by children
    elif system == "Darwin":
        if os.path.exists("/usr/sbin/taskpolicy"):
            argv = ["/usr/sbin/taskpolicy", "-b"] + argv
        else:
            kwargs["preexec_fn"] = lambda: os.nice(10)
    else:
        ionice = shutil.which("ionice")
        if ionice:
            argv = [ionice, "-c", "3"] + argv
        else:
            warnings.append("ionice not found; running with CPU nice only")
        kwargs["preexec_fn"] = lambda: os.nice(10)
    return argv, kwargs, warnings


def run_child(argv, low_priority=True, err=sys.stderr):
    """Run argv with this process's stdio; forward SIGINT/SIGTERM; return its exit code."""
    kwargs = {}
    resolved = shutil.which(argv[0])
    if resolved is None:  # resolve before any priority wrapper, so "not found" is ours to report
        raise FileNotFoundError("command not found: %s" % argv[0])
    argv = [resolved] + list(argv[1:])
    if low_priority:
        argv, kwargs, warnings = low_priority_spawn(list(argv))
        for w in warnings:
            err.write("host_gate: warning: %s\n" % w)
    child = subprocess.Popen(argv, **kwargs)  # never through a shell
    forwarded = []

    def forward(signum, _frame):
        forwarded.append(signum)
        try:
            child.send_signal(signum)
        except OSError:
            pass

    old = {}
    for name in ("SIGINT", "SIGTERM", "SIGHUP"):
        sig = getattr(signal, name, None)
        if sig is not None:
            try:
                old[sig] = signal.signal(sig, forward)
            except (ValueError, OSError):
                pass
    try:
        code = child.wait()
    finally:
        for sig, handler in old.items():
            signal.signal(sig, handler)
    return 128 - code if code < 0 else code  # POSIX: killed by signal N -> 128+N
