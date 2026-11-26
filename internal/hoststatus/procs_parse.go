package hoststatus

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// parseProcPidStat reads the command name, parent pid and start time from
// one /proc/<pid>/stat line. The name is parenthesized and may contain
// spaces or parentheses, so fields are counted after the last ')'.
func parseProcPidStat(text string) (name string, ppid int, start uint64, err error) {
	open := strings.IndexByte(text, '(')
	closing := strings.LastIndexByte(text, ')')
	if open < 0 || closing < open {
		return "", 0, 0, fmt.Errorf("parse /proc stat: no command name")
	}
	name = text[open+1 : closing]
	f := strings.Fields(text[closing+1:])
	// f[0] is state (field 3); ppid is field 4, starttime field 22.
	if len(f) < 20 {
		return "", 0, 0, fmt.Errorf("parse /proc stat: %d fields after name, want at least 20", len(f))
	}
	if ppid, err = strconv.Atoi(f[1]); err != nil {
		return "", 0, 0, fmt.Errorf("parse /proc stat ppid: %w", err)
	}
	if start, err = strconv.ParseUint(f[19], 10, 64); err != nil {
		return "", 0, 0, fmt.Errorf("parse /proc stat starttime: %w", err)
	}
	return name, ppid, start, nil
}

// parseProcPidIO reads read_bytes and write_bytes (bytes that reached the
// storage layer) from /proc/<pid>/io.
func parseProcPidIO(text string) (read, write uint64, err error) {
	found := 0
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok || (key != "read_bytes" && key != "write_bytes") {
			continue
		}
		v, perr := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if perr != nil {
			return 0, 0, fmt.Errorf("parse /proc io %s: %w", key, perr)
		}
		if key == "read_bytes" {
			read = v
		} else {
			write = v
		}
		found++
	}
	if found != 2 {
		return 0, 0, fmt.Errorf("parse /proc io: read_bytes or write_bytes missing")
	}
	return read, write, nil
}

// parsePs parses `ps -axww -o pid=,ppid=,ucomm=,args=` output. ucomm is a
// single token; args is the rest of the line.
func parsePs(text string) (map[int]*procInfo, error) {
	out := map[int]*procInfo{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		out[pid] = &procInfo{
			pid: pid, ppid: ppid, name: f[2],
			cmdline: strings.Join(f[3:], " "), detailsLoaded: true,
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("parse ps output: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("parse ps output: no processes")
	}
	return out, nil
}
