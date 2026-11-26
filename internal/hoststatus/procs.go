package hoststatus

import (
	"regexp"
	"sort"
	"strings"
)

// Runtime labels attributed to a process from its ancestor chain.
const (
	RuntimeClaudeCode = "claude-code"
	RuntimeCodex      = "codex"
	RuntimeOpenCode   = "opencode"
	RuntimePi         = "pi"
	RuntimeCursor     = "cursor"
	RuntimeTerminal   = "terminal"
	RuntimeUnknown    = "unknown"
)

// TopIOLimit is how many processes top_io lists.
const TopIOLimit = 5

const maxAncestors = 32

// TopIOScopeAllIO marks top_io rates that count every byte a process moves:
// Windows GetProcessIoCounters includes network and pipe transfers, not only
// disk.
const TopIOScopeAllIO = "all I/O (Windows counts network and pipes)"

// topIOScope returns the io_scope for top_io rates sampled on goos.
func topIOScope(goos string, haveTopIO bool) string {
	if haveTopIO && goos == "windows" {
		return TopIOScopeAllIO
	}
	return ""
}

// ProcIO is one process's I/O rate over the sampling window, attributed to a
// worktree and the agent runtime it runs under.
type ProcIO struct {
	PID      int     `json:"pid"`
	Name     string  `json:"name"`
	WriteMBs float64 `json:"write_mb_s"`
	ReadMBs  float64 `json:"read_mb_s"`
	Cwd      string  `json:"cwd,omitempty"`
	Worktree string  `json:"worktree,omitempty"`
	Runtime  string  `json:"runtime"`
}

// procInfo is one process-table row. cmdline and cwd may be filled lazily by
// the table's loader because they are costly on some platforms.
type procInfo struct {
	pid, ppid      int
	name           string
	cmdline, cwd   string
	readB, writeB  uint64
	ioOK           bool
	detailsLoaded  bool
	startTimeTicks uint64 // platform start time; 0 when unknown, guards pid reuse
}

// procTable is one process snapshot. load fills cmdline and cwd on demand.
type procTable struct {
	procs map[int]*procInfo
	load  func(*procInfo)
}

func (t *procTable) details(p *procInfo) *procInfo {
	if !p.detailsLoaded {
		p.detailsLoaded = true
		if t.load != nil {
			t.load(p)
		}
	}
	return p
}

// ancestors returns p followed by its parents, bounded and cycle-safe (pid
// reuse can make a parent point back into the chain).
func (t *procTable) ancestors(p *procInfo) []*procInfo {
	out := []*procInfo{p}
	seen := map[int]bool{p.pid: true}
	for cur := p; len(out) < maxAncestors; {
		parent, ok := t.procs[cur.ppid]
		if !ok || seen[parent.pid] || cur.ppid == cur.pid {
			break
		}
		seen[parent.pid] = true
		out = append(out, parent)
		cur = parent
	}
	return out
}

// baseName lower-cases a process image name and strips a path and ".exe".
func baseName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	return strings.TrimSuffix(n, ".exe")
}

var scriptHosts = map[string]bool{"node": true, "bun": true, "deno": true}

// cmdlineMarkers maps a script-host command line to the agent runtime it
// hosts. Order matters: the first match wins.
var cmdlineMarkers = []struct{ marker, runtime string }{
	{"@anthropic-ai/claude-code", RuntimeClaudeCode},
	{"claude-code", RuntimeClaudeCode},
	{"@openai/codex", RuntimeCodex},
	{"opencode", RuntimeOpenCode},
	{"pi-coding-agent", RuntimePi},
	{"cursor-agent", RuntimeCursor},
}

var terminalNames = map[string]bool{
	"bash": true, "zsh": true, "sh": true, "fish": true, "dash": true, "pwsh": true,
	"powershell": true, "cmd": true, "windowsterminal": true, "wt": true, "conhost": true,
	"openconsole": true, "terminal": true, "iterm2": true, "tmux": true, "screen": true,
	"gnome-terminal-server": true, "konsole": true, "alacritty": true, "wezterm-gui": true, "kitty": true,
}

// runtimeOf names the agent runtime a process is, or "" when it is none.
// needCmdline reports whether the answer depends on the command line, so
// callers load it only for script hosts.
func runtimeOf(name string) (rt string, needCmdline bool) {
	switch b := baseName(name); b {
	case "claude":
		return RuntimeClaudeCode, false
	case "codex":
		return RuntimeCodex, false
	case "opencode":
		return RuntimeOpenCode, false
	case "pi":
		return RuntimePi, false
	case "cursor", "cursor-agent":
		return RuntimeCursor, false
	default:
		return "", scriptHosts[b]
	}
}

