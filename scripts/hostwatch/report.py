"""Collect a host report, evaluate it into one verdict, and render it."""

from __future__ import annotations

import json
import os
import platform
import time

from . import common, procs, sampling
from .common import SEVERITY, median


def _level(value, hold, throttle):
    if value >= hold:
        return "hold"
    if value >= throttle:
        return "throttle"
    return "go"


def evaluate(report, cfg):
    """Set report['verdict'] to the most severe per-metric level and add reasons."""
    verdict, reasons, sampled = "go", list(report.get("reasons", [])), False

    def raise_to(v):
        nonlocal verdict
        if SEVERITY[v] > SEVERITY[verdict]:
            verdict = v

    d = report.get("disk")
    if d and (d.get("active_pct") is not None or d.get("latency_ms") is not None):
        sampled = True
        a, lat = d.get("active_pct") or 0, d.get("latency_ms") or 0
        lvl = ("hold" if a >= cfg["disk_hold_active_pct"] or lat >= cfg["disk_hold_latency_ms"] else
               "throttle" if a >= cfg["disk_throttle_active_pct"] or lat >= cfg["disk_throttle_latency_ms"] else "go")
        if lvl != "go":
            reasons.append("disk %s: active %.0f%% latency %.0f ms" % (lvl, a, lat))
        raise_to(lvl)
    if report.get("cpu"):
        sampled = True
        lvl = _level(report["cpu"]["busy_pct"], cfg["cpu_hold_pct"], cfg["cpu_throttle_pct"])
        if lvl != "go":
            reasons.append("cpu %s: busy %.0f%%" % (lvl, report["cpu"]["busy_pct"]))
        raise_to(lvl)
    if report.get("memory"):
        sampled = True
        lvl = _level(report["memory"]["used_pct"], cfg["memory_hold_pct"], cfg["memory_throttle_pct"])
        if lvl != "go":
            reasons.append("memory %s: used %.0f%%" % (lvl, report["memory"]["used_pct"]))
        raise_to(lvl)
    for s in report.get("space", []):
        s["low"] = s["free_gb"] < cfg["min_free_gb"]
        if s["low"]:
            reasons.append("space clean: %s has %.1f GB free (< %g GB)" % (s["path"], s["free_gb"], cfg["min_free_gb"]))
            raise_to("clean")
    if not sampled:
        reasons.append("no pressure metric could be sampled")
        raise_to("unknown")
    report["verdict"], report["reasons"] = verdict, reasons
    return report


def collect(cfg, samples=3, interval=1.0, watch_paths=None, caches=False, sampler=None):
    """Sample the host and return an evaluated report (the host.status payload)."""
    io0, t0 = procs.io_snapshot(), time.monotonic()
    raw = (sampler or sampling.sample_host)(samples, interval)
    io1 = procs.io_snapshot()
    report = {"verdict": "go", "reasons": list(raw.get("notes", [])), "sampled_at": common.now_iso(),
              "source": "sampled", "os": platform.system().lower(), "cpu": None, "memory": raw.get("memory"),
              "disk": sampling.summarize_disks(raw.get("disks", []), raw.get("scope", "host")),
              "space": [], "top_io": None}
    busy = median(raw.get("cpu", []))
    if busy is not None:
        report["cpu"] = {"busy_pct": round(busy, 1), "cores": os.cpu_count() or 0}
    for metric, missing in (("cpu", report["cpu"] is None), ("memory", report["memory"] is None),
                            ("disk", report["disk"] is None)):
        if missing:
            report["reasons"].append("%s unavailable: %s" % (metric, raw.get("missing", {}).get(metric, "no samples")))
    if io0 is not None:
        report["top_io"] = procs.top_io(io0, io1, time.monotonic() - t0)
    else:
        report["reasons"].append("top_io unavailable: per-process disk I/O is read on Linux only by the Python scripts")
    report["space"], notes = sampling.space_report(watch_paths or sampling.default_paths(), cfg["min_free_gb"])
    report["reasons"] += notes
    if caches:
        from .caches import inventory
        report["caches"] = inventory()
    return evaluate(report, cfg)


