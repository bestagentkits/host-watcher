# Troubleshooting

**Verdict is always `unknown` on macOS.** Expected when CPU and memory could
not be read either; check `reasons`. Disk activity is never measurable on
macOS ([platform support](platform-support.md)); CPU, memory and space still
decide the verdict when available.

**`disk unavailable: powershell not found` on Windows.** The Python sampler
uses PowerShell for CIM disk counters. Install PowerShell, or use the Go
binary, which reads PDH directly.

**Numbers in WSL look nothing like Task Manager.** WSL reports its virtual
disk (`scope: wsl-guest`). Run the Windows build on the Windows side for the
host disk.

**`host_status` shows `source: sampled` although a watcher runs.** The
snapshot is older than 15 s (watcher stuck or stopped), or you passed an
option the snapshot does not carry (`--fresh`, `--caches`, `--path`,
`--min-free-gb`, `--config`). Check `python scripts/host_watch.py --detach`:
it reports `already-running (pid N)` or starts a new one.

**`already-running` but nothing updates.** Stop and restart:
`python scripts/host_watch.py --stop && python scripts/host_watch.py --detach`.
If `--stop` times out, end the pid with your OS tools; the lock is released
when the process exits. Background Python watcher errors go to
`~/.host-watcher/watch.log`.

**The watcher exits after 30 minutes.** No coding-agent process was seen. Use
`--idle-exit 0` (Python) to keep it running, or a longer duration.

**The gate waits too long.** It waits only on `hold` and `clean`. Lower the
wait with `--max-wait`, skip it with `--max-wait 0`, or free space if the
reason is `space clean`. Tune thresholds in
[configuration](configuration.md).

**`host_gate.py` exits 5.** The command could not be found or started. Pass
the full path, and do not wrap it in quotes as one string: use
`-- npm run build`, not `-- "npm run build"`. For shell syntax, gate a shell
explicitly: `-- sh -c 'a && b'`.

**Cleanup skips everything with `process list unavailable`.** Your
environment hides other processes (container, `hidepid`). This is
deliberate: nothing is deleted when use cannot be verified.

**No inbox notices.** They need per-process I/O: Linux (both
implementations) or Windows with the Go watcher. None on macOS.

**`invalid config`.** The file named in the error is not valid JSON. Fix it
or unset `HOST_WATCHER_CONFIG`.
