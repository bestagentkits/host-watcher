//go:build linux

package hoststatus

import (
	"fmt"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

const (
	ioprioWhoProcess = 1
	ioprioClassIdle  = 3
	ioprioClassShift = 13
	lowNice          = 10
)

// StartLowPriority starts cmd at nice 10 in the idle I/O class. Both are
// per-thread on Linux and inherited at fork, so the calling OS thread is
// lowered and the child forked from it; the thread is then discarded by
// exiting the locked goroutine. Nothing races the child's own children.
func StartLowPriority(cmd *exec.Cmd) (warnings []string, err error) {
	type result struct {
		warnings []string
		err      error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the lowered thread dies with this goroutine
		var w []string
		if err := unix.Setpriority(unix.PRIO_PROCESS, 0, lowNice); err != nil {
			w = append(w, fmt.Sprintf("low CPU priority not applied: %v", err))
		}
		if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0,
			uintptr(ioprioClassIdle<<ioprioClassShift)); errno != 0 {
			w = append(w, fmt.Sprintf("idle I/O priority not applied: %v", errno))
		}
		done <- result{w, cmd.Start()}
	}()
	r := <-done
	return r.warnings, r.err
}
