# host-watcher

Keep a machine usable while several coding agents (Claude Code, Codex, Pi,
OpenCode, Cursor, or plain scripts) build and test in parallel.

Parallel agents rarely run out of CPU first. They saturate the **disk**: one
NVMe at 100% active time with hundreds of milliseconds of latency, while CPU
sits at 20%, is enough to freeze the editor or the agent host. host-watcher
measures that pressure, turns it into one verdict, and paces heavy commands
until the machine can take them.

- **Local-first.** No network, telemetry, external API, LLM call, database or
  mandatory daemon. State is a few JSON files in `~/.host-watcher`.
- **Ready to run.** Python 3.8+ standard library only. An optional single-file
  Go binary does the same with richer per-process I/O on Windows.
- **Cross-platform.** Windows, Linux, WSL and macOS, with every unmeasurable
  metric reported as `null` plus a reason, never guessed
  ([platform support](docs/platform-support.md)).
- **Safe cleanup.** Dry-run by default; never deletes active, shared, unowned
  or unverified data ([cleanup safety](docs/cleanup-safety.md)).
- **Agent-agnostic.** Hooks for Claude Code, Pi and any agent that can run a
  command; an `AGENTS.md` snippet for the rest ([integrations](docs/integrations.md)).

## Quick start

```bash
git clone https://github.com/bestagentkits/host-watcher
cd host-watcher

# Inspect resources
python scripts/host_status.py

# Start monitoring (foreground; or --detach / --stop)
python scripts/host_watch.py

# Run a heavy command
python scripts/host_gate.py -- npm run build

# Preview safe cleanup
python scripts/cache_cleanup.py --dry-run
```

Use `python3` where `python` is not on PATH. Nothing to install.

Example output (values vary):

```text
[ok] verdict go (linux, sampled at 2026-11-26T07:01:27Z)
cpu     11.0% busy, 8 cores
memory  61.9% used, 6.1 GB available of 16.0 GB
disk    nvme0n1  active 12.4%  latency 0.6 ms  write 3.10 MB/s  read 0.40 MB/s (host)
space   /home/dev/project 412.9 GB free of 953.9 GB
```

## Verdicts

| Verdict | Trigger (defaults) | What to do |
|---|---|---|
| `go` | below every throttle line | admit heavy jobs |
| `throttle` | disk active ≥ 70% or latency ≥ 50 ms, CPU ≥ 85%, memory ≥ 88% | at most one more heavy job |
| `unknown` | no pressure metric could be sampled | run heavy jobs one at a time |
| `clean` | a watched volume has < 25 GB free | free space before heavy work |
| `hold` | disk active ≥ 90% or latency ≥ 100 ms, CPU ≥ 95%, memory ≥ 95% | start nothing heavy; wait |

The verdict is the most severe signal (`hold` > `clean` > `unknown` >
`throttle` > `go`), from the **median** of several samples so one spike does
not gate. Every verdict exits 0; read it from the output or `--json`.
Thresholds are configurable ([configuration](docs/configuration.md)).

## Commands

| Python script | Go binary | Purpose |
|---|---|---|
| `scripts/host_status.py [--json] [--fresh] [--caches] [--path P] [--min-free-gb N]` | `host-watcher status` | one verdict now (reuses a fresh watcher snapshot) |
| `scripts/host_watch.py [--detach \| --stop] [--interval S] [--idle-exit S]` | `host-watcher watch` | one machine-wide watcher writing the snapshot and inbox |
| `scripts/host_gate.py [--max-wait S] [--no-low-priority] -- <cmd>` | `host-watcher gate -- <cmd>` | wait out `hold`/`clean`, then run at low priority |
| `scripts/host_inbox.py [--take] [--json]` | `host-watcher inbox` | this worktree's notice from the watcher |
| `scripts/cache_cleanup.py [--glob P --apply]` | `host-watcher caches` | cache inventory; safe temp-dir cleanup (Python only) |

Both implementations share the state directory, file formats and lock, so a
Go watcher serves Python callers and the reverse.

### `host_gate`: resource-aware execution

