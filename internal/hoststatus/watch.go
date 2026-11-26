package hoststatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bestagentkits/host-watcher/internal/lockfile"
)

// Watcher defaults and limits.
const (
	SnapshotMaxAge       = 15 * time.Second
	DefaultWatchInterval = 5 * time.Second
	DefaultIdleExit      = 30 * time.Minute
	SnapshotKind         = "host.status"
)

// ErrWatcherRunning reports that another watcher holds the singleton lock.
var ErrWatcherRunning = errors.New("a host watcher is already running")

// ErrWatcherNotRunning reports that no watcher holds the singleton lock.
var ErrWatcherNotRunning = errors.New("no host watcher is running")

// Paths are the watcher's files, all under one directory.
type Paths struct {
	Dir      string // <state dir>
	Lock     string // held for the watcher's lifetime; never read
	PID      string // {"pid":N,"started_at":"..."} for reporting only
	Snapshot string // status.json: the host.status JSON envelope
	Stop     string // sentinel the loop polls to exit cleanly
	Inbox    string // per-worktree inbox entries
}

// PathsIn returns the watcher paths under dir.
func PathsIn(dir string) Paths {
	return Paths{
		Dir:      dir,
		Lock:     filepath.Join(dir, "watch.lock"),
		PID:      filepath.Join(dir, "watch.pid"),
		Snapshot: filepath.Join(dir, "status.json"),
		Stop:     filepath.Join(dir, "watch.stop"),
		Inbox:    filepath.Join(dir, "inbox"),
	}
}

