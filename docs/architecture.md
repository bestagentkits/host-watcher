# Architecture

host-watcher is two interchangeable implementations of the same contract plus
agent hooks that read its files. There is no server, database or network
access.

```text
             samples (OS counters)                      reads (no sampling)
 host_status.py ─┐                                    ┌─ hooks/claude-code/*.cjs
 host-watcher status ─┤──► verdict                     ├─ hooks/generic/*.py
                  │                                    ├─ hooks/pi/*.ts
 host_watch.py ───┤──► ~/.host-watcher/status.json ───┤─ host_status.py (fresh snapshot)
 host-watcher watch ┘     ~/.host-watcher/inbox/*.json ─┴─ host_gate.py / host_inbox.py
                          ~/.host-watcher/watch.lock  (one watcher per machine)
```

## Components

| Path | Role |
|---|---|
| `scripts/hostwatch/sampling.py` | per-OS samplers for CPU, memory, busiest disk, free space |
| `scripts/hostwatch/report.py` | collect, evaluate into a verdict, render, read snapshots |
| `scripts/hostwatch/watcher.py` | singleton lock, watch loop, stop sentinel |
| `scripts/hostwatch/inbox.py` | per-worktree notices |
| `scripts/hostwatch/gate.py` | wait with backoff, low-priority child, signal forwarding |
| `scripts/hostwatch/caches.py` | cache inventory, cleanup candidates, safe removal |
| `scripts/hostwatch/procs.py` | process list, agent detection, Linux per-process I/O |
| `internal/hoststatus` | Go equivalent (PDH on Windows, `/proc` on Linux, `iostat`/`vm_stat` on macOS) |
| `internal/lockfile` | `flock` / `LockFileEx` advisory lock |
| `cmd/host-watcher` | Go CLI: `status`, `watch`, `gate`, `inbox`, `caches` |

## State directory

`$HOST_WATCHER_HOME`, default `~/.host-watcher`:

| File | Writer | Meaning |
|---|---|---|
| `status.json` | watcher | `{"schema_version":1,"kind":"host.status","data":{...report}}`, rewritten atomically every interval |
| `watch.lock` | watcher | held for the watcher's lifetime; liveness is the lock, not the pid file |
| `watch.pid` | watcher | `{"pid":N,"started_at":"..."}` for reporting |
| `watch.stop` | `--stop` | sentinel the loop polls to exit cleanly |
| `inbox/<hash>.json` | watcher | notice for one worktree; removed when read with `--take` or after 10 min |
| `config.json` | you | optional thresholds ([configuration](configuration.md)) |
| `watch.log` | Python `--detach` | the background watcher's stderr |

A snapshot older than 15 s is ignored and callers sample directly, so a dead
watcher can never serve stale data.

## Report (`data`)

| Field | Notes |
|---|---|
| `verdict` | `go`, `throttle`, `unknown`, `clean`, `hold` |
| `reasons` | why, plus a reason for every `null` metric |
| `sampled_at`, `source`, `os` | `source` is `watcher` or `sampled` |
| `cpu` | `{busy_pct, cores}` or `null` |
| `memory` | `{used_pct, available_gb, total_gb}` or `null` |
| `disk` | busiest disk: `{device, active_pct, latency_ms, write_mb_s, read_mb_s, throughput_mb_s, scope}`; `scope` is `host`, `wsl-guest` or `unavailable` |
| `space` | `[{path, free_gb, total_gb, low}]`, one per distinct volume |
| `top_io` | up to 5 processes by I/O with `worktree` and agent `runtime`, or `null` where unsupported |
| `io_scope` | set on Windows (Go): rates count all I/O, network and pipes included |
| `inbox` | this worktree's notice, shown without consuming it |
| `caches` | with `--caches`: `[{name, path, gb}]` |

## Worktree hash

Inbox files are named by the first 16 hex characters of
`sha256(normalize(worktree_root))`, where normalize replaces `\` with `/`,
cleans the path POSIX-style (collapse `//`, resolve `.`/`..`, drop a trailing
`/`), turns a bare drive `X:` into `X:/`, and on Windows lower-cases ASCII
`A-Z` only. Go, Python and the JS hook share the test vector
`D:\Worktrees\x\` → `d49b00fbf4d3b90e`.

## Design limits

The watcher is a pacing aid, not an adaptive scheduler: it does not queue,
predict, rank or kill. Admission decisions stay with the caller.
