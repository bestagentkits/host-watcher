package hoststatus

import (
	"strings"
	"testing"
)

func existsIn(set ...string) func(string) bool {
	m := map[string]bool{}
	for _, s := range set {
		m[s] = true
	}
	return func(p string) bool { return m[p] }
}

func TestGitRootPosixAndWindows(t *testing.T) {
	cases := []struct {
		path, want string
		exists     func(string) bool
	}{
		{"/home/u/wt/a/b", "/home/u/wt", existsIn("/home/u/wt/.git")},
		{"/home/u/wt/", "/home/u/wt", existsIn("/home/u/wt/.git")},
		{"/repo/x", "/", existsIn("//.git", "/.git")},
		{"/nowhere/x", "", existsIn()},
		{`D:\worktrees\proj\x\apps\cli`, `D:\worktrees\proj\x`, existsIn(`D:\worktrees\proj\x\.git`)},
		{`D:/worktrees/x/apps`, `D:\worktrees\x`, existsIn(`D:\worktrees\x\.git`)},
		{`C:\src\file.go`, `C:\`, existsIn(`C:\.git`)},
	}
	for _, c := range cases {
		if got := gitRoot(c.path, c.exists); got != c.want {
			t.Errorf("gitRoot(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestWorktreeForPrefersCwdThenCmdline(t *testing.T) {
	ex := existsIn("/w/one/.git", `D:\w\two\.git`)
	if got := worktreeFor("/w/one/sub", `go test D:\w\two\pkg`, ex); got != "/w/one" {
		t.Fatalf("cwd should win, got %q", got)
	}
	if got := worktreeFor("", `"C:\Go\bin\go.exe" build -o=D:\w\two\bin\app.exe ./...`, ex); got != `D:\w\two` {
		t.Fatalf("cmdline path, got %q", got)
	}
}

func TestRuntimeDetection(t *testing.T) {
	cases := []struct{ name, cmdline, want string }{
		{"claude.exe", "", RuntimeClaudeCode},
		{"codex", "", RuntimeCodex},
		{"node", "node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", RuntimeClaudeCode},
		{"node.exe", `C:\nodejs\node.exe C:\npm\node_modules\@openai\codex\bin\codex.js`, RuntimeCodex},
		{"bun", "bun /x/opencode/index.js", RuntimeOpenCode},
		{"node", "node /x/pi-coding-agent/dist/cli.js", RuntimePi},
		{"node", "node server.js", ""},
		{"go.exe", "go build", ""},
	}
	for _, c := range cases {
		tbl := &procTable{procs: map[int]*procInfo{1: {pid: 1, name: c.name, cmdline: c.cmdline, detailsLoaded: true}}}
		if got := tbl.agentRuntime(tbl.procs[1]); got != c.want {
			t.Errorf("runtime(%q, %q) = %q, want %q", c.name, c.cmdline, got, c.want)
		}
	}
}

func table(procs ...*procInfo) *procTable {
	t := &procTable{procs: map[int]*procInfo{}}
	for _, p := range procs {
		p.detailsLoaded = true
		t.procs[p.pid] = p
	}
	return t
}

func TestTopIORanksAndAttributes(t *testing.T) {
	prev := table(
		&procInfo{pid: 10, ppid: 1, name: "claude.exe"},
		&procInfo{pid: 20, ppid: 10, name: "bash.exe"},
		&procInfo{pid: 30, ppid: 20, name: "go.exe", ioOK: true, writeB: 0, readB: 0},
		&procInfo{pid: 40, ppid: 2, name: "pwsh.exe", ioOK: true},
		&procInfo{pid: 50, ppid: 0, name: "svc.exe", ioOK: true},
		&procInfo{pid: 60, ppid: 0, name: "old.exe", ioOK: true, startTimeTicks: 1},
	)
	cur := table(
		&procInfo{pid: 10, ppid: 1, name: "claude.exe"},
		&procInfo{pid: 20, ppid: 10, name: "bash.exe", cwd: `D:\w\a`},
		&procInfo{pid: 30, ppid: 20, name: "go.exe", ioOK: true, writeB: 60e6, readB: 2e6},
		&procInfo{pid: 40, ppid: 2, name: "pwsh.exe", ioOK: true, writeB: 1e6},
		&procInfo{pid: 50, ppid: 0, name: "svc.exe", ioOK: true, readB: 4e6},
		&procInfo{pid: 60, ppid: 0, name: "old.exe", ioOK: true, startTimeTicks: 2, writeB: 9e9}, // pid reused
	)
	got := topIO(prev, cur, 2, 5, existsIn(`D:\w\a\.git`))
	if len(got) != 3 {
		t.Fatalf("got %d rows: %+v", len(got), got)
	}
	if got[0].PID != 30 || got[0].WriteMBs != 30 || got[0].ReadMBs != 1 {
		t.Fatalf("top row = %+v", got[0])
	}
	if got[0].Runtime != RuntimeClaudeCode || got[0].Worktree != `D:\w\a` {
		t.Fatalf("attribution = %+v", got[0])
	}
	if got[1].PID != 50 || got[1].Runtime != RuntimeUnknown {
		t.Fatalf("second row = %+v", got[1])
	}
	if got[2].PID != 40 || got[2].Runtime != RuntimeTerminal {
		t.Fatalf("third row = %+v", got[2])
	}
}

func TestAncestorsStopOnCycle(t *testing.T) {
	tbl := table(&procInfo{pid: 1, ppid: 2, name: "a"}, &procInfo{pid: 2, ppid: 1, name: "b"})
	if n := len(tbl.ancestors(tbl.procs[1])); n != 2 {
		t.Fatalf("ancestors = %d, want 2", n)
	}
}

func TestParseProcPidStat(t *testing.T) {
	line := "1234 (my (odd) proc) S 77 1234 1234 0 -1 4194560 100 0 0 0 5 3 0 0 20 0 1 0 98765 1000 100 18446744073709551615"
	name, ppid, start, err := parseProcPidStat(line)
	if err != nil || name != "my (odd) proc" || ppid != 77 || start != 98765 {
		t.Fatalf("got %q %d %d %v", name, ppid, start, err)
	}
	if _, _, _, err := parseProcPidStat("garbage"); err == nil {
		t.Fatal("want error")
	}
}

func TestParseProcPidIO(t *testing.T) {
	r, w, err := parseProcPidIO("rchar: 1\nwchar: 2\nsyscr: 3\nsyscw: 4\nread_bytes: 4096\nwrite_bytes: 8192\ncancelled_write_bytes: 0\n")
	if err != nil || r != 4096 || w != 8192 {
		t.Fatalf("got %d %d %v", r, w, err)
	}
	if _, _, err := parseProcPidIO("rchar: 1\n"); err == nil {
		t.Fatal("want error for missing fields")
	}
}

func TestParsePs(t *testing.T) {
	out := "    1     0 launchd          /sbin/launchd\n  501     1 node             node /opt/@anthropic-ai/claude-code/cli.js --resume\n"
	procs, err := parsePs(out)
	if err != nil || len(procs) != 2 {
		t.Fatalf("parsePs: %v %v", procs, err)
	}
	p := procs[501]
	if p.ppid != 1 || p.name != "node" || !strings.Contains(p.cmdline, "claude-code") {
		t.Fatalf("row = %+v", p)
	}
	tbl := &procTable{procs: procs}
	if !tbl.agentAlive() {
		t.Fatal("agentAlive should see the node-hosted Claude Code")
	}
}
