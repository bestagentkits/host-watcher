//go:build windows

package hoststatus

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessIoCounters = kernel32.NewProc("GetProcessIoCounters")

const procIOUnavailable = ""

// pidU32 converts a pid from the OS (always within uint32) for Win32 calls.
func pidU32(pid int) uint32 {
	if pid < 0 || uint64(pid) > uint64(^uint32(0)) {
		return 0
	}
	return uint32(pid)
}

// ioCounters mirrors IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

// readProcTable snapshots processes with CreateToolhelp32Snapshot and reads
// each one's cumulative I/O transfer counts. Processes the caller cannot
// open (protected or another user's) stay without rates.
func readProcTable() (*procTable, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	t := &procTable{procs: map[int]*procInfo{}, load: loadWindowsDetails}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := int(e.ProcessID)
		if pid == 0 {
			continue
		}
		p := &procInfo{pid: pid, ppid: int(e.ParentProcessID), name: windows.UTF16ToString(e.ExeFile[:])}
		readWindowsIO(p)
		t.procs[pid] = p
	}
	if len(t.procs) == 0 {
		return nil, fmt.Errorf("process snapshot is empty: %w", err)
	}
	return t, nil
}

func readWindowsIO(p *procInfo) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pidU32(p.pid))
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var c ioCounters
	if r, _, _ := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&c))); r == 0 { //nolint:gosec // Win32 call writes IO_COUNTERS
		return
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
		p.startTimeTicks = uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	}
	p.readB, p.writeB, p.ioOK = c.ReadTransferCount, c.WriteTransferCount, true
}

// loadWindowsDetails reads the command line and current directory from the
// target's process parameters block. It needs PROCESS_VM_READ, so it only
// succeeds for the caller's own (non-elevated) processes; failures leave
// both empty and attribution falls back to the parent chain.
func loadWindowsDetails(p *procInfo) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pidU32(p.pid))
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var pbi windows.PROCESS_BASIC_INFORMATION
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation,
		unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), nil); err != nil || pbi.PebBaseAddress == nil { //nolint:gosec // NT call writes PROCESS_BASIC_INFORMATION
		return
	}
	var peb windows.PEB
	if !readRemote(h, uintptr(unsafe.Pointer(pbi.PebBaseAddress)), unsafe.Pointer(&peb), unsafe.Sizeof(peb)) || //nolint:gosec // remote addresses read from the target process
		peb.ProcessParameters == nil {
		return
	}
	var params windows.RTL_USER_PROCESS_PARAMETERS
	if !readRemote(h, uintptr(unsafe.Pointer(peb.ProcessParameters)), unsafe.Pointer(&params), unsafe.Sizeof(params)) { //nolint:gosec // remote addresses read from the target process
		return
	}
	p.cmdline = readRemoteString(h, params.CommandLine)
	p.cwd = readRemoteString(h, params.CurrentDirectory.DosPath)
}

// readRemote copies size bytes from the target's address space. The pointer
// arguments are addresses in another process, so unsafe is required.
func readRemote(h windows.Handle, addr uintptr, dst unsafe.Pointer, size uintptr) bool {
	var n uintptr
	return windows.ReadProcessMemory(h, addr, (*byte)(dst), size, &n) == nil && n == size
}

func readRemoteString(h windows.Handle, s windows.NTUnicodeString) string {
	if s.Buffer == nil || s.Length == 0 || s.Length > 32*1024 {
		return ""
	}
	buf := make([]uint16, s.Length/2)
	if !readRemote(h, uintptr(unsafe.Pointer(s.Buffer)), unsafe.Pointer(&buf[0]), uintptr(s.Length)) { //nolint:gosec // remote addresses read from the target process
		return ""
	}
	return windows.UTF16ToString(buf)
}
