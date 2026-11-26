//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal makes the kernel send SIGTERM to the child if host-watcher dies
// first, so a killed gate does not orphan the command.
func setParentDeathSignal(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
