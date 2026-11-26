//go:build !unix

package main

import "os/exec"

// childStatus is the command's exit code.
func childStatus(e *exec.ExitError) int { return e.ExitCode() }
