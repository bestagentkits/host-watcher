package hoststatus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Inbox timing.
const (
	InboxRewriteAfter = 5 * time.Minute
	InboxMaxAge       = 10 * time.Minute
)

// InboxProcess is one process of the worktree that the watcher saw moving
// I/O.
type InboxProcess struct {
	PID      int     `json:"pid"`
	Name     string  `json:"name"`
	WriteMBs float64 `json:"write_mb_s"`
	ReadMBs  float64 `json:"read_mb_s"`
}

// InboxEntry is the per-worktree notice the watcher leaves for the agent
// session working in that worktree.
type InboxEntry struct {
	Worktree  string         `json:"worktree"`
	Verdict   Verdict        `json:"verdict"`
	SampledAt string         `json:"sampled_at"`
	Processes []InboxProcess `json:"processes"`
	// IOScope copies the report's io_scope for the process rates.
	IOScope  string `json:"io_scope,omitempty"`
	Action   string `json:"action"`
	Severity int    `json:"severity"`
}

var driveOnly = regexp.MustCompile(`^[A-Za-z]:$`)

// WorktreeHash names a worktree's inbox file. The algorithm, which the JS
// hook reimplements byte for byte, is:
//
//  1. Replace every `\` with `/`.
//  2. Clean the path POSIX-style (Go path.Clean / Node path.posix.normalize
//     plus removing a trailing `/`): collapse `//`, resolve `.` and `..`,
//     drop a trailing slash. A bare drive `X:` becomes `X:/`.
//  3. On Windows only, lower-case ASCII A-Z (and nothing else; non-ASCII
//     characters are kept as is so Go and JS agree byte for byte).
//  4. sha256 of the UTF-8 bytes; the name is the first 16 hex characters
//     (lower-case).
//
// Example: `D:\Worktrees\x\` on Windows normalizes to "d:/worktrees/x" and
// names the file d49b00fbf4d3b90e.json.
func WorktreeHash(root string) string {
	return worktreeHash(root, runtime.GOOS == "windows")
}

func worktreeHash(root string, foldCase bool) string {
	return hashKey(normalizeWorktree(root, foldCase))
}

func normalizeWorktree(root string, foldCase bool) string {
	p := path.Clean(strings.ReplaceAll(root, `\`, "/"))
	if driveOnly.MatchString(p) {
		p += "/"
	}
	if foldCase {
		p = asciiLower(p)
	}
	return p
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// InboxPath is the entry file for worktree under dir.
func InboxPath(dir, worktree string) string {
	return filepath.Join(dir, WorktreeHash(worktree)+".json")
}

func inboxSeverity(v Verdict) int {
	switch v {
	case VerdictThrottle:
		return 1
	case VerdictClean:
		return 2
	case VerdictHold:
		return 3
	default:
		return 0
	}
}

func inboxAction(v Verdict) string {
	switch v {
	case VerdictHold:
		return "Pause heavy builds and tests in this worktree until host status reports go; run them through `host-watcher gate -- <cmd>` (or `python scripts/host_gate.py -- <cmd>`)."
	case VerdictClean:
		return "Free disk space before heavy work: check `host-watcher caches` (or `python scripts/cache_cleanup.py`) and clean caches you own."
	default:
		return "Reduce parallel jobs in this worktree and run heavy commands through `host-watcher gate -- <cmd>` (or `python scripts/host_gate.py -- <cmd>`)."
	}
}

// UpdateInbox writes one entry per worktree whose processes appear in
// r.TopIO when the verdict is throttle, clean or hold. An existing fresh
// entry is rewritten only when severity rises or it is older than
// InboxRewriteAfter. Entries older than InboxMaxAge are removed.
func UpdateInbox(dir string, r *Report, now time.Time) error {
	if err := PruneInbox(dir, now); err != nil {
		return err
	}
	sev := inboxSeverity(r.Verdict)
	if sev == 0 {
		return nil
	}
	byWorktree := map[string][]InboxProcess{}
	var order []string
	for _, p := range r.TopIO {
		if p.Worktree == "" {
			continue
		}
		if _, ok := byWorktree[p.Worktree]; !ok {
			order = append(order, p.Worktree)
		}
		byWorktree[p.Worktree] = append(byWorktree[p.Worktree],
			InboxProcess{PID: p.PID, Name: p.Name, WriteMBs: p.WriteMBs, ReadMBs: p.ReadMBs})
	}
	if len(order) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create inbox %s: %w", dir, err)
	}
	var errs []error
	for _, wt := range order {
		file := InboxPath(dir, wt)
		if old, err := readInboxFile(file); err == nil {
			at, perr := time.Parse(time.RFC3339, old.SampledAt)
			if perr == nil && old.Severity >= sev && now.Sub(at) < InboxRewriteAfter {
				continue
			}
		}
		e := InboxEntry{
			Worktree: wt, Verdict: r.Verdict, SampledAt: now.UTC().Format(time.RFC3339),
			Processes: byWorktree[wt], IOScope: r.IOScope, Action: inboxAction(r.Verdict), Severity: sev,
		}
		b, err := json.MarshalIndent(e, "", "  ")
		if err == nil {
			err = writeFileAtomic(file, append(b, '\n'), 0o644)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("write %s: %w", file, err))
		}
	}
	return errors.Join(errs...)
}

func readInboxFile(file string) (*InboxEntry, error) {
	b, err := os.ReadFile(file) //nolint:gosec,forbidigo // host-watcher-owned inbox entry, not kit content
	if err != nil {
		return nil, err
	}
	var e InboxEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func inboxStale(e *InboxEntry, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, e.SampledAt)
	return err != nil || now.Sub(at) > InboxMaxAge
}

// PruneInbox removes entries older than InboxMaxAge and unreadable ones.
// A missing inbox directory is not an error.
func PruneInbox(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read inbox %s: %w", dir, err)
	}
	for _, d := range entries {
		if d.IsDir() || filepath.Ext(d.Name()) != ".json" {
			continue
		}
		file := filepath.Join(dir, d.Name())
		if e, err := readInboxFile(file); err != nil || inboxStale(e, now) {
			_ = os.Remove(file)
		}
	}
	return nil
}

// ReadInbox returns the fresh entry for worktree without consuming it. A
// stale entry is removed and reported as absent.
func ReadInbox(dir, worktree string, now time.Time) *InboxEntry {
	if worktree == "" {
		return nil
	}
	file := InboxPath(dir, worktree)
	e, err := readInboxFile(file)
	if err != nil {
		return nil
	}
	if inboxStale(e, now) {
		_ = os.Remove(file)
		return nil
	}
	return e
}

// TakeInbox returns the fresh entry for worktree and deletes it.
func TakeInbox(dir, worktree string, now time.Time) *InboxEntry {
	e := ReadInbox(dir, worktree, now)
	if e != nil {
		_ = os.Remove(InboxPath(dir, worktree))
	}
	return e
}
