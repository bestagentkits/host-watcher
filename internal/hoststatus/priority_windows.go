//go:build windows

package hoststatus

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ioPriorityLow is IO_PRIORITY_HINT IoPriorityLow.
const ioPriorityLow uint32 = 1

// processSuspendResume is PROCESS_SUSPEND_RESUME.
const processSuspendResume = 0x0800

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// StartLowPriority starts cmd suspended in the below-normal priority class,
// which its children inherit, lowers its I/O priority, then resumes it, so
// the command never runs a single instruction at normal I/O priority. The
// I/O step is best effort: a failure is returned as a warning. If the
// suspended process cannot be resumed it is ended and an error returned.
func StartLowPriority(cmd *exec.Cmd) (warnings []string, err error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.BELOW_NORMAL_PRIORITY_CLASS | windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION|processSuspendResume, false, pidU32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("open suspended process to resume it: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	prio := ioPriorityLow
	if err := windows.NtSetInformationProcess(h, windows.ProcessIoPriority, unsafe.Pointer(&prio), uint32(unsafe.Sizeof(prio))); err != nil { //nolint:gosec // Win32 call takes a pointer to the priority value
		warnings = append(warnings, fmt.Sprintf("low I/O priority not applied: %v", err))
	}
	if ferr := procNtResumeProcess.Find(); ferr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return warnings, fmt.Errorf("resume suspended process: %w", ferr)
	}
	if status, _, _ := procNtResumeProcess.Call(uintptr(h)); status != 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return warnings, fmt.Errorf("resume suspended process: NTSTATUS 0x%08X", status)
	}
	return warnings, nil
}

// StartDetached starts cmd with no console, in its own process group, and
// out of the caller's job where the job allows breakaway, so closing the
// terminal or agent session does not end it.
func StartDetached(cmd *exec.Cmd) error {
	flags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	err := cmd.Start()
	if err == nil {
		return nil
	}
	// A job that forbids breakaway refuses the start; retry inside it.
	retry := exec.Command(cmd.Path, cmd.Args[1:]...) //nolint:gosec // re-launches the same resolved binary
	retry.Env, retry.Dir = cmd.Env, cmd.Dir
	retry.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags &^ windows.CREATE_BREAKAWAY_FROM_JOB, HideWindow: true}
	if rerr := retry.Start(); rerr != nil {
		return fmt.Errorf("%w (retry inside the job: %v)", err, rerr)
	}
	*cmd = *retry //nolint:govet // copying a fresh, started Cmd into the caller's value
	return nil
}
