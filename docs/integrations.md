# Agent integrations

Every integration does the same two things: tell the agent when the machine is
under pressure, and point heavy commands at the gate. Hooks never sample (they
must be fast); they read the watcher snapshot and the worktree inbox, stay
silent on `go`/`unknown`, remind once per verdict per session, and always exit
0 so they can never break an agent.

Start a watcher so hooks have something to read:

```bash
python scripts/host_watch.py --detach      # or: host-watcher watch --detach
```

## Claude Code

Two equivalent hooks; pick one.

**Node** (`hooks/claude-code/host-pressure-reminder.cjs`): also starts
`host-watcher watch --detach` when the snapshot is stale and the Go binary is
on PATH (at most once a minute; disable with `HOST_WATCHER_AUTOSTART=0`).
Merge [settings.example.json](../hooks/claude-code/settings.example.json)
into `~/.claude/settings.json` or `.claude/settings.json` and replace
`/ABSOLUTE/PATH/TO/host-watcher`.

**Python, no Node** (`hooks/generic/host_pressure_hook.py`): merge
[settings.python.example.json](../hooks/claude-code/settings.python.example.json).

Both register on `UserPromptSubmit` and on `PostToolUse` for `Bash|PowerShell`,
and return `hookSpecificOutput.additionalContext`.

Check by hand:

```bash
echo '{"cwd":"'"$PWD"'","session_id":"t","hook_event_name":"UserPromptSubmit"}' \
  | node hooks/claude-code/host-pressure-reminder.cjs
```

## Pi

Copy [hooks/pi/host-watcher.ts](../hooks/pi/host-watcher.ts) into your Pi
extensions and set `HOST_WATCHER_DIR` to this clone. It appends the reminder
to `bash` tool results and injects it as a hidden message before each agent
turn. It calls the Python hook, so Node is only needed for Pi itself.
Status: written against Pi's `tool_result` / `before_agent_start` extension
events; not run in this repository's CI.

## Codex and agents configured through instructions

Append [hooks/codex/AGENTS.snippet.md](../hooks/codex/AGENTS.snippet.md) to
the repository's `AGENTS.md` (Codex, OpenCode, Cursor and most others read
it) and replace `<host-watcher>` with the clone path. The agent then runs
heavy commands through `host_gate.py`, which works with no hook support at
all. If your agent supports command hooks that pass JSON on stdin, also wire
`hooks/generic/host_pressure_hook.py` as below.

## Any agent with command hooks

```bash
# JSON in Claude Code's hook shape (default)
echo '{"cwd":"'"$PWD"'","session_id":"s1"}' | python hooks/generic/host_pressure_hook.py
# Plain text; empty output when there is nothing to say
echo '{"cwd":"'"$PWD"'"}' | python hooks/generic/host_pressure_hook.py --format text
```

Without hooks at all, gate a step yourself:

```bash
sh hooks/generic/host-pressure-check.sh && npm run build      # exit 3 on clean/hold
pwsh hooks/generic/host-pressure-check.ps1                       # Windows
```

## Scripts, Makefiles and CI

- `examples/gate-in-makefile.mk`: `$(GATE) go test ./...`.
- `examples/package-json-scripts.json`: gated npm scripts.
- `examples/parallel-worktrees.sh`: one command across several worktrees, staggered and gated.
- `examples/github-actions-self-hosted.yml`: shared self-hosted runners.
