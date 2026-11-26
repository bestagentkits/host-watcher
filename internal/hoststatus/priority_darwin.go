//go:build darwin

package hoststatus

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

const taskpolicy = "/usr/sbin/taskpolicy"

// StartLowPriority runs cmd under `taskpolicy -b` (background CPU and I/O
// policy) when available; otherwise it lowers this process's nice value,
// which the child inherits.
func StartLowPriority(cmd *exec.Cmd) (warnings []string, err error) {
	if _, statErr := os.Stat(taskpolicy); statErr == nil {
		cmd.Args = append([]string{taskpolicy, "-b", cmd.Path}, cmd.Args[1:]...)
		cmd.Path = taskpolicy
	} else {
		warnings = append(warnings, "background I/O policy not applied: "+taskpolicy+" not found")
		if err := unix.Setpriority(unix.PRIO_PROCESS, 0, 10); err != nil {
			warnings = append(warnings, fmt.Sprintf("low CPU priority not applied: %v", err))
		}
	}
	return warnings, cmd.Start()
}
