//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// childStatus maps a wait status to a shell-style exit code: 128 plus the
// signal number when a signal ended the command.
func childStatus(e *exec.ExitError) int {
	if ws, ok := e.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return e.ExitCode()
}
