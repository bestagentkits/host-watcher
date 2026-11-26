# Contributing

Issues and pull requests are welcome.

- Keep the Python scripts standard-library only (Python 3.8+), and the Go
  module limited to `golang.org/x/sys`.
- No network calls, telemetry or background services beyond the opt-in
  watcher.
- The Python and Go implementations share file formats, the lock and the
  worktree hash. Change both, and keep the shared test vectors passing.
- Cleanup must stay dry-run by default and must never delete shared,
  unowned, active or unverified data.
- Run before pushing:

```bash
python -m unittest discover -s tests -v
go vet ./... && go test ./...
node --test tests/host-pressure-reminder.test.cjs
```
