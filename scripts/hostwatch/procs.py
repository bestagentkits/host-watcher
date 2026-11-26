"""Process table helpers: command lines, agent runtime detection, Linux top I/O."""

from __future__ import annotations

import os
import platform
import shutil
import subprocess

from .common import worktree_root

AGENT_NAMES = {"claude": "claude-code", "codex": "codex", "opencode": "opencode", "pi": "pi",
               "cursor": "cursor", "cursor-agent": "cursor"}
CMDLINE_MARKERS = [("@anthropic-ai/claude-code", "claude-code"), ("claude-code", "claude-code"),
                   ("@openai/codex", "codex"), ("opencode", "opencode"), ("pi-coding-agent", "pi"),
                   ("cursor-agent", "cursor")]


def runtime_of(name, cmdline=""):
    base = os.path.basename(name.replace("\\", "/")).lower()
    if base.endswith(".exe"):
        base = base[:-4]
    if base in AGENT_NAMES:
        return AGENT_NAMES[base]
    c = cmdline.lower().replace("\\", "/")
    for marker, rt in CMDLINE_MARKERS:
        if marker in c:
            return rt
    return ""


def process_list():
    """[(pid, name, cmdline, cwd)] or None when the table cannot be read.

    Linux hides other users' rows under hidepid; fewer than three readable
    rows is treated as unreadable, never as "nothing running"."""
    system = platform.system()
    try:
        if system == "Linux":
            rows = []
            for pid in os.listdir("/proc"):
                if not pid.isdigit():
                    continue
                try:
                    with open("/proc/%s/cmdline" % pid, "rb") as fh:
                        argv = fh.read().split(b"\0")
                    name = argv[0].decode(errors="ignore") if argv and argv[0] else ""
                    try:
                        cwd = os.readlink("/proc/%s/cwd" % pid)
                    except OSError:
                        cwd = ""
                    rows.append((int(pid), name, b" ".join(argv).decode(errors="ignore"), cwd))
                except OSError:
                    pass
            return rows if len(rows) > 2 else None
        if system == "Windows":
            exe = shutil.which("powershell") or shutil.which("pwsh")
            if not exe:
                return None
            out = subprocess.run([exe, "-NoProfile", "-NonInteractive", "-Command",
                                  "Get-CimInstance Win32_Process | ForEach-Object { \"$($_.ProcessId)`t$($_.Name)`t$($_.CommandLine)\" }"],
                                 capture_output=True, text=True, timeout=60)
            if out.returncode != 0 or not out.stdout.strip():
                return None
            rows = []
            for line in out.stdout.splitlines():
                parts = line.split("\t", 2)
                if len(parts) == 3 and parts[0].isdigit():
                    rows.append((int(parts[0]), parts[1], parts[2], ""))
            return rows
        out = subprocess.run(["ps", "-axo", "pid=,comm=,command="], capture_output=True, text=True, timeout=30)
        if out.returncode != 0 or not out.stdout.strip():
            return None
        rows = []
        for line in out.stdout.splitlines():
            f = line.split(None, 2)
            if len(f) >= 2 and f[0].isdigit():
                rows.append((int(f[0]), f[1], f[2] if len(f) > 2 else f[1], ""))
        return rows
    except (OSError, subprocess.SubprocessError):
        return None


def agent_alive():
    """True/False, or None when the process table is unreadable."""
    rows = process_list()
    if rows is None:
        return None
    return any(runtime_of(name, cmd) for _, name, cmd, _ in rows)


def command_text(rows):
    """All command lines and cwds, lower-cased with forward slashes."""
    return "\n".join("%s\n%s" % (cmd, cwd) for _, _, cmd, cwd in rows).lower().replace("\\", "/")


def _linux_io():
    out = {}
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        try:
            vals = {}
            with open("/proc/%s/io" % pid, encoding="ascii") as fh:
                for line in fh:
                    k, _, v = line.partition(":")
                    vals[k] = int(v)
            out[int(pid)] = (vals.get("read_bytes", 0), vals.get("write_bytes", 0))
        except (OSError, ValueError):
            pass
    return out


def io_snapshot():
    """Opaque per-process I/O counters, or None where unsupported (Linux only)."""
    if platform.system() != "Linux":
        return None
    snap = _linux_io()
    return snap or None


def top_io(before, after, seconds, limit=5):
    """Top processes by write (then read) MB/s between two Linux snapshots."""
    if not before or not after or seconds <= 0:
        return []
    rates = []
    for pid, (r1, w1) in after.items():
        if pid not in before:
            continue
        r0, w0 = before[pid]
        w, r = max(0, w1 - w0) / seconds / 1e6, max(0, r1 - r0) / seconds / 1e6
        if w > 0 or r > 0:
            rates.append((w, r, pid))
    rates.sort(reverse=True)
    out = []
    for w, r, pid in rates[:limit]:
        try:
            with open("/proc/%d/comm" % pid, encoding="utf-8", errors="ignore") as fh:
                name = fh.read().strip()
            cwd = os.readlink("/proc/%d/cwd" % pid)
        except OSError:
            name, cwd = str(pid), ""
        out.append({"pid": pid, "name": name, "write_mb_s": round(w, 2), "read_mb_s": round(r, 2),
                    "cwd": cwd, "worktree": worktree_root(cwd) if cwd else "", "runtime": "unknown"})
    return out
