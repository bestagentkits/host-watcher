//go:build linux

package hoststatus

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readProcTable lists /proc. Per-process I/O needs read access to
// /proc/<pid>/io, which the kernel grants for the caller's own processes;
// others stay without rates.
func readProcTable() (*procTable, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	t := &procTable{procs: map[int]*procInfo{}, load: loadLinuxDetails}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := "/proc/" + e.Name()
		stat, err := readProcFile(dir + "/stat")
		if err != nil {
			continue // exited or hidden
		}
		name, ppid, start, err := parseProcPidStat(stat)
		if err != nil {
			continue
		}
		p := &procInfo{pid: pid, ppid: ppid, name: name, startTimeTicks: start}
		if io, err := readProcFile(dir + "/io"); err == nil {
			if r, w, err := parseProcPidIO(io); err == nil {
				p.readB, p.writeB, p.ioOK = r, w, true
			}
		}
		t.procs[pid] = p
	}
	if len(t.procs) == 0 {
		return nil, fmt.Errorf("read /proc: no readable processes")
	}
	return t, nil
}

func loadLinuxDetails(p *procInfo) {
	dir := "/proc/" + strconv.Itoa(p.pid)
	if b, err := readProcFile(dir + "/cmdline"); err == nil {
		p.cmdline = strings.TrimSpace(strings.ReplaceAll(b, "\x00", " "))
	}
	if cwd, err := os.Readlink(dir + "/cwd"); err == nil {
		p.cwd = cwd
	}
}

const procIOUnavailable = ""
