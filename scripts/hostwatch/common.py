"""Shared constants, configuration, state paths and small file helpers."""

from __future__ import annotations

import hashlib
import json
import os
import platform
import posixpath
import re
import statistics
import tempfile
from datetime import datetime, timezone

GIB = 1024 ** 3

# Verdicts from least to most severe.
SEVERITY = {"go": 0, "throttle": 1, "unknown": 2, "clean": 3, "hold": 4}

# Default thresholds. Override with a JSON config file or CLI flags.
DEFAULTS = {
    "disk_hold_active_pct": 90.0,
    "disk_hold_latency_ms": 100.0,
    "disk_throttle_active_pct": 70.0,
    "disk_throttle_latency_ms": 50.0,
    "cpu_hold_pct": 95.0,
    "cpu_throttle_pct": 85.0,
    "memory_hold_pct": 95.0,
    "memory_throttle_pct": 88.0,
    "min_free_gb": 25.0,
}

SNAPSHOT_KIND = "host.status"
SNAPSHOT_MAX_AGE_S = 15


def state_dir():
    """$HOST_WATCHER_HOME, else ~/.host-watcher (shared with the Go binary)."""
    env = os.environ.get("HOST_WATCHER_HOME", "").strip()
    return env or os.path.join(os.path.expanduser("~"), ".host-watcher")


def paths(base=None):
    d = base or state_dir()
    return {
        "dir": d,
        "lock": os.path.join(d, "watch.lock"),
        "pid": os.path.join(d, "watch.pid"),
        "snapshot": os.path.join(d, "status.json"),
        "stop": os.path.join(d, "watch.stop"),
        "inbox": os.path.join(d, "inbox"),
        "config": os.path.join(d, "config.json"),
    }


def load_config(path=None):
    """Return thresholds: defaults, then the config file, if any.

    The file is $HOST_WATCHER_CONFIG, the given path, or <state dir>/config.json.
    Unknown keys are ignored; a broken file raises ValueError with its path."""
    cfg = dict(DEFAULTS)
    path = path or os.environ.get("HOST_WATCHER_CONFIG") or paths()["config"]
    if path and os.path.isfile(path):
        try:
            with open(path, encoding="utf-8") as fh:
                data = json.load(fh)
        except (OSError, ValueError) as exc:
            raise ValueError("invalid config %s: %s" % (path, exc)) from exc
        for key, value in (data.get("thresholds", data) if isinstance(data, dict) else {}).items():
            if key in DEFAULTS and isinstance(value, (int, float)) and not isinstance(value, bool):
                cfg[key] = float(value)
    return cfg


def now_iso():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_iso(text):
    try:
        return datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()
    except (TypeError, ValueError):
        return None


def median(values):
    vals = [v for v in values if v is not None]
    return statistics.median(vals) if vals else None


def write_atomic(path, data):
    """Write text to a temp file in the same dir, then rename over path."""
    d = os.path.dirname(path) or "."
    os.makedirs(d, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix="." + os.path.basename(path) + ".tmp-", dir=d)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(data)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.remove(tmp)
        except OSError:
            pass
        raise


def envelope(kind, data):
    return json.dumps({"schema_version": 1, "kind": kind, "data": data}, indent=2) + "\n"


def is_windows():
    return platform.system() == "Windows"


def is_wsl():
    try:
        with open("/proc/version", encoding="ascii", errors="ignore") as fh:
            return "microsoft" in fh.read().lower()
    except OSError:
        return False


def worktree_root(start):
    """Nearest directory at or above start that holds `.git`, else ""."""
    cur = os.path.abspath(start)
    while True:
        if os.path.exists(os.path.join(cur, ".git")):
            return cur
        parent = os.path.dirname(cur)
        if parent == cur:
            return ""
        cur = parent


_DRIVE_ONLY = re.compile(r"^[A-Za-z]:$")


def normalize_worktree(root, fold_case):
    """Same normalization as the Go binary and the JS hook (see docs/architecture.md)."""
    p = posixpath.normpath(root.replace("\\", "/"))
    if p.startswith("//") and not p.startswith("///"):
        p = "/" + p.lstrip("/")  # Go path.Clean collapses a leading `//`
    if _DRIVE_ONLY.match(p):
        p += "/"
    if fold_case:
        p = "".join(chr(ord(c) + 32) if "A" <= c <= "Z" else c for c in p)
    return p


def worktree_hash(root, fold_case=None):
    if fold_case is None:
        fold_case = is_windows()
    return hashlib.sha256(normalize_worktree(root, fold_case).encode("utf-8")).hexdigest()[:16]
