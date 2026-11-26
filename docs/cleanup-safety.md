# Cleanup safety

`cache_cleanup.py` is dry-run by default. It deletes only with `--apply`, and
only directories it has verified. The Go binary's `caches` command is
inventory-only.

## Shared caches are never deleted

Go build and module caches, golangci-lint, npm, pnpm, pip, yarn and the cargo
registry are shared by every project and agent on the machine. The tool
measures them and prints the native prune command (`go clean -cache`,
`pnpm store prune`, ...) for you to run when nothing is building.

## Temp-dir cleanup rules

A directory under `--root` (default: the system temp dir) is a candidate only
when **all** of these hold:

| Check | Rejected as |
|---|---|
| its name matches an explicit `--glob` (at least 3 literal characters, no path separators) | not considered |
| it is a plain directory, not a symlink or junction | `not a plain directory` |
| it is **owned** by you: same uid on Unix; inside `%USERPROFILE%` on Windows | `owned by another user` |
| the process list could be read | `process list unavailable` (nothing is deleted) |
| no running process names it in its command line or working directory, in any slash form or by its basename | `referenced by a running process` |
| nothing in its top two levels changed for `--min-age` minutes (default 60) | `modified N min ago` |

Removal never follows links and never changes permissions outside the tree;
read-only files inside it are made writable so the tree can go.

## Known limits

- On macOS and Windows only command lines are visible, not each process's
  working directory. A process that `cd`-ed into a directory without naming it
  is not seen; the age check is the remaining guard. Pick `--min-age` longer
  than your longest idle step.
- "Owned" means owned by your user, not created by a specific agent. Use a
  name pattern only your tools produce (for example `myagent-run-*`).
- `--root` is not restricted; pointing it at a shared location is safe only
  because of the ownership and reference checks above.

Always run once without `--apply` and read the list.
