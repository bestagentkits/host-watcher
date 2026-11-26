"""Cache inventory and conservative cleanup of owned, exited temp dirs.

Shared caches (Go, npm, pnpm...) are only measured, never deleted: the
native command to prune them is suggested instead. Temp-dir cleanup only
removes directories that match an explicit pattern, belong to the current
user, are not referenced by any running process, and have been idle for a
minimum age. Anything that cannot be verified is skipped with a reason.
"""

from __future__ import annotations

import fnmatch
import os
import platform
import shutil
import stat
import subprocess
import sys
import tempfile
import time

from . import procs
from .common import GIB

PRUNE_HINTS = {
    "go-build": "go clean -cache",
    "go-mod": "go clean -modcache",
    "golangci-lint": "golangci-lint cache clean",
    "pnpm-store": "pnpm store prune",
    "npm-cache": "npm cache verify",
    "pip": "pip cache purge",
    "yarn": "yarn cache clean",
    "cargo-registry": "cargo cache --autoclean  (needs cargo-cache)",
}


def _go_env(key):
    if os.environ.get(key) and os.environ[key] != "off":
        return os.environ[key]
    if shutil.which("go"):
        try:
            v = subprocess.run(["go", "env", key], capture_output=True, text=True, timeout=20).stdout.strip()
            return v if v and v != "off" else None
        except (OSError, subprocess.SubprocessError):
            return None
    return None


def locations():
    home = os.path.expanduser("~")
    system = platform.system()
    if system == "Windows":
        local = os.environ.get("LOCALAPPDATA", os.path.join(home, "AppData", "Local"))
        user_cache, pnpm, npm = local, os.path.join(local, "pnpm", "store"), os.path.join(local, "npm-cache")
        pip, yarn = os.path.join(local, "pip", "Cache"), os.path.join(local, "Yarn", "Cache")
    elif system == "Darwin":
        user_cache = os.path.join(home, "Library", "Caches")
        pnpm, npm = os.path.join(home, "Library", "pnpm", "store"), os.path.join(home, ".npm")
        pip, yarn = os.path.join(user_cache, "pip"), os.path.join(user_cache, "Yarn")
    else:
        user_cache = os.environ.get("XDG_CACHE_HOME", os.path.join(home, ".cache"))
        data = os.environ.get("XDG_DATA_HOME", os.path.join(home, ".local", "share"))
        pnpm, npm = os.path.join(data, "pnpm", "store"), os.path.join(home, ".npm")
        pip, yarn = os.path.join(user_cache, "pip"), os.path.join(user_cache, "yarn")
    gopath = (os.environ.get("GOPATH") or os.path.join(home, "go")).split(os.pathsep)[0]
    return {
        "go-build": _go_env("GOCACHE") or os.path.join(user_cache, "go-build"),
        "go-mod": _go_env("GOMODCACHE") or os.path.join(gopath, "pkg", "mod"),
        "golangci-lint": os.environ.get("GOLANGCI_LINT_CACHE") or os.path.join(user_cache, "golangci-lint"),
        "pnpm-store": os.environ.get("PNPM_STORE_DIR") or pnpm,
        "npm-cache": os.environ.get("npm_config_cache") or npm,
        "pip": os.environ.get("PIP_CACHE_DIR") or pip,
        "yarn": yarn,
        "cargo-registry": os.path.join(os.environ.get("CARGO_HOME") or os.path.join(home, ".cargo"), "registry"),
    }


def tree_size(path):
    """Bytes under path; never follows symlinks; unreadable entries skipped."""
    total, stack = 0, [path]
    while stack:
        try:
            with os.scandir(stack.pop()) as it:
                for e in it:
                    try:
                        if e.is_dir(follow_symlinks=False):
                            stack.append(e.path)
                        elif e.is_file(follow_symlinks=False):
                            total += e.stat(follow_symlinks=False).st_size
                    except OSError:
                        pass
        except OSError:
            pass
    return total


