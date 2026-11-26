"""Singleton background watcher: writes the snapshot and inbox notices."""

from __future__ import annotations

import json
import os
import sys
import time

from . import common, inbox, procs, report


class Busy(Exception):
    """Another watcher holds the lock."""


class FileLock:
    """Non-blocking exclusive lock (flock / msvcrt), compatible with the Go binary.

    `supported` is False on filesystems that cannot lock; the watcher then
    runs unprotected and says so."""

    def __init__(self, path):
        self.path, self.fh, self.supported = path, None, True

    def acquire(self):
        os.makedirs(os.path.dirname(self.path), exist_ok=True)
        fh = open(self.path, "a+b")  # noqa: SIM115 - held for the watcher's lifetime
        try:
            if os.name == "nt":
                import msvcrt
                fh.seek(0)
                msvcrt.locking(fh.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            fh.close()
            raise Busy(self.path)
        except OSError as exc:
            import errno
            if exc.errno in (errno.EACCES, errno.EAGAIN, errno.EDEADLK if hasattr(errno, "EDEADLK") else -1):
                fh.close()
                raise Busy(self.path)
            if exc.errno in (errno.ENOLCK, errno.EOPNOTSUPP, errno.ENOSYS):
                self.supported = False
            else:
                fh.close()
                raise
        self.fh = fh
        return self

    def release(self):
        if self.fh:
            try:
                if os.name == "nt":
                    import msvcrt
                    self.fh.seek(0)
                    msvcrt.locking(self.fh.fileno(), msvcrt.LK_UNLCK, 1)
                else:
                    import fcntl
                    fcntl.flock(self.fh.fileno(), fcntl.LOCK_UN)
            except OSError:
                pass
            self.fh.close()
            self.fh = None


def running_pid(p):
    """PID of the live watcher, -1 if locked without pid file, 0 if none."""
    lock = FileLock(p["lock"])
    try:
        lock.acquire()
    except Busy:
        try:
            with open(p["pid"], encoding="utf-8") as fh:
                return int(json.load(fh).get("pid") or -1)
        except (OSError, ValueError, AttributeError):
            return -1
    except OSError:
        return 0
    lock.release()
    return 0


def publish(p, r, log=sys.stderr):
    r["source"], r["snapshot_path"] = "watcher", p["snapshot"]
    try:
        common.write_atomic(p["snapshot"], common.envelope(common.SNAPSHOT_KIND, r))
    except OSError as exc:
        log.write("warning: write %s: %s\n" % (p["snapshot"], exc))
    try:
        inbox.update(p["inbox"], r)
    except OSError as exc:
        log.write("warning: update inbox: %s\n" % exc)


def watch(p, cfg, interval=5.0, idle_exit=1800.0, collect=None, alive=None, log=sys.stderr, max_loops=None):
    """Run until stopped, interrupted or idle. Returns 'stopped', 'idle' or 'canceled'."""
    lock = FileLock(p["lock"]).acquire()  # raises Busy
    if not lock.supported:
        log.write("warning: %s cannot be locked; a second watcher would not be detected\n" % p["lock"])
    collect = collect or (lambda: report.collect(cfg, samples=1, interval=interval))
    alive = alive or procs.agent_alive
    try:
        if os.path.exists(p["stop"]):
            os.remove(p["stop"])
        common.write_atomic(p["pid"], json.dumps({"pid": os.getpid(), "started_at": common.now_iso()}))
        last_agent, loops = time.monotonic(), 0
        while True:
            if os.path.exists(p["stop"]):
                os.remove(p["stop"])
                return "stopped"
            start = time.monotonic()
            try:
                publish(p, collect(), log)
            except Exception as exc:  # keep watching; sampling problems are transient
                log.write("warning: sampling failed: %s\n" % exc)
            if alive() is not False:  # None (unreadable) never triggers idle exit
                last_agent = time.monotonic()
            if idle_exit and time.monotonic() - last_agent >= idle_exit:
                return "idle"
            loops += 1
            if max_loops and loops >= max_loops:
                return "stopped"
            rest = interval - (time.monotonic() - start)
            if rest > 0:
                time.sleep(rest)
    except KeyboardInterrupt:
        return "canceled"
    finally:
        try:
            os.remove(p["pid"])
        except OSError:
            pass
        lock.release()


def stop(p, timeout=20.0):
    """Ask the running watcher to exit via the stop sentinel. Returns its pid or 0."""
    pid = running_pid(p)
    if pid == 0:
        return 0
    os.makedirs(p["dir"], exist_ok=True)
    with open(p["stop"], "w", encoding="ascii") as fh:
        fh.write(str(pid))
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if running_pid(p) == 0:
            return pid
        time.sleep(0.1)
    raise TimeoutError("host watcher (pid %s) did not exit within %ss; end it with your OS tools" % (pid, timeout))
