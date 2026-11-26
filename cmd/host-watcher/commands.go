package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/bestagentkits/host-watcher/internal/hoststatus"
)

const (
	minWatchInterval = time.Second
	maxWatchInterval = 5 * time.Minute
	minIdleExit      = time.Minute
	stopTimeout      = 20 * time.Second
)

type watchResult struct {
	State        string `json:"state"`
	PID          int    `json:"pid,omitempty"`
	SnapshotPath string `json:"snapshot_path"`
	Detail       string `json:"detail,omitempty"`
}

func cmdWatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	interval, idleExit := hoststatus.DefaultWatchInterval, hoststatus.DefaultIdleExit
	var detach, stopFlag, asJSON bool
	fs := newFlags("watch")
	fs.DurationVar(&interval, "interval", interval, "time between snapshots (1s-5m)")
	fs.DurationVar(&idleExit, "idle-exit", idleExit, "exit after this long with no agent runtime alive (>= 1m)")
	fs.BoolVar(&detach, "detach", false, "start the watcher in the background and return")
	fs.BoolVar(&stopFlag, "stop", false, "stop the running watcher")
	fs.BoolVar(&asJSON, "json", false, "print a JSON envelope")
	if err := fs.Parse(args); err != nil {
		return exitErr(2, err)
	}
	switch {
	case detach && stopFlag:
		return exitErr(2, errors.New("--detach and --stop cannot be combined"))
	case interval < minWatchInterval || interval > maxWatchInterval:
		return exitErr(2, fmt.Errorf("--interval must be between %s and %s, got %s", minWatchInterval, maxWatchInterval, interval))
	case idleExit < minIdleExit:
		return exitErr(2, fmt.Errorf("--idle-exit must be at least %s, got %s", minIdleExit, idleExit))
	}
	paths, err := hoststatus.DefaultPaths()
	if err != nil {
		return exitErr(1, err)
	}
	var res watchResult
	switch {
	case stopFlag:
		res, err = stopWatcher(paths)
	case detach:
		res, err = detachWatcher(paths, interval, idleExit)
	default:
		var exit hoststatus.WatchExit
		exit, err = hoststatus.Watch(ctx, hoststatus.WatchOptions{Paths: paths, Interval: interval, IdleExit: idleExit, Log: stderr})
		switch {
		case errors.Is(err, hoststatus.ErrWatcherRunning):
			res, err = watchResult{State: "already-running", PID: hoststatus.RunningPID(paths), SnapshotPath: paths.Snapshot,
				Detail: "stop it with `host-watcher watch --stop`"}, nil
		case err == nil:
			res = watchResult{State: "exited", PID: os.Getpid(), SnapshotPath: paths.Snapshot, Detail: "reason: " + string(exit)}
		default:
			err = fmt.Errorf("%w; check that %s is writable", err, paths.Dir)
		}
	}
	if err != nil {
		return exitErr(1, err)
	}
	if asJSON {
		return writeJSON(stdout, "host.watch", res)
	}
	line := "host watcher " + res.State
	if res.PID > 0 {
		line += fmt.Sprintf(" (pid %d)", res.PID)
	}
	line += "\n  snapshot " + res.SnapshotPath + "\n"
	if res.Detail != "" {
		line += "  " + res.Detail + "\n"
	}
	_, err = io.WriteString(stdout, line)
	return err
}

func stopWatcher(paths hoststatus.Paths) (watchResult, error) {
	pid, err := hoststatus.Stop(paths, stopTimeout)
	if errors.Is(err, hoststatus.ErrWatcherNotRunning) {
		return watchResult{State: "not-running", SnapshotPath: paths.Snapshot}, nil
	}
	if err != nil {
		return watchResult{}, fmt.Errorf("%w; end the watcher with your OS tools, or delete %s when none is running", err, paths.Lock)
	}
	return watchResult{State: "stopped", PID: pid, SnapshotPath: paths.Snapshot}, nil
}

