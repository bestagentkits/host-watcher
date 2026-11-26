# Platform support and limitations

| Metric | Linux | WSL 2 | Windows | macOS |
|---|---|---|---|---|
| CPU busy | `/proc/stat` | `/proc/stat` (VM) | `GetSystemTimes` | `iostat -C` |
| Memory | `/proc/meminfo` `MemAvailable` | VM memory | `GlobalMemoryStatusEx` | `vm_stat` + `hw.memsize` |
| Disk active time / latency | `/proc/diskstats` | **VM virtual disk only** | CIM raw counters (Python), PDH (Go) | **not available** |
| Disk throughput | yes | VM only | yes | `iostat` |
| Free space | yes | yes | every fixed drive | yes |
| Per-process I/O (`top_io`) | Python and Go | Python and Go (VM) | **Go only** | **none** |
| Worktree inbox notices | yes | yes | Go watcher only | **none** |
| Low-priority gate | `nice 10` + `ionice -c3` | same | Python: below-normal CPU class; Go: also low I/O priority | `taskpolicy -b` |
| Agent-alive check (idle exit) | `/proc` | `/proc` | CIM `Win32_Process` | `ps` |
| CI coverage | ubuntu-latest | **not covered by CI** | windows-latest | macos-latest |

## What the gaps mean

- **macOS**: there is no per-disk utilisation or latency counter without
  private APIs, so disk activity never drives the verdict; `disk.scope` is
  `unavailable` and only throughput is shown. CPU, memory and space still
  gate. Per-process disk I/O needs `libproc` (cgo), so `top_io` is `null`
  and no inbox notices are written.
- **WSL**: numbers describe the WSL virtual machine and its virtual disk, not
  the Windows host NVMe (`disk.scope` = `wsl-guest`). For host truth run the
  Windows build of the tools on the Windows side, or both.
- **Windows (Python)**: disk counters come from one PowerShell CIM call per
  status (about 1 s of overhead). Per-process I/O is not collected by the
  Python scripts; use the Go binary for `top_io` and inbox notices. The
  Python gate lowers CPU priority class only. Go `top_io` rates count every
  byte a process moves, network and pipes included (`io_scope` says so).
- **Containers and sandboxes**: `hidepid` or a PID namespace hides other
  processes. Agent detection then never triggers idle exit, and cleanup
  refuses to delete anything ("process list unavailable").
- **Filesystems that cannot lock** (NFS without locking, WSL DrvFs): the
  watcher runs but warns that a second watcher would not be detected. Keep
  `HOST_WATCHER_HOME` on a local disk.
- **Other Unixes (BSD)**: unsupported; metrics are `null` with a reason and
  the verdict is `unknown`, which never blocks the gate.

Requirements: Python 3.8+ (no packages). Go 1.22+ to build the binary.
Node 18+ only for the optional Claude Code JS hook.
