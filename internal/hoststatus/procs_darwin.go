//go:build darwin

package hoststatus

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// readProcTable lists processes with ps. macOS exposes per-process disk I/O
// only through proc_pid_rusage in libproc, which needs cgo, so rows carry no
// I/O counters and top_io is reported unavailable.
func readProcTable() (*procTable, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axww", "-o", "pid=,ppid=,ucomm=,args=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	procs, err := parsePs(string(out))
	if err != nil {
		return nil, err
	}
	return &procTable{procs: procs}, nil
}

const procIOUnavailable = "top_io unavailable: macOS exposes per-process disk I/O only through libproc, which needs cgo"
