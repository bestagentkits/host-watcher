//go:build !linux

package main

import "os/exec"

// setParentDeathSignal is a no-op: only Linux has a parent-death signal.
func setParentDeathSignal(*exec.Cmd) {}