func runtimeFromCmdline(cmdline string) string {
	c := strings.ToLower(strings.ReplaceAll(cmdline, `\`, "/"))
	for _, m := range cmdlineMarkers {
		if strings.Contains(c, m.marker) {
			return m.runtime
		}
	}
	return ""
}

// agentRuntime returns the runtime p itself is, loading its command line
// only when p is a script host.
func (t *procTable) agentRuntime(p *procInfo) string {
	rt, need := runtimeOf(p.name)
	if rt != "" || !need {
		return rt
	}
	return runtimeFromCmdline(t.details(p).cmdline)
}

// AgentAlive reports whether any agent runtime process is in the table.
func (t *procTable) agentAlive() bool {
	for _, p := range t.procs {
		if t.agentRuntime(p) != "" {
			return true
		}
	}
	return false
}

// absPathRe matches an absolute POSIX or Windows path inside a command line.
var absPathRe = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|/)[^\s"'=;,|<>*?]*`)

// worktreeFor finds the nearest directory containing `.git` above any
// absolute path in cwd, then in the command line. exists is injected so the
// walk is testable with fixtures and GOOS-independent.
func worktreeFor(cwd, cmdline string, exists func(string) bool) string {
	candidates := []string{}
	if cwd != "" {
		candidates = append(candidates, cwd)
	}
	candidates = append(candidates, absPathRe.FindAllString(cmdline, -1)...)
	for _, c := range candidates {
		if root := gitRoot(c, exists); root != "" {
			return root
		}
	}
	return ""
}

// gitRoot walks up from p (either separator style) to the nearest directory
// that holds a `.git` entry.
func gitRoot(p string, exists func(string) bool) string {
	sep := "/"
	if strings.Contains(p, `\`) || (len(p) >= 2 && p[1] == ':') {
		sep = `\`
		p = strings.ReplaceAll(p, "/", `\`)
	}
	p = strings.TrimRight(p, sep)
	for i := 0; i < 64 && p != ""; i++ {
		if exists(p + sep + ".git") {
			if len(p) == 2 && p[1] == ':' {
				return p + sep
			}
			return p
		}
		j := strings.LastIndex(p, sep)
		if j < 0 {
			return ""
		}
		if j == 0 && sep == "/" {
			if p == "/" {
				return ""
			}
			p = "/"
			continue
		}
		p = p[:j]
	}
	return ""
}

// attribute fills worktree, cwd and runtime for p from itself and its
// ancestors: the nearest ancestor that is an agent runtime wins; a terminal
// ancestor yields "terminal"; otherwise "unknown".
func (t *procTable) attribute(p *procInfo, exists func(string) bool) (cwd, worktree, rt string) {
	chain := t.ancestors(p)
	t.details(p)
	cwd = p.cwd
	terminal := false
	for _, a := range chain {
		if worktree == "" {
			t.details(a)
			worktree = worktreeFor(a.cwd, a.cmdline, exists)
		}
		if rt == "" {
			rt = t.agentRuntime(a)
		}
		if terminalNames[baseName(a.name)] {
			terminal = true
		}
		if rt != "" && worktree != "" {
			break
		}
	}
	switch {
	case rt != "":
	case terminal:
		rt = RuntimeTerminal
	default:
		rt = RuntimeUnknown
	}
	return cwd, worktree, rt
}

// topIO ranks processes by bytes moved between two snapshots elapsedSec
// apart and attributes the top limit. A pid whose name or start time changed
// is a different process and is skipped.
func topIO(prev, cur *procTable, elapsedSec float64, limit int, exists func(string) bool) []ProcIO {
	if prev == nil || cur == nil || elapsedSec <= 0 {
		return nil
	}
	type cand struct {
		p           *procInfo
		read, write float64
	}
	var cands []cand
	for pid, c := range cur.procs {
		p, ok := prev.procs[pid]
		if !ok || !c.ioOK || !p.ioOK || p.name != c.name || p.startTimeTicks != c.startTimeTicks ||
			c.readB < p.readB || c.writeB < p.writeB {
			continue
		}
		r := float64(c.readB-p.readB) / elapsedSec / bytesPerMB
		w := float64(c.writeB-p.writeB) / elapsedSec / bytesPerMB
		if r+w <= 0 {
			continue
		}
		cands = append(cands, cand{p: c, read: r, write: w})
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i].read+cands[i].write, cands[j].read+cands[j].write
		if a != b {
			return a > b
		}
		return cands[i].p.pid < cands[j].p.pid
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]ProcIO, 0, len(cands))
	for _, c := range cands {
		cwd, wt, rt := cur.attribute(c.p, exists)
		out = append(out, ProcIO{
			PID: c.p.pid, Name: c.p.name,
			WriteMBs: round(c.write, 2), ReadMBs: round(c.read, 2),
			Cwd: cwd, Worktree: wt, Runtime: rt,
		})
	}
	return out
}

// cachedExists memoizes a stat-style existence check for one attribution run.
func cachedExists(stat func(string) bool) func(string) bool {
	memo := map[string]bool{}
	return func(p string) bool {
		if v, ok := memo[p]; ok {
			return v
		}
		v := stat(p)
		memo[p] = v
		return v
	}
}