def add_caller_space(r, directory, cfg):
    """Add the caller's volume to a watcher snapshot, so a caller on a volume
    the watcher does not watch still gets its low-space check."""
    extra, _ = sampling.space_report([directory], cfg["min_free_gb"])
    have = {s["path"] for s in r.get("space", [])}
    try:
        have_dev = {os.stat(s["path"]).st_dev for s in r.get("space", [])}
    except OSError:
        have_dev = set()
    for s in extra:
        try:
            if s["path"] in have or os.stat(s["path"]).st_dev in have_dev:
                continue
        except OSError:
            continue
        r.setdefault("space", []).append(s)
        if s["low"]:
            r.setdefault("reasons", []).append(
                "space clean: %s has %.1f GB free (< %g GB)" % (s["path"], s["free_gb"], cfg["min_free_gb"]))
            if SEVERITY["clean"] > SEVERITY.get(r.get("verdict"), 0):
                r["verdict"] = "clean"
    return r


def read_snapshot(path, max_age=common.SNAPSHOT_MAX_AGE_S, now=None):
    """The watcher's report when fresh, else None (callers then sample)."""
    try:
        with open(path, encoding="utf-8") as fh:
            env = json.load(fh)
    except (OSError, ValueError):
        return None
    data = env.get("data") if isinstance(env, dict) else None
    if env.get("kind") != common.SNAPSHOT_KIND or not isinstance(data, dict):
        return None
    at, now = common.parse_iso(data.get("sampled_at")), now if now is not None else time.time()
    if at is None or now - at > max_age or at > now + max_age:
        return None
    data["source"], data["snapshot_path"] = "watcher", path
    return data


def render(r):
    mark = {"go": "[ok]", "hold": "[!!]"}.get(r["verdict"], "[! ]")
    lines = ["%s verdict %s (%s, %s at %s)" % (mark, r["verdict"], r.get("os"), r.get("source"), r.get("sampled_at"))]
    c, m, d = r.get("cpu"), r.get("memory"), r.get("disk")
    lines.append("cpu     %.1f%% busy, %d cores" % (c["busy_pct"], c["cores"]) if c else "cpu     unavailable")
    lines.append("memory  %.1f%% used, %.1f GB available of %.1f GB" % (m["used_pct"], m["available_gb"], m["total_gb"])
                 if m else "memory  unavailable")
    if d:
        parts = [d["device"]]
        for label, key, fmt in (("active", "active_pct", "%.1f%%"), ("latency", "latency_ms", "%.1f ms"),
                                ("write", "write_mb_s", "%.2f MB/s"), ("read", "read_mb_s", "%.2f MB/s")):
            if d.get(key) is not None:
                parts.append(label + " " + fmt % d[key])
        if d.get("write_mb_s") is None and d.get("throughput_mb_s") is not None:
            parts.append("throughput %.2f MB/s" % d["throughput_mb_s"])
        lines.append("disk    " + "  ".join(parts) + " (" + d.get("scope", "") + ")")
    else:
        lines.append("disk    unavailable")
    for s in r.get("space", []):
        lines.append("space   %s %.1f GB free of %.1f GB%s" % (s["path"], s["free_gb"], s["total_gb"], "  LOW" if s.get("low") else ""))
    for p in r.get("top_io") or []:
        lines.append("io      %s pid %d write %.2f MB/s read %.2f MB/s %s" % (
            p["name"], p["pid"], p["write_mb_s"], p["read_mb_s"], p.get("worktree") or p.get("cwd", "")))
    if r.get("inbox"):
        e = r["inbox"]
        lines.append("[! ] inbox %s (%s): %s" % (e["verdict"], e["sampled_at"], e["action"]))
    for c in r.get("caches") or []:
        lines.append("cache   %s %.2f GB %s" % (c["name"], c["gb"], c["path"]))
    lines += ["[i ] " + x for x in r.get("reasons", [])]
    return "\n".join(lines) + "\n"