```bash
python scripts/host_gate.py -- go test ./...
python scripts/host_gate.py --max-wait 600 -- pnpm build
python scripts/host_gate.py --max-wait 0 -- make lint      # never wait, still low priority
```

- Waits only on `hold` and `clean`, with backoff 15 s, 30 s, then 60 s, one
  stderr line per wait. After `--max-wait` (default 1800 s) it runs anyway, so
  it never hangs a script.
- Runs the command directly (no shell) with your terminal's stdio at low
  priority: `nice 10` + idle I/O class (`ionice -c3`) on Linux,
  `taskpolicy -b` on macOS, below-normal priority class on Windows.
- Forwards Ctrl-C/SIGTERM/SIGHUP and returns the command's own exit code
  (128+N when a signal ended it). Exit 5 means it could not start; 2 is bad
  arguments.

### `host_watch` and the worktree inbox

The watcher samples every 5 s, writes `~/.host-watcher/status.json`, and
exits on its own after 30 minutes with no coding-agent process alive. When
the verdict is `throttle`, `clean` or `hold` and the top I/O processes run
inside a git worktree, it leaves that worktree a notice
(`~/.host-watcher/inbox/<hash>.json`). The agent in that worktree sees it
through its hook, `host_inbox.py`, or the next `host_gate.py` run. Notices
expire after 10 minutes. Per-process I/O is needed for notices; see
[platform support](docs/platform-support.md).

### `cache_cleanup`: inventory and safe cleanup

```bash
python scripts/cache_cleanup.py                                   # sizes + native prune commands
python scripts/cache_cleanup.py --glob 'my-agent-run-*'           # preview (default)
python scripts/cache_cleanup.py --glob 'my-agent-run-*' --apply   # delete verified candidates
```

Shared caches (Go, npm, pnpm, pip, yarn, cargo) are measured, never deleted;
the tool prints the native command that prunes each. Temp-dir cleanup needs an
explicit name pattern and removes a directory only when it is a plain
directory, owned by you, not named by any running process, and idle for
`--min-age` minutes. Everything else is listed as skipped with the reason.

## Optional Go binary

Download from [Releases](https://github.com/bestagentkits/host-watcher/releases)
(Linux, macOS, Windows; amd64 and arm64), or build it:

```bash
go install github.com/bestagentkits/host-watcher/cmd/host-watcher@latest
host-watcher status
host-watcher watch --detach
host-watcher gate -- npm run build
```

The binary adds per-process I/O attribution on Windows, PDH disk counters
without PowerShell, and starts children with low I/O priority on Windows.

## Agent integrations

| Agent | How | File |
|---|---|---|
| Claude Code | `UserPromptSubmit` + `PostToolUse` hook | [hooks/claude-code](hooks/claude-code) |
| Pi | extension on `tool_result` / `before_agent_start` | [hooks/pi/host-watcher.ts](hooks/pi/host-watcher.ts) |
| Codex and others | `AGENTS.md` instructions + `host_gate.py` | [hooks/codex/AGENTS.snippet.md](hooks/codex/AGENTS.snippet.md) |
| Any hook-capable agent | Node-free Python hook, JSON or text output | [hooks/generic](hooks/generic) |

Setup details: [docs/integrations.md](docs/integrations.md).

## Documentation

- [How it works (EN/VI)](https://bestagentkits.github.io/host-watcher/)
- [Architecture](docs/architecture.md): components, files, formats.
- [Configuration and thresholds](docs/configuration.md).
- [Platform support and limitations](docs/platform-support.md).
- [Cleanup safety](docs/cleanup-safety.md).
- [Integrations](docs/integrations.md).
- [Troubleshooting](docs/troubleshooting.md).

## Non-goals

host-watcher is a pacing gate, not a scheduler. It does not queue jobs,
predict load, rank agents or kill processes. It reports, waits and lowers
priority; decisions stay with you and your agents.

## Development

```bash
python -m unittest discover -s tests -v
go test ./...
node --test tests/host-pressure-reminder.test.cjs
```

CI runs all three on Linux, Windows and macOS. See
[CONTRIBUTING](.github/CONTRIBUTING.md). License: [MIT](LICENSE).
