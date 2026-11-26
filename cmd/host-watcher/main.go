// Command host-watcher reports host resource pressure (CPU, memory, disk
// activity, free space) as one verdict, keeps an optional background
// watcher, and runs heavy commands only when the host can take them.
//
// It is local-only: no network, no telemetry. State lives in
// $HOST_WATCHER_HOME (default ~/.host-watcher) and is shared with the
// Python scripts in scripts/.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/bestagentkits/host-watcher/internal/hoststatus"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `host-watcher: monitor host resource contention for parallel coding agents.

Usage:
  host-watcher status [--json] [--fresh] [--caches] [--samples N] [--interval D]
                      [--min-free-gb GB] [--path P ...] [--write-cache FILE]
  host-watcher watch  [--interval D] [--idle-exit D] [--detach | --stop] [--json]
  host-watcher gate   [--max-wait D] [--low-priority=false] -- <cmd> [args...]
  host-watcher inbox  [--take] [--dir WORKTREE] [--json]
  host-watcher caches [--json]
  host-watcher version

Thresholds: $HOST_WATCHER_CONFIG or $HOST_WATCHER_HOME/config.json (shared with scripts/).
Verdicts: go, throttle, unknown, clean, hold. Every verdict exits 0.
Docs: https://github.com/bestagentkits/host-watcher
`

// cliError carries an exit code to main.
type cliError struct {
	code int
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }

func exitErr(code int, err error) error { return &cliError{code: code, err: err} }

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err := hoststatus.LoadConfig(hoststatus.ConfigPath()); err != nil {
		fmt.Fprintln(stderr, "host-watcher:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch args[0] {
	case "status":
		err = cmdStatus(ctx, args[1:], stdout)
	case "watch":
		err = cmdWatch(ctx, args[1:], stdout, stderr)
	case "gate":
		stop() // the gate handles its own interrupts and forwards them to the child
		err = cmdGate(args[1:], stdin, stdout, stderr)
	case "inbox":
		err = cmdInbox(args[1:], stdout)
	case "caches":
		err = cmdCaches(ctx, args[1:], stdout)
	case "version", "--version":
		fmt.Fprintln(stdout, "host-watcher", version)
	default:
		err = exitErr(2, fmt.Errorf("unknown command %q; run `host-watcher --help`", args[0]))
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var ce *cliError
	if errors.As(err, &ce) {
		if errors.Is(ce.err, flag.ErrHelp) {
			return 0
		}
		if ce.code != 0 && !isChildExit(ce.err) {
			fmt.Fprintln(stderr, "host-watcher:", ce.err)
		}
		return ce.code
	}
	var child *childExitError
	if errors.As(err, &child) {
		return child.code
	}
	fmt.Fprintln(stderr, "host-watcher:", err)
	return 1
}

func isChildExit(err error) bool {
	var child *childExitError
	return errors.As(err, &child)
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("host-watcher "+name, flag.ContinueOnError)
	return fs
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func writeJSON(w io.Writer, kind string, data any) error {
	b, err := json.MarshalIndent(map[string]any{"schema_version": 1, "kind": kind, "data": data}, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
