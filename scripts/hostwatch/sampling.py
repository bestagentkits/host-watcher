"""Platform samplers for CPU, memory, physical disk activity and free space.

Each sampler returns plain dicts. A metric that cannot be measured is None
and its reason lands in `missing`; values are never guessed.
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import tempfile
import time

from .common import GIB, is_wsl, median

WIN_DISK_PS = r"""
$ErrorActionPreference='Stop'
for ($i = 0; $i -lt {n}; $i++) {{
  if ($i -gt 0) {{ Start-Sleep -Milliseconds {ms} }}
  $rows = Get-CimInstance -ClassName Win32_PerfRawData_PerfDisk_PhysicalDisk |
    Where-Object {{ $_.Name -ne '_Total' }} |
    ForEach-Object {{ @{{ n = $_.Name; idle = [double]$_.PercentIdleTime;
      ts = [double]$_.Timestamp_Sys100NS; idleb = [double]$_.PercentIdleTime_Base; lat = [double]$_.AvgDisksecPerTransfer;
      latb = [double]$_.AvgDisksecPerTransfer_Base; f = [double]$_.Frequency_PerfTime;
      wb = [double]$_.DiskWriteBytesPersec; rb = [double]$_.DiskReadBytesPersec }} }}
  ConvertTo-Json -Compress -InputObject @($rows)
}}
"""


def _rate(active=None, latency=None, write=None, read=None, throughput=None):
    return {"active": active, "latency": latency, "write": write, "read": read, "throughput": throughput}


# ---------- Linux ----------

def parse_proc_stat(text):
    for line in text.splitlines():
        f = line.split()
        if f and f[0] == "cpu":
            vals = [int(x) for x in f[1:]]
            idle = vals[3] + (vals[4] if len(vals) > 4 else 0)
            return sum(vals[:8]), idle
    raise ValueError("no aggregate cpu line in /proc/stat")


def busy_pct(prev, cur):
    total, idle = cur[0] - prev[0], cur[1] - prev[1]
    if total <= 0:
        return None
    return max(0.0, min(100.0, (total - idle) / total * 100))


def parse_meminfo(text):
    vals = {}
    for line in text.splitlines():
        key, _, rest = line.partition(":")
        if key in ("MemTotal", "MemAvailable") and rest.split():
            vals[key] = int(rest.split()[0]) * 1024
    if not vals.get("MemTotal") or "MemAvailable" not in vals:
        raise ValueError("MemTotal or MemAvailable missing in /proc/meminfo")
    return memory(vals["MemTotal"], vals["MemAvailable"])


def parse_diskstats(text, is_whole=lambda n: os.path.exists("/sys/block/" + n)):
    rows = {}
    for line in text.splitlines():
        f = line.split()
        if len(f) < 14 or f[2].startswith(("loop", "ram", "zram", "sr", "fd")) or not is_whole(f[2]):
            continue
        rows[f[2]] = [int(x) for x in f[3:14]]
    return rows


def diskstats_rates(prev, cur, elapsed_ms):
    out = {}
    for name, c in cur.items():
        p = prev.get(name)
        if not p or elapsed_ms <= 0:
            continue
        ios = (c[0] - p[0]) + (c[4] - p[4])
        wait = (c[3] - p[3]) + (c[7] - p[7])
        secs = elapsed_ms / 1000
        w, r = (c[6] - p[6]) * 512 / secs / 1e6, (c[2] - p[2]) * 512 / secs / 1e6
        out[name] = _rate(min(100.0, (c[9] - p[9]) / elapsed_ms * 100), wait / ios if ios else 0.0, w, r, w + r)
    return out


def _read(path):
    with open(path, encoding="ascii", errors="ignore") as fh:
        return fh.read()


def sample_linux(n, interval):
    raw = {"cpu": [], "memory": None, "disks": [], "scope": "wsl-guest" if is_wsl() else "host",
           "notes": [], "missing": {}}
    try:
        raw["memory"] = parse_meminfo(_read("/proc/meminfo"))
    except (OSError, ValueError) as exc:
        raw["missing"]["memory"] = str(exc)
    try:
        cpu0, disk0, t0 = parse_proc_stat(_read("/proc/stat")), parse_diskstats(_read("/proc/diskstats")), time.monotonic()
    except (OSError, ValueError) as exc:
        raw["missing"]["cpu"] = raw["missing"]["disk"] = str(exc)
        return raw
    for _ in range(n):
        time.sleep(interval)
        cpu1, disk1, t1 = parse_proc_stat(_read("/proc/stat")), parse_diskstats(_read("/proc/diskstats")), time.monotonic()
        raw["cpu"].append(busy_pct(cpu0, cpu1))
        raw["disks"].append(diskstats_rates(disk0, disk1, (t1 - t0) * 1000))
        cpu0, disk0, t0 = cpu1, disk1, t1
    if raw["scope"] == "wsl-guest":
        raw["notes"].append("WSL: disk metrics describe the WSL virtual disk, not the Windows host")
    return raw


# ---------- macOS ----------

def parse_vm_stat(text):
    import re
    m = re.search(r"page size of (\d+) bytes", text)
    if not m:
        raise ValueError("vm_stat: page size not found")
    pages, found = 0, 0
    for line in text.splitlines():
        key, _, rest = line.partition(":")
        if key.strip() in ("Pages free", "Pages inactive", "Pages speculative"):
            pages += int(rest.strip().rstrip("."))
            found += 1
    if not found:
        raise ValueError("vm_stat: no free/inactive/speculative page counts")
    return pages * int(m.group(1))


def parse_iostat(text):
    """Rows of macOS `iostat -d -C -c N -w S`; the since-boot first row is dropped."""
    devices, has_cpu, rows = None, False, []
    for line in text.splitlines():
        f = line.split()
        if not f:
            continue
        try:
            float(f[0])
        except ValueError:
            if devices is None and "/" not in f[0]:
                devices = [t for t in f if t.startswith("disk")]
                has_cpu = "cpu" in f
            continue
        rows.append([float(x) for x in f])
    if not devices and not has_cpu:
        raise ValueError("iostat: header not found")
    if len(rows) < 2:
        raise ValueError("iostat: need at least two rows")
    out = []
    for row in rows[1:]:
        disks = {d: row[3 * i + 2] for i, d in enumerate(devices or []) if 3 * i + 2 < len(row)}
        base = 3 * len(devices or [])
        cpu = max(0.0, min(100.0, 100 - row[base + 2])) if has_cpu and base + 2 < len(row) else None
        out.append((disks, cpu))
    return out


def sample_darwin(n, interval):
    raw = {"cpu": [], "memory": None, "disks": [], "scope": "unavailable", "notes": [], "missing": {}}
    try:
        total = int(subprocess.run(["/usr/sbin/sysctl", "-n", "hw.memsize"], capture_output=True, text=True,
                                   timeout=10).stdout.strip())
        vm = subprocess.run(["/usr/bin/vm_stat"], capture_output=True, text=True, timeout=10).stdout
        raw["memory"] = memory(total, parse_vm_stat(vm))
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        raw["missing"]["memory"] = str(exc)
    secs = max(1, int(round(interval)))
    try:
        out = subprocess.run(["/usr/sbin/iostat", "-d", "-C", "-c", str(n + 1), "-w", str(secs)],
                             capture_output=True, text=True, timeout=n * secs + 30).stdout
        for disks, cpu in parse_iostat(out):
            if cpu is not None:
                raw["cpu"].append(cpu)
            raw["disks"].append({d: _rate(throughput=mbs) for d, mbs in disks.items()})
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        raw["missing"]["cpu"] = raw["missing"]["disk"] = "iostat: %s" % exc
    raw["notes"].append("disk active time and latency are not measurable on macOS; throughput only")
    return raw


# ---------- Windows ----------

def _win_cpu_times():
    import ctypes
    from ctypes import wintypes
    idle, kernel, user = wintypes.FILETIME(), wintypes.FILETIME(), wintypes.FILETIME()
    if not ctypes.windll.kernel32.GetSystemTimes(ctypes.byref(idle), ctypes.byref(kernel), ctypes.byref(user)):
        raise OSError("GetSystemTimes failed")
    v = lambda ft: (ft.dwHighDateTime << 32) | ft.dwLowDateTime  # noqa: E731
    return v(kernel) + v(user), v(idle)  # kernel time includes idle time


def _win_memory():
    import ctypes

    class MEMORYSTATUSEX(ctypes.Structure):
        _fields_ = [("dwLength", ctypes.c_ulong), ("dwMemoryLoad", ctypes.c_ulong),
                    ("ullTotalPhys", ctypes.c_ulonglong), ("ullAvailPhys", ctypes.c_ulonglong),
                    ("ullTotalPageFile", ctypes.c_ulonglong), ("ullAvailPageFile", ctypes.c_ulonglong),
                    ("ullTotalVirtual", ctypes.c_ulonglong), ("ullAvailVirtual", ctypes.c_ulonglong),
                    ("ullAvailExtendedVirtual", ctypes.c_ulonglong)]
    st = MEMORYSTATUSEX()
    st.dwLength = ctypes.sizeof(st)
    if not ctypes.windll.kernel32.GlobalMemoryStatusEx(ctypes.byref(st)) or not st.ullTotalPhys:
        raise OSError("GlobalMemoryStatusEx failed")
    return memory(st.ullTotalPhys, st.ullAvailPhys)


def win_disk_rates(snaps):
    """Turn consecutive raw CIM snapshots into per-disk rates (Task Manager 'Active time')."""
    samples = []
    for prev, cur in zip(snaps, snaps[1:]):
        sample = {}
        for name, c in cur.items():
            p = prev.get(name)
            dts = c["ts"] - p["ts"] if p else 0
            if dts <= 0:
                continue
            didle_base = (c.get("idleb") or 0) - (p.get("idleb") or 0)
            idle = max(0.0, min(100.0, (c["idle"] - p["idle"]) / (didle_base if didle_base > 0 else dts) * 100))
            dbase = c["latb"] - p["latb"]
            lat = ((c["lat"] - p["lat"]) / c["f"] / dbase * 1000) if dbase > 0 and c["f"] else 0.0
            secs = dts / 1e7
            w, r = (c["wb"] - p["wb"]) / secs / 1e6, (c["rb"] - p["rb"]) / secs / 1e6
            sample[name] = _rate(100.0 - idle, lat, w, r, w + r)
        samples.append(sample)
    return samples


def sample_windows(n, interval):
    raw = {"cpu": [], "memory": None, "disks": [], "scope": "host", "notes": [], "missing": {}}
    try:
        raw["memory"] = _win_memory()
    except OSError as exc:
        raw["missing"]["memory"] = str(exc)
    exe = shutil.which("powershell") or shutil.which("pwsh")
    try:
        c0 = _win_cpu_times()
        if not exe:
            raise RuntimeError("powershell not found")
        script = WIN_DISK_PS.format(n=n + 1, ms=int(interval * 1000))
        out = subprocess.run([exe, "-NoProfile", "-NonInteractive", "-Command", script],
                             capture_output=True, text=True, timeout=30 + n * (interval + 5))
        c1 = _win_cpu_times()
        if out.returncode != 0:
            raise RuntimeError(out.stderr.strip()[:300] or "CIM sampling failed")
        snaps = [{r["n"]: r for r in json.loads(l)} for l in out.stdout.splitlines() if l.strip().startswith("[")]
        raw["disks"] = win_disk_rates(snaps)
        raw["cpu"].append(busy_pct(c0, c1))
    except Exception as exc:  # degrade to null metrics, never crash
        raw["missing"].setdefault("disk", str(exc)[:300])
        if not raw["cpu"]:
            try:
                c0 = _win_cpu_times()
                time.sleep(interval * n)
                raw["cpu"].append(busy_pct(c0, _win_cpu_times()))
            except OSError as exc2:
                raw["missing"]["cpu"] = str(exc2)
    return raw


def sample_host(n, interval):
    system = platform.system()
    if system == "Linux":
        return sample_linux(n, interval)
    if system == "Darwin":
        return sample_darwin(n, interval)
    if system == "Windows":
        return sample_windows(n, interval)
    return {"cpu": [], "memory": None, "disks": [], "scope": "unavailable", "notes": [],
            "missing": {k: "unsupported platform " + system for k in ("cpu", "memory", "disk")}}


# ---------- shared ----------

def memory(total, avail):
    avail = min(avail, total)
    return {"used_pct": round((total - avail) / total * 100, 1), "available_gb": round(avail / GIB, 1),
            "total_gb": round(total / GIB, 1)}


def summarize_disks(samples, scope):
    """Busiest device by median active time, then latency, then throughput."""
    by_dev = {}
    for s in samples:
        for name, rate in s.items():
            by_dev.setdefault(name, []).append(rate)
    best = None
    for name in sorted(by_dev):
        rates = by_dev[name]
        pick = lambda k, d: (lambda m: None if m is None else round(m, d))(median([r[k] for r in rates]))  # noqa: E731
        disk = {"device": name, "active_pct": pick("active", 1), "latency_ms": pick("latency", 1),
                "write_mb_s": pick("write", 2), "read_mb_s": pick("read", 2),
                "throughput_mb_s": pick("throughput", 2), "scope": scope}
        key = lambda d: (d["active_pct"] or 0, d["latency_ms"] or 0, d["throughput_mb_s"] or 0)  # noqa: E731
        if best is None or key(disk) > key(best):
            best = disk
    return best


def windows_fixed_drives():
    import ctypes
    mask, drives = ctypes.windll.kernel32.GetLogicalDrives(), []
    for i in range(26):
        root = chr(65 + i) + ":\\"
        if mask & (1 << i) and ctypes.windll.kernel32.GetDriveTypeW(root) == 3:  # DRIVE_FIXED
            drives.append(root)
    return drives


def default_paths():
    out = [os.getcwd(), os.path.expanduser("~"), tempfile.gettempdir()]
    if platform.system() == "Windows":
        out += windows_fixed_drives()
    return out


def space_report(paths_, min_free_gb):
    seen, out, notes = set(), [], []
    for p in paths_:
        try:
            key = os.stat(p).st_dev
            if key in seen:
                continue
            seen.add(key)
            u = shutil.disk_usage(p)
        except OSError as exc:
            notes.append("space unavailable for %s: %s" % (p, exc))
            continue
        free = u.free / GIB
        out.append({"path": p, "free_gb": round(free, 1), "total_gb": round(u.total / GIB, 1), "low": free < min_free_gb})
    return out, notes
