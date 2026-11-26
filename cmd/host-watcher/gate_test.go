package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bestagentkits/host-watcher/internal/hoststatus"
)

// TestMain doubles as a child process for exit-code passthrough tests.
func TestMain(m *testing.M) {
	if code := os.Getenv("HOSTCMD_TEST_EXIT"); code != "" {
		n, _ := strconv.Atoi(code)
		_, _ = os.Stdout.WriteString("child ran\n")
		os.Exit(n)
	}
	os.Exit(m.Run())
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.t = c.t.Add(d)
	return nil
}

func deps(clock *fakeClock, verdicts []hoststatus.Verdict, inbox *hoststatus.InboxEntry, stderr *bytes.Buffer) (gateDeps, *int) {
	calls := 0
	return gateDeps{
		status: func(context.Context) (*hoststatus.Report, error) {
			v := verdicts[min(calls, len(verdicts)-1)]
			calls++
			return &hoststatus.Report{Verdict: v, Reasons: []string{"disk hold: active 97%"},
				TopIO: []hoststatus.ProcIO{{PID: 9, Name: "go.exe", WriteMBs: 40, Worktree: "/w/a", Runtime: "codex"}}}, nil
		},
		inbox: func() *hoststatus.InboxEntry {
			e := inbox
			inbox = nil
			return e
		},
		sleep: clock.sleep, now: clock.now, stderr: stderr,
	}, &calls
}

func TestGateWaitsWhileHoldThenRuns(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	var stderr bytes.Buffer
	d, calls := deps(clock, []hoststatus.Verdict{hoststatus.VerdictHold, hoststatus.VerdictHold, hoststatus.VerdictThrottle}, nil, &stderr)
	waitForHost(context.Background(), d, 30*time.Minute)
	if *calls != 3 {
		t.Fatalf("status calls = %d, want 3", *calls)
	}
	if got := clock.t.Sub(time.Unix(0, 0)); got != 45*time.Second {
		t.Fatalf("waited %s, want 45s (15s then 30s backoff)", got)
	}
	out := stderr.String()
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, "hold (disk hold: active 97%)") ||
		!strings.Contains(out, "top I/O go.exe pid 9") || !strings.Contains(out, "host-watcher gate --max-wait 0") {
		t.Fatalf("stderr = %q", out)
	}
}

func TestGateMaxWaitRunsAnyway(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	var stderr bytes.Buffer
	d, _ := deps(clock, []hoststatus.Verdict{hoststatus.VerdictClean}, nil, &stderr)
	waitForHost(context.Background(), d, 100*time.Second)
	if got := clock.t.Sub(time.Unix(0, 0)); got != 100*time.Second {
		t.Fatalf("waited %s, want exactly --max-wait 100s", got)
	}
	out := stderr.String()
	if !strings.Contains(out, "running anyway") || !strings.Contains(out, "host-watcher caches") {
		t.Fatalf("stderr = %q", out)
	}
}

func TestGateUnknownAndErrorsDoNotBlock(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	var stderr bytes.Buffer
	d, _ := deps(clock, []hoststatus.Verdict{hoststatus.VerdictUnknown}, nil, &stderr)
	waitForHost(context.Background(), d, time.Hour)
	d.status = func(context.Context) (*hoststatus.Report, error) { return nil, errors.New("boom") }
	waitForHost(context.Background(), d, time.Hour)
	if !clock.t.Equal(time.Unix(0, 0)) || !strings.Contains(stderr.String(), "running without waiting") {
		t.Fatalf("waited %s, stderr %q", clock.t.Sub(time.Unix(0, 0)), stderr.String())
	}
}

func TestGatePrintsInboxOnce(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	var stderr bytes.Buffer
	entry := &hoststatus.InboxEntry{Verdict: hoststatus.VerdictHold, SampledAt: "2026-11-26T00:00:00Z", Action: "Pause heavy builds.",
		Processes: []hoststatus.InboxProcess{{PID: 7, Name: "go.exe", WriteMBs: 30}}}
	d, _ := deps(clock, []hoststatus.Verdict{hoststatus.VerdictGo}, entry, &stderr)
	waitForHost(context.Background(), d, time.Hour)
	waitForHost(context.Background(), d, time.Hour)
	if strings.Count(stderr.String(), "notice for this worktree") != 1 || !strings.Contains(stderr.String(), "go.exe pid 7") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunChildPassesExitCode(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{0, 3} {
		t.Setenv("HOSTCMD_TEST_EXIT", strconv.Itoa(want))
		for _, low := range []bool{true, false} {
			var out, errOut bytes.Buffer
			code, err := runChild([]string{exe}, low, strings.NewReader(""), &out, &errOut)
			if err != nil || code != want || !strings.Contains(out.String(), "child ran") {
				t.Fatalf("low=%v: code %d err %v out %q stderr %q", low, code, err, out.String(), errOut.String())
			}
		}
	}
}

func TestRunChildStartFailure(t *testing.T) {
	var out bytes.Buffer
	if _, err := runChild([]string{"host-watcher-no-such-command"}, true, nil, &out, &out); err == nil {
		t.Fatal("want start error")
	}
}

func TestGateCommandPassthroughAndValidation(t *testing.T) {
	t.Setenv("HOST_WATCHER_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOSTCMD_TEST_EXIT", "4")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"gate", "--max-wait", "0", "--", exe, "--flag-for-child"}, nil, &stdout, &stderr); code != 4 {
		t.Fatalf("exit = %d, want child exit 4 (stderr %q)", code, stderr.String())
	}
	if code := run([]string{"gate", "--max-wait", "-1s", "--", exe}, nil, &stdout, &stderr); code != 2 {
		t.Fatalf("negative --max-wait exit = %d, want 2", code)
	}
	if code := run([]string{"gate"}, nil, &stdout, &stderr); code != 2 {
		t.Fatalf("missing command exit = %d, want 2", code)
	}
}

