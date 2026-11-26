//go:build !windows && !linux && !darwin

package hoststatus

import "os/exec"

// StartLowPriority starts cmd unchanged: this platform has no supported
// priority control.
func StartLowPriority(cmd *exec.Cmd) ([]string, error) {
	return []string{"low priority not applied: unsupported platform"}, cmd.Start()
}