def inventory(extra=()):
    out = []
    named = dict(locations())
    for p in extra:
        named[p] = p
    for name, path in named.items():
        if path and os.path.isdir(path):
            out.append({"name": name, "path": path, "gb": round(tree_size(path) / GIB, 2),
                        "prune_with": PRUNE_HINTS.get(name, "")})
    return out


def newest_mtime(path, depth=2):
    newest, frontier = os.lstat(path).st_mtime, [path]
    for _ in range(depth):
        nxt = []
        for d in frontier:
            try:
                with os.scandir(d) as it:
                    for e in it:
                        try:
                            newest = max(newest, e.stat(follow_symlinks=False).st_mtime)
                            if e.is_dir(follow_symlinks=False):
                                nxt.append(e.path)
                        except OSError:
                            pass
            except OSError:
                pass
        frontier = nxt
    return newest


def referenced(path, text):
    """A running command line or cwd names the dir (any slash form or its basename)."""
    norm = path.lower().replace("\\", "/")
    return norm in text or os.path.basename(path).lower() in text


def owned(entry):
    """True when the current user owns the dir. Windows has no cheap uid check, so
    there the dir must sit inside the user's own profile (%USERPROFILE%)."""
    if not hasattr(os, "getuid"):
        profile = os.path.normcase(os.path.abspath(os.environ.get("USERPROFILE") or os.path.expanduser("~")))
        target = os.path.normcase(os.path.abspath(entry.path))
        return bool(profile) and target.startswith(profile.rstrip("\\/") + os.sep)
    try:
        return entry.stat(follow_symlinks=False).st_uid == os.getuid()
    except OSError:
        return False


def candidates(globs, roots=None, min_age_min=60.0, process_text=None, now=None):
    """(found, skipped). process_text=None means the process list was unreadable."""
    roots = roots or [tempfile.gettempdir()]
    now = now if now is not None else time.time()
    found, skipped = [], []
    for root in roots:
        try:
            entries = list(os.scandir(root))
        except OSError as exc:
            skipped.append({"path": root, "reason": "root unreadable: %s" % exc})
            continue
        for e in entries:
            if not any(fnmatch.fnmatch(e.name, g) for g in globs):
                continue
            if e.is_symlink() or getattr(e, "is_junction", lambda: False)() or not e.is_dir(follow_symlinks=False):
                skipped.append({"path": e.path, "reason": "not a plain directory (link or file)"})
                continue
            try:
                age_min = (now - newest_mtime(e.path)) / 60
            except OSError:
                continue  # removed meanwhile
            if not owned(e):
                skipped.append({"path": e.path, "reason": "owned by another user"})
            elif process_text is None:
                skipped.append({"path": e.path, "reason": "process list unavailable; cannot verify it is unused"})
            elif referenced(e.path, process_text):
                skipped.append({"path": e.path, "reason": "referenced by a running process"})
            elif age_min < min_age_min:
                skipped.append({"path": e.path, "reason": "modified %.0f min ago (< %g)" % (age_min, min_age_min)})
            else:
                found.append({"path": e.path, "age_min": round(age_min), "gb": round(tree_size(e.path) / GIB, 3)})
    return found, skipped


def force_remove(path):
    """rmtree that clears read-only bits but never follows or chmods links."""
    def retry(func, target, exc):
        if func is os.path.islink:
            raise exc[1] if isinstance(exc, tuple) else exc
        for p in (os.path.dirname(target), target):
            try:
                if not os.path.islink(p):
                    os.chmod(p, os.lstat(p).st_mode | stat.S_IWRITE | stat.S_IREAD | stat.S_IEXEC)
            except OSError:
                pass
        func(target)

    if sys.version_info >= (3, 12):
        shutil.rmtree(path, onexc=retry)
    else:
        shutil.rmtree(path, onerror=retry)


def process_text():
    rows = procs.process_list()
    return procs.command_text(rows) if rows is not None else None