func TestWatchFlagValidation(t *testing.T) {
	t.Setenv("HOST_WATCHER_HOME", t.TempDir())
	for _, args := range [][]string{
		{"watch", "--detach", "--stop"},
		{"watch", "--interval", "1ms"},
		{"watch", "--idle-exit", "1s"},
	} {
		var out bytes.Buffer
		if code := run(args, nil, &out, &out); code != 2 {
			t.Fatalf("run(%v) = %d, want 2", args, code)
		}
	}
}

func TestWatchStopWhenNotRunning(t *testing.T) {
	t.Setenv("HOST_WATCHER_HOME", t.TempDir())
	var stdout bytes.Buffer
	if code := run([]string{"watch", "--stop", "--json"}, nil, &stdout, &stdout); code != 0 {
		t.Fatalf("exit %d: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"state": "not-running"`) || !strings.Contains(stdout.String(), "host.watch") {
		t.Fatalf("out = %q", stdout.String())
	}
}

func TestInboxEmpty(t *testing.T) {
	t.Setenv("HOST_WATCHER_HOME", t.TempDir())
	var stdout bytes.Buffer
	if code := run([]string{"inbox", "--json"}, nil, &stdout, &stdout); code != 0 || !strings.Contains(stdout.String(), `"data": null`) {
		t.Fatalf("exit %d out %q", code, stdout.String())
	}
}

func TestGateMaxWaitZeroSkipsSampling(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	var stderr bytes.Buffer
	d, calls := deps(clock, []hoststatus.Verdict{hoststatus.VerdictHold}, nil, &stderr)
	waitForHost(context.Background(), d, 0)
	if *calls != 0 || !clock.t.Equal(time.Unix(0, 0)) || stderr.Len() != 0 {
		t.Fatalf("status calls %d, waited %s, stderr %q", *calls, clock.t.Sub(time.Unix(0, 0)), stderr.String())
	}
}

func TestQuoteArgv(t *testing.T) {
	argv := []string{"go", "test", "./...", "-run", "Foo Bar", "it's", `C:\x\y.exe`, ""}
	if got, want := quoteArgvFor("windows", argv), `go test ./... -run 'Foo Bar' 'it''s' C:\x\y.exe ''`; got != want {
		t.Fatalf("windows quoteArgv = %s, want %s", got, want)
	}
	if got, want := quoteArgvFor("linux", argv), `go test ./... -run 'Foo Bar' 'it'\''s' C:\x\y.exe ''`; got != want {
		t.Fatalf("posix quoteArgv = %s, want %s", got, want)
	}
}

func TestOffenderLinesSayIOAndPreferWrite(t *testing.T) {
	r := &hoststatus.Report{Verdict: hoststatus.VerdictHold, IOScope: hoststatus.TopIOScopeAllIO,
		TopIO: []hoststatus.ProcIO{{PID: 9, Name: "svc.exe", WriteMBs: 2, ReadMBs: 80}}}
	got := gateDetail(r)
	if !strings.Contains(got, "top I/O svc.exe pid 9 I/O write 2.0 MB/s") || !strings.Contains(got, "Windows counts network and pipes") {
		t.Fatalf("gateDetail = %q", got)
	}
	e := &hoststatus.InboxEntry{IOScope: hoststatus.TopIOScopeAllIO,
		Processes: []hoststatus.InboxProcess{{PID: 7, Name: "rd.exe", ReadMBs: 12}}}
	if got := inboxProcs(e); !strings.Contains(got, "rd.exe pid 7 I/O read 12.0 MB/s") || !strings.Contains(got, "all I/O") ||
		strings.Contains(got, "disk") {
		t.Fatalf("inboxProcs = %q", got)
	}
}
