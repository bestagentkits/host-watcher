package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bestagentkits/host-watcher/internal/hoststatus"
)

const (
	defaultMaxWait = 30 * time.Minute
	exitGateStart  = 5
)

// gateBackoff is the wait sequence between status checks; the last value
// repeats.
var gateBackoff = []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second}

// childExitError passes the command's own non-zero exit status through
// without an error box: the command already reported its failure.
type childExitError struct{ code int }

func (e *childExitError) Error() string          { return fmt.Sprintf("command exited with status %d", e.code) }
func (e *childExitError) ExitCode() int          { return e.code }
func (e *childExitError) SuppressErrorBox() bool { return true }

// gateDeps are the gate's effects, injectable for tests.
type gateDeps struct {
	status func(ctx context.Context) (*hoststatus.Report, error)
	inbox  func() *hoststatus.InboxEntry
	sleep  func(ctx context.Context, d time.Duration) error
	now    func() time.Time
	stderr io.Writer
}

// waitForHost blocks while the verdict is hold or clean, up to maxWait,
// printing one stderr line per backoff. It never fails: an unreadable status
// does not block.
func waitForHost(ctx context.Context, d gateDeps, maxWait time.Duration) {
	if e := d.inbox(); e != nil {
		_, _ = fmt.Fprintf(d.stderr, "host-watcher gate: notice for this worktree (%s, %s): %s%s\n",
			e.Verdict, e.SampledAt, e.Action, inboxProcs(e))
	}
	if maxWait == 0 {
		return // --max-wait 0: run at once, without sampling
	}
	start := d.now()
	for attempt := 0; ; attempt++ {
		r, err := d.status(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(d.stderr, "host-watcher gate: warning: host status unavailable (%v); running without waiting\n", err)
			return
		}
		if r.Verdict != hoststatus.VerdictHold && r.Verdict != hoststatus.VerdictClean {
			return
		}
		waited := d.now().Sub(start)
		remaining := maxWait - waited
		if remaining <= 0 {
			_, _ = fmt.Fprintf(d.stderr, "host-watcher gate: warning: still %s after waiting %s (--max-wait); running anyway%s\n",
				r.Verdict, waited.Round(time.Second), gateDetail(r))
			return
		}
		pause := gateBackoff[min(attempt, len(gateBackoff)-1)]
		if pause > remaining {
			pause = remaining
		}
		_, _ = fmt.Fprintf(d.stderr, "host-watcher gate: %s%s; waiting %s (%s of %s used); skip with `host-watcher gate --max-wait 0 -- <cmd>`\n",
			r.Verdict, gateDetail(r), pause, waited.Round(time.Second), maxWait)
		if d.sleep(ctx, pause) != nil {
			return
		}
	}
}

func inboxProcs(e *hoststatus.InboxEntry) string {
	if len(e.Processes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(e.Processes))
	for _, p := range e.Processes {
		parts = append(parts, fmt.Sprintf("%s pid %d %s", p.Name, p.PID, ioRate(p.WriteMBs, p.ReadMBs)))
	}
	return " [" + strings.Join(parts, ", ") + "]" + ioScopeNote(e.IOScope)
}

// ioRate names a process's I/O rate, preferring write bytes (what fills and
// saturates a disk) and falling back to read bytes when it wrote nothing.
func ioRate(writeMBs, readMBs float64) string {
	if writeMBs > 0 || readMBs <= 0 {
		return fmt.Sprintf("I/O write %.1f MB/s", writeMBs)
	}
	return fmt.Sprintf("I/O read %.1f MB/s", readMBs)
}

func ioScopeNote(scope string) string {
	if scope == "" {
		return ""
	}
	return " (" + scope + ")"
}