func detachWatcher(paths hoststatus.Paths, interval, idleExit time.Duration) (watchResult, error) {
	if pid := hoststatus.RunningPID(paths); pid != 0 {
		return watchResult{State: "already-running", PID: pid, SnapshotPath: paths.Snapshot,
			Detail: "stop it with `host-watcher watch --stop`"}, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return watchResult{}, fmt.Errorf("locate the host-watcher binary: %w", err)
	}
	if err := os.MkdirAll(paths.Dir, 0o750); err != nil {
		return watchResult{}, err
	}
	child := exec.Command(exe, "watch", "--interval", interval.String(), "--idle-exit", idleExit.String()) //nolint:gosec // re-launches this binary
	child.Dir = paths.Dir                                                                                  // never pin the caller's worktree directory
	if err := hoststatus.StartDetached(child); err != nil {
		return watchResult{}, fmt.Errorf("start background watcher: %w; run `host-watcher watch` in a separate terminal instead", err)
	}
	pid := child.Process.Pid
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	deadline := time.Now().Add(interval + 15*time.Second)
	for time.Now().Before(deadline) {
		if hoststatus.ReadSnapshot(paths.Snapshot, hoststatus.SnapshotMaxAge, time.Now()) != nil {
			return watchResult{State: "started", PID: pid, SnapshotPath: paths.Snapshot}, nil
		}
		select {
		case <-exited:
			if running := hoststatus.RunningPID(paths); running != 0 {
				return watchResult{State: "already-running", PID: running, SnapshotPath: paths.Snapshot}, nil
			}
			return watchResult{}, errors.New("background watcher exited at once; run `host-watcher watch` in the foreground to see why")
		case <-time.After(200 * time.Millisecond):
		}
	}
	return watchResult{State: "running", PID: pid, SnapshotPath: paths.Snapshot, Detail: "first snapshot not written yet"}, nil
}

func cmdGate(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	maxWait, lowPriority := defaultMaxWait, true
	fs := newFlags("gate")
	fs.DurationVar(&maxWait, "max-wait", maxWait, "longest time to wait for the host before running anyway (0 runs at once)")
	fs.BoolVar(&lowPriority, "low-priority", lowPriority, "run the command at low CPU and I/O priority")
	if err := fs.Parse(args); err != nil {
		return exitErr(2, err)
	}
	argv := fs.Args()
	if len(argv) == 0 {
		return exitErr(2, errors.New("no command given; usage: host-watcher gate [flags] -- <cmd> [args...]"))
	}
	if maxWait < 0 {
		return exitErr(2, fmt.Errorf("--max-wait must not be negative, got %s", maxWait))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	waitForHost(ctx, gateDeps{status: gateStatus, inbox: takeOwnInbox, sleep: sleepCtx, now: time.Now, stderr: stderr}, maxWait)
	interrupted := ctx.Err() != nil
	stop()
	if interrupted {
		return exitErr(exitGateStart, fmt.Errorf("interrupted while waiting; the command was not started. Rerun `host-watcher gate -- %s`", quoteArgv(argv)))
	}
	code, err := runChild(argv, lowPriority, stdin, stdout, stderr)
	if err != nil {
		return exitErr(exitGateStart, fmt.Errorf("could not start %s: %w; check the command name and PATH", argv[0], err))
	}
	if code != 0 {
		return &childExitError{code: code}
	}
	return nil
}

func cmdInbox(args []string, stdout io.Writer) error {
	var take, asJSON bool
	dir := ""
	fs := newFlags("inbox")
	fs.BoolVar(&take, "take", false, "delete the notice after reading it")
	fs.StringVar(&dir, "dir", "", "directory inside the worktree (default: current directory)")
	fs.BoolVar(&asJSON, "json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exitErr(2, err)
	}
	paths, err := hoststatus.DefaultPaths()
	if err != nil {
		return exitErr(1, err)
	}
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return exitErr(1, err)
		}
	}
	now := time.Now()
	_ = hoststatus.PruneInbox(paths.Inbox, now)
	wt := hoststatus.WorktreeRoot(dir)
	var e *hoststatus.InboxEntry
	if take {
		e = hoststatus.TakeInbox(paths.Inbox, wt, now)
	} else {
		e = hoststatus.ReadInbox(paths.Inbox, wt, now)
	}
	if asJSON {
		return writeJSON(stdout, "host.inbox", e)
	}
	if e == nil {
		_, err = fmt.Fprintln(stdout, "no notice for this worktree")
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s (%s): %s%s\n", e.Verdict, e.SampledAt, e.Action, inboxProcs(e))
	return err
}

func cmdCaches(ctx context.Context, args []string, stdout io.Writer) error {
	var asJSON bool
	fs := newFlags("caches")
	fs.BoolVar(&asJSON, "json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exitErr(2, err)
	}
	caches := hoststatus.CacheReport(ctx)
	if asJSON {
		return writeJSON(stdout, "host.caches", caches)
	}
	if len(caches) == 0 {
		_, err := fmt.Fprintln(stdout, "no known caches found")
		return err
	}
	var b strings.Builder
	for _, c := range caches {
		fmt.Fprintf(&b, "%-14s %8.2f GB  %s\n", c.Name, c.GB, c.Path)
	}
	b.WriteString("Inventory only. Preview owned temp-dir cleanup with `python scripts/cache_cleanup.py`.\n")
	_, err := io.WriteString(stdout, b.String())
	return err
}
