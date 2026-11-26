# Configuration and thresholds

Defaults work without any file. To change thresholds, create
`~/.host-watcher/config.json` (or point `HOST_WATCHER_CONFIG` at a file). The
Python scripts and the Go binary read the same file.

```json
{
  "thresholds": {
    "cpu_throttle_pct": 85,
    "cpu_hold_pct": 95,
    "memory_throttle_pct": 88,
    "memory_hold_pct": 95,
    "disk_throttle_active_pct": 70,
    "disk_hold_active_pct": 90,
    "disk_throttle_latency_ms": 50,
    "disk_hold_latency_ms": 100,
    "min_free_gb": 25
  }
}
```

A copy lives in [examples/config.json](../examples/config.json). Keys may also
sit at the top level without `"thresholds"`. Unknown keys and non-numeric
values are ignored; invalid JSON is an error (exit 2) naming the file.

## How the verdict is computed

1. Disk: `hold` when active ≥ `disk_hold_active_pct` **or** latency ≥
   `disk_hold_latency_ms`; `throttle` at the throttle values. Only active time
   and latency count; throughput alone never gates.
2. CPU and memory: `hold` / `throttle` at their percentages.
3. Space: any watched volume with less than `min_free_gb` free → `clean`.
4. No CPU, memory or disk-activity sample at all → `unknown`.
5. The most severe result wins: `hold` > `clean` > `unknown` > `throttle` > `go`.

Values are medians over the samples (`--samples`, default 3, `--interval`,
default 1 s). The watcher takes one sample per interval.

## Environment variables

| Variable | Effect |
|---|---|
| `HOST_WATCHER_HOME` | state directory (default `~/.host-watcher`) |
| `HOST_WATCHER_CONFIG` | thresholds file (default `$HOST_WATCHER_HOME/config.json`) |
| `HOST_WATCHER_HOOK=off` | silences the hooks |
| `HOST_WATCHER_AUTOSTART=0` | the Claude Code hook never starts a watcher |
| `HOST_WATCHER_DIR`, `HOST_WATCHER_PYTHON` | used by the Pi extension |

## Per-command options

| Option | Python | Go |
|---|---|---|
| alternate config file | `--config FILE` | `HOST_WATCHER_CONFIG` |
| free-space floor | `--min-free-gb N` | `--min-free-gb N` |
| volumes to check | `--path P` (repeatable; replaces defaults) | `--path P` |
| sampling | `--samples N --interval S` (seconds) | `--samples N --interval 1s` (duration) |
| watcher cadence | `--interval 5 --idle-exit 1800` (seconds; 0 = never idle-exit) | `--interval 5s --idle-exit 30m` |
| gate wait | `--max-wait 1800` (seconds) | `--max-wait 30m` |

Default watched volumes: the working directory, home, the temp directory and,
on Windows, every fixed drive. Passing any of `--fresh`, `--caches`, `--path`,
`--min-free-gb` or `--config` to `status` bypasses the watcher snapshot.

## Tuning tips

- HDDs and network disks show high latency at low load; raise the latency
  thresholds rather than the active ones.
- On machines with little free space by design, set `min_free_gb` to the
  space your largest build needs, not to zero.
- Use `--path` to watch the volume that holds worktrees and caches when it
  differs from home.