// DefaultPaths resolves the state dir: $HOST_WATCHER_HOME when set, else
// ~/.host-watcher. The Python scripts use the same location and file formats.
func DefaultPaths() (Paths, error) {
	if dir := strings.TrimSpace(os.Getenv("HOST_WATCHER_HOME")); dir != "" {
		return PathsIn(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory (set HOST_WATCHER_HOME): %w", err)
	}
	return PathsIn(filepath.Join(home, ".host-watcher")), nil
}

type snapshotEnvelope struct {
	SchemaVersion int     `json:"schema_version"`
	Kind          string  `json:"kind"`
	Data          *Report `json:"data"`
}

// EncodeSnapshot renders r as the host.status envelope `host-watcher status
// --json` prints.
func EncodeSnapshot(r *Report) ([]byte, error) {
	b, err := json.MarshalIndent(snapshotEnvelope{SchemaVersion: 1, Kind: SnapshotKind, Data: r}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ReadSnapshot returns the watcher's report when it is no older than maxAge
// at now. A missing, stale or unreadable snapshot returns nil and no error:
// callers then sample directly.
func ReadSnapshot(path string, maxAge time.Duration, now time.Time) *Report {
	b, err := os.ReadFile(path) //nolint:gosec,forbidigo // host-watcher-owned snapshot, not kit content
	if err != nil {
		return nil
	}
	var env snapshotEnvelope
	if json.Unmarshal(b, &env) != nil || env.Kind != SnapshotKind || env.Data == nil {
		return nil
	}
	at, err := time.Parse(time.RFC3339, env.Data.SampledAt)
	if err != nil || now.Sub(at) > maxAge || at.After(now.Add(maxAge)) {
		return nil
	}
	env.Data.Source = SourceWatcher
	env.Data.SnapshotPath = path
	return env.Data
}

type pidRecord struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// RunningPID reports the pid of the live watcher, or 0 when none holds the
// lock. The lock, not the pid file, decides liveness.
func RunningPID(p Paths) int {
	l, err := lockfile.AcquirePath(context.Background(), p.Lock, 0)
	if err == nil || errors.Is(err, lockfile.ErrUnsupported) {
		_ = l.Unlock()
		return 0
	}
	if !errors.Is(err, lockfile.ErrBusy) {
		return 0
	}
	if pid := readPID(p.PID); pid > 0 {
		return pid
	}
	return -1 // locked, pid file not yet written
}

func readPID(path string) int {
	b, err := os.ReadFile(path) //nolint:gosec,forbidigo // host-watcher-owned pid file, not kit content
	if err != nil {
		return 0
	}
	var rec pidRecord
	if json.Unmarshal(b, &rec) != nil {
		return 0
	}
	return rec.PID
}

// WatchOptions configures Watch.
type WatchOptions struct {
	Paths    Paths
	Interval time.Duration
	IdleExit time.Duration
	// Log receives one line per non-fatal problem; nil discards.
	Log io.Writer
	// Collect and AgentAlive are injectable for tests.
	Collect    func(ctx context.Context) (*Report, error)
	AgentAlive func() (bool, error)
	Now        func() time.Time
}

// WatchExit says why Watch returned.
type WatchExit string

// Watch exit reasons.
const (
	ExitStopped  WatchExit = "stopped"
	ExitIdle     WatchExit = "idle"
	ExitCanceled WatchExit = "canceled"
)

// Watch holds the singleton lock and writes a snapshot plus inbox entries
// every interval until stopped, cancelled, or no agent runtime has been
// alive for IdleExit. It returns ErrWatcherRunning when another watcher
// holds the lock.
func Watch(ctx context.Context, o WatchOptions) (WatchExit, error) {
	if o.Interval <= 0 || o.IdleExit <= 0 {
		return "", fmt.Errorf("interval and idle exit must be positive")
	}
	logw := o.Log
	if logw == nil {
		logw = io.Discard
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	collect := o.Collect
	if collect == nil {
		collect = func(ctx context.Context) (*Report, error) {
			return Collect(ctx, Options{Samples: 1, Interval: o.Interval, MinFreeGB: DefaultMinFreeGB, TopIO: true})
		}
	}
	alive := o.AgentAlive
	if alive == nil {
		alive = AgentRuntimeAlive
	}

	lock, err := lockfile.AcquirePath(ctx, o.Paths.Lock, watchLockWait)
	switch {
	case errors.Is(err, lockfile.ErrBusy):
		return "", ErrWatcherRunning
	case errors.Is(err, lockfile.ErrUnsupported):
		_, _ = fmt.Fprintf(logw, "warning: %s cannot be locked; a second watcher would not be detected\n", o.Paths.Lock)
	case err != nil:
		return "", fmt.Errorf("lock %s: %w", o.Paths.Lock, err)
	}
	defer func() { _ = lock.Unlock() }()
	_ = os.Remove(o.Paths.Stop)
	rec, _ := json.Marshal(pidRecord{PID: os.Getpid(), StartedAt: now().UTC().Format(time.RFC3339)})
	if err := writeFileAtomic(o.Paths.PID, rec, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", o.Paths.PID, err)
	}
	defer func() { _ = os.Remove(o.Paths.PID) }()

	lastAgent := now()
	for {
		if ctx.Err() != nil {
			return ExitCanceled, nil
		}
		if _, err := os.Stat(o.Paths.Stop); err == nil {
			_ = os.Remove(o.Paths.Stop)
			return ExitStopped, nil
		}
		// Each iteration takes at least Interval, so a sampler that fails or
		// returns at once (no counters on this OS) can never spin.
		iterStart := time.Now()
		r, err := collect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ExitCanceled, nil
			}
			_, _ = fmt.Fprintf(logw, "warning: sampling failed: %v\n", err)
		} else {
			publish(o, r, now(), logw)
		}
		ok, aliveErr := alive()
		if aliveErr != nil || ok {
			lastAgent = now() // an unreadable process table never triggers idle exit
		}
		if now().Sub(lastAgent) >= o.IdleExit {
			return ExitIdle, nil
		}
		if rest := o.Interval - time.Since(iterStart); rest > 0 && sleepCtx(ctx, rest) != nil {
			return ExitCanceled, nil
		}
	}
}

// watchLockWait covers RunningPID's brief probe of the same lock, which a
// starting watcher would otherwise read as a live watcher.
var watchLockWait = 2 * time.Second

// publish writes the snapshot and the inbox notices for one report.
func publish(o WatchOptions, r *Report, at time.Time, logw io.Writer) {
	r.Source = SourceWatcher
	r.SnapshotPath = o.Paths.Snapshot
	if b, err := EncodeSnapshot(r); err != nil {
		_, _ = fmt.Fprintf(logw, "warning: encode snapshot: %v\n", err)
	} else if err := writeFileAtomic(o.Paths.Snapshot, b, 0o644); err != nil {
		// A reader holding the file open on Windows can make the rename
		// fail; the next interval retries.
		_, _ = fmt.Fprintf(logw, "warning: write %s: %v\n", o.Paths.Snapshot, err)
	}
	if err := UpdateInbox(o.Paths.Inbox, r, at); err != nil {
		_, _ = fmt.Fprintf(logw, "warning: update inbox: %v\n", err)
	}
}

// Stop asks the running watcher to exit through the stop sentinel and waits
// up to timeout for it to release the lock. Only when it does not, and the
// lock is still held, is the recorded pid killed. It returns the pid stopped.
func Stop(p Paths, timeout time.Duration) (int, error) {
	pid := RunningPID(p)
	if pid == 0 {
		_ = os.Remove(p.PID)
		return 0, ErrWatcherNotRunning
	}
	if err := os.MkdirAll(p.Dir, 0o750); err != nil {
		return pid, err
	}
	if err := os.WriteFile(p.Stop, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return pid, fmt.Errorf("write stop request %s: %w", p.Stop, err)
	}
	if waitUnlocked(p, timeout) {
		return pid, nil
	}
	if pid > 0 && RunningPID(p) == pid {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
		if waitUnlocked(p, 5*time.Second) {
			_ = os.Remove(p.Stop)
			_ = os.Remove(p.PID)
			return pid, nil
		}
	}
	if pid < 0 {
		return pid, fmt.Errorf("host watcher (pid not recorded) did not exit within %s", timeout)
	}
	return pid, fmt.Errorf("host watcher (pid %d) did not exit within %s", pid, timeout)
}

func waitUnlocked(p Paths, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if RunningPID(p) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// WorktreeRoot returns the nearest directory at or above dir holding `.git`,
// or "" when dir is not inside a worktree.
func WorktreeRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	root := gitRoot(abs, func(p string) bool { _, err := os.Stat(p); return err == nil })
	return strings.TrimSpace(root)
}