// gateDetail names the verdict's reasons, the top I/O offender, and for a
// clean verdict the cleanup hint.
func gateDetail(r *hoststatus.Report) string {
	var b strings.Builder
	if len(r.Reasons) > 0 {
		b.WriteString(" (" + strings.Join(r.Reasons, "; ") + ")")
	}
	if len(r.TopIO) > 0 {
		t := r.TopIO[0]
		fmt.Fprintf(&b, "; top I/O %s pid %d %s", t.Name, t.PID, ioRate(t.WriteMBs, t.ReadMBs))
		if t.Worktree != "" {
			b.WriteString(" in " + t.Worktree)
		}
		if t.Runtime != "" && t.Runtime != hoststatus.RuntimeUnknown {
			b.WriteString(" (" + t.Runtime + ")")
		}
		b.WriteString(ioScopeNote(r.IOScope))
	}
	if r.Verdict == hoststatus.VerdictClean {
		b.WriteString("; free space: see `host-watcher caches`")
	}
	return b.String()
}

func gateStatus(ctx context.Context) (*hoststatus.Report, error) {
	if paths, err := hoststatus.DefaultPaths(); err == nil {
		if r := hoststatus.ReadSnapshot(paths.Snapshot, hoststatus.SnapshotMaxAge, time.Now()); r != nil {
			return r, nil
		}
	}
	return hoststatus.Collect(ctx, hoststatus.Options{
		Samples: hoststatus.DefaultSamples, Interval: hoststatus.DefaultInterval,
		MinFreeGB: hoststatus.DefaultMinFreeGB, TopIO: true,
	})
}

func takeOwnInbox() *hoststatus.InboxEntry {
	paths, err := hoststatus.DefaultPaths()
	if err != nil {
		return nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	now := time.Now()
	_ = hoststatus.PruneInbox(paths.Inbox, now)
	return hoststatus.TakeInbox(paths.Inbox, hoststatus.WorktreeRoot(wd), now)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runChild runs argv with this process's stdio and returns its exit status.
// host-watcher forwards interrupts and termination signals to the child and keeps
// waiting so the status is always passed on.
func runChild(argv []string, lowPriority bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c := exec.Command(argv[0], argv[1:]...) //nolint:gosec // running the user's command is this command's purpose
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	// SIGINT, SIGTERM and SIGHUP are forwarded so a harness timeout or
	// interrupt aimed at host-watcher does not leave the command running.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	if !lowPriority {
		// The low-priority Linux start forks from a thread that then exits,
		// which would fire a parent-death signal at once; only a plain start
		// can carry it.
		setParentDeathSignal(c)
	}
	defer signal.Stop(sigs)
	var err error
	if lowPriority {
		var warnings []string
		warnings, err = hoststatus.StartLowPriority(c)
		for _, w := range warnings {
			_, _ = fmt.Fprintf(stderr, "host-watcher gate: warning: %s\n", w)
		}
	} else {
		err = c.Start()
	}
	if err != nil {
		return 0, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sigs:
				// Forwarded too for SIGINT, which a terminal already delivers
				// to the child's process group but a harness signalling only
				// host-watcher does not. Windows cannot signal a process: an error is ignored.
				_ = c.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	waitErr := c.Wait()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return childStatus(exitErr), nil
	}
	if waitErr != nil {
		return 0, waitErr
	}
	return 0, nil
}

// quoteArgv renders argv for a copy-paste remediation in this OS's shell:
// plain words as is, anything else single-quoted, with an embedded quote
// doubled for PowerShell or closed, escaped and reopened for POSIX shells.
func quoteArgv(argv []string) string { return quoteArgvFor(runtime.GOOS, argv) }

func quoteArgvFor(goos string, argv []string) string {
	inner := `'\''`
	if goos == "windows" {
		inner = "''"
	}
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.IndexFunc(a, unsafeShellRune) < 0 {
			parts[i] = a
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(a, "'", inner) + "'"
	}
	return strings.Join(parts, " ")
}

func unsafeShellRune(r rune) bool {
	safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,\\", r)
	return !safe
}
