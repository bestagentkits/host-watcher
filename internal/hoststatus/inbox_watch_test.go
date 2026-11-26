package hoststatus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestagentkits/host-watcher/internal/lockfile"
)

func TestWorktreeHashVectors(t *testing.T) {
	cases := []struct {
		in   string
		fold bool
		norm string
	}{
		{`D:\Worktrees\x\`, true, "d:/worktrees/x"},
		{`D:/Worktrees//x/./y/..`, true, "d:/worktrees/x"},
		{`C:\`, true, "c:/"},
		{"/home/U/wt/", false, "/home/U/wt"},
		{`C:\Users\ÄİΣ\Wt`, true, "c:/users/ÄİΣ/wt"},
	}
	for _, c := range cases {
		if got := normalizeWorktree(c.in, c.fold); got != c.norm {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.norm)
		}
	}
	// sha256("d:/worktrees/x") first 16 hex; pinned so the JS hook can test
	// against the same vector.
	if got := worktreeHash(`D:\Worktrees\x`, true); got != "d49b00fbf4d3b90e" {
		t.Fatalf("hash = %q", got)
	}
	if worktreeHash(`D:\A`, true) != worktreeHash(`d:/a/`, true) {
		t.Fatal("case and separator variants must hash alike on Windows")
	}
}

func pressured(v Verdict, wt string) *Report {
	return &Report{Verdict: v, TopIO: []ProcIO{
		{PID: 7, Name: "go.exe", WriteMBs: 30, ReadMBs: 1, Worktree: wt},
		{PID: 8, Name: "other", WriteMBs: 1},
	}}
}

func TestUpdateInboxWritesAndDedupes(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "wt")
	t0 := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)

	if err := UpdateInbox(dir, pressured(VerdictGo, wt), t0); err != nil {
		t.Fatal(err)
	}
	if ReadInbox(dir, wt, t0) != nil {
		t.Fatal("go verdict must not write an entry")
	}
	if err := UpdateInbox(dir, pressured(VerdictThrottle, wt), t0); err != nil {
		t.Fatal(err)
	}
	e := ReadInbox(dir, wt, t0)
	if e == nil || e.Severity != 1 || len(e.Processes) != 1 || e.Processes[0].PID != 7 || e.Action == "" {
		t.Fatalf("entry = %+v", e)
	}
	// Same severity within 5 min: kept as is.
	_ = UpdateInbox(dir, pressured(VerdictThrottle, wt), t0.Add(time.Minute))
	if e := ReadInbox(dir, wt, t0.Add(time.Minute)); e.SampledAt != t0.Format(time.RFC3339) {
		t.Fatalf("deduped entry rewritten: %+v", e)
	}
	// Severity rises: rewritten.
	_ = UpdateInbox(dir, pressured(VerdictHold, wt), t0.Add(2*time.Minute))
	if e := ReadInbox(dir, wt, t0.Add(2*time.Minute)); e.Severity != 3 {
		t.Fatalf("hold not written: %+v", e)
	}
	// Older than 5 min: rewritten even at lower severity.
	_ = UpdateInbox(dir, pressured(VerdictThrottle, wt), t0.Add(8*time.Minute))
	if e := ReadInbox(dir, wt, t0.Add(8*time.Minute)); e.Severity != 1 {
		t.Fatalf("aged entry not refreshed: %+v", e)
	}
}

func TestInboxStaleCleanupAndTake(t *testing.T) {
	dir := t.TempDir()
	wt := "/w/a"
	t0 := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	_ = UpdateInbox(dir, pressured(VerdictHold, wt), t0)
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PruneInbox(dir, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.json")); !os.IsNotExist(err) {
		t.Fatal("unreadable entry not removed")
	}
	if ReadInbox(dir, wt, t0.Add(11*time.Minute)) != nil {
		t.Fatal("stale entry returned")
	}
	if _, err := os.Stat(InboxPath(dir, wt)); !os.IsNotExist(err) {
		t.Fatal("stale entry not removed by reader")
	}
	_ = UpdateInbox(dir, pressured(VerdictHold, wt), t0)
	if TakeInbox(dir, wt, t0) == nil || ReadInbox(dir, wt, t0) != nil {
		t.Fatal("take must return then delete")
	}
	if err := PruneInbox(filepath.Join(dir, "missing"), t0); err != nil {
		t.Fatalf("missing inbox dir: %v", err)
	}
}

func TestWatchSingletonSnapshotAndStop(t *testing.T) {
	p := PathsIn(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now
	ticks := make(chan struct{}, 100)
	opts := WatchOptions{
		Paths: p, Interval: 10 * time.Millisecond, IdleExit: time.Hour,
		Collect: func(context.Context) (*Report, error) {
			time.Sleep(10 * time.Millisecond)
			r := pressured(VerdictHold, "/w/a")
			r.SampledAt = now().UTC().Format(time.RFC3339)
			ticks <- struct{}{}
			return r, nil
		},
		AgentAlive: func() (bool, error) { return true, nil },
	}
	done := make(chan WatchExit, 1)
	go func() {
		exit, err := Watch(ctx, opts)
		if err != nil {
			t.Errorf("watch: %v", err)
		}
		done <- exit
	}()
	<-ticks
	<-ticks
	defer func(w time.Duration) { watchLockWait = w }(watchLockWait)
	watchLockWait = 200 * time.Millisecond
	if _, err := Watch(ctx, opts); !errors.Is(err, ErrWatcherRunning) {
		t.Fatalf("second watcher err = %v, want ErrWatcherRunning", err)
	}
	if pid := RunningPID(p); pid != os.Getpid() {
		t.Fatalf("RunningPID = %d, want %d", pid, os.Getpid())
	}
	r := ReadSnapshot(p.Snapshot, SnapshotMaxAge, time.Now())
	if r == nil || r.Source != SourceWatcher || r.SnapshotPath != p.Snapshot {
		t.Fatalf("snapshot = %+v", r)
	}
	if ReadInbox(p.Inbox, "/w/a", time.Now()) == nil {
		t.Fatal("watcher did not write the inbox entry")
	}
	pid, err := Stop(p, 5*time.Second)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("stop = %d, %v", pid, err)
	}
	if exit := <-done; exit != ExitStopped {
		t.Fatalf("exit = %q", exit)
	}
	if RunningPID(p) != 0 {
		t.Fatal("lock still held after stop")
	}
	if _, err := Stop(p, time.Second); !errors.Is(err, ErrWatcherNotRunning) {
		t.Fatalf("second stop err = %v", err)
	}
	if ReadSnapshot(p.Snapshot, SnapshotMaxAge, time.Now().Add(time.Minute)) != nil {
		t.Fatal("stale snapshot must read as nil")
	}
}

func TestWatchIdleExit(t *testing.T) {
	p := PathsIn(t.TempDir())
	clock := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	exit, err := Watch(context.Background(), WatchOptions{
		Paths: p, Interval: time.Millisecond, IdleExit: time.Minute,
		Now: func() time.Time { clock = clock.Add(20 * time.Second); return clock },
		Collect: func(context.Context) (*Report, error) {
			return &Report{Verdict: VerdictGo, SampledAt: clock.Format(time.RFC3339)}, nil
		},
		AgentAlive: func() (bool, error) { return false, nil },
	})
	if err != nil || exit != ExitIdle {
		t.Fatalf("exit = %q, %v", exit, err)
	}
	if _, err := os.Stat(p.PID); !os.IsNotExist(err) {
		t.Fatal("pid file left behind")
	}
}

// A sampler that fails at once must not spin: each iteration is paced to
// Interval.
func TestWatchPacesFailingSampler(t *testing.T) {
	p := PathsIn(t.TempDir())
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	exit, err := Watch(ctx, WatchOptions{
		Paths: p, Interval: 50 * time.Millisecond, IdleExit: time.Hour,
		Collect: func(context.Context) (*Report, error) {
			calls.Add(1)
			return nil, errors.New("no counters")
		},
		AgentAlive: func() (bool, error) { return true, nil },
	})
	if err != nil || exit != ExitCanceled {
		t.Fatalf("exit = %q, %v", exit, err)
	}
	// 300 ms at a 50 ms pace is about 6 calls; a spinning loop makes thousands.
	if n := calls.Load(); n < 2 || n > 10 {
		t.Fatalf("collect called %d times in 300ms at a 50ms interval", n)
	}
}

// A lock held briefly (as RunningPID's probe does) must not make a starting
// watcher report another watcher as running.
func TestWatchRetriesBrieflyHeldLock(t *testing.T) {
	p := PathsIn(t.TempDir())
	probe, err := lockfile.AcquirePath(context.Background(), p.Lock, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = probe.Unlock()
	}()
	clock := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	exit, err := Watch(context.Background(), WatchOptions{
		Paths: p, Interval: time.Millisecond, IdleExit: time.Minute,
		Now: func() time.Time { clock = clock.Add(time.Minute); return clock },
		Collect: func(context.Context) (*Report, error) {
			return &Report{Verdict: VerdictGo, SampledAt: clock.Format(time.RFC3339)}, nil
		},
		AgentAlive: func() (bool, error) { return false, nil },
	})
	if err != nil || exit != ExitIdle {
		t.Fatalf("exit = %q, %v; want idle after acquiring the released lock", exit, err)
	}
}

func TestTopIOScopeMarksWindowsAllIO(t *testing.T) {
	if topIOScope("windows", true) != TopIOScopeAllIO || topIOScope("linux", true) != "" || topIOScope("windows", false) != "" {
		t.Fatal("io_scope must be set only for Windows top_io")
	}
	dir := t.TempDir()
	r := pressured(VerdictHold, "/w/a")
	r.IOScope = TopIOScopeAllIO
	if err := UpdateInbox(dir, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if e := ReadInbox(dir, "/w/a", time.Now()); e == nil || e.IOScope != TopIOScopeAllIO {
		t.Fatalf("inbox entry = %+v, want io_scope copied", e)
	}
}
