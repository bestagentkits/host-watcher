//go:build unix

package hoststatus

import (
	"os/exec"
	"syscall"
)

// StartDetached starts cmd in a new session so it survives the caller's
// terminal and process group.
func StartDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
