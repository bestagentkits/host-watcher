//go:build windows

package hoststatus

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                         = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes               = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx         = kernel32.NewProc("GlobalMemoryStatusEx")
	pdh                              = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW                = pdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW        = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData          = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArrayW = pdh.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery                = pdh.NewProc("PdhCloseQuery")
)

const (
	pdhFmtDouble   = 0x00000200
	pdhFmtNoCap100 = 0x00008000
	pdhMoreData    = 0x800007D2
	// PDH_FMT_COUNTERVALUE_ITEM_W is {LPWSTR szName; PDH_FMT_COUNTERVALUE
	// FmtValue}. FmtValue holds a double, so it is 8-byte aligned on both
	// 32- and 64-bit Windows: CStatus at offset 8, the double at 16, 24 bytes
	// per item.
	pdhItemSize        = 24
	pdhItemStatusOff   = 8
	pdhItemValueOff    = 16
	pdhCStatusValid    = 0
	pdhCStatusNewData  = 1
	counterIdle        = `\PhysicalDisk(*)\% Idle Time`
	counterLatency     = `\PhysicalDisk(*)\Avg. Disk sec/Transfer`
	counterWriteBytes  = `\PhysicalDisk(*)\Disk Write Bytes/sec`
	counterReadBytes   = `\PhysicalDisk(*)\Disk Read Bytes/sec`
	memoryStatusExSize = 64
)

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// sampleHost reads CPU times with GetSystemTimes and per-physical-disk
// counters through PDH at each interval boundary, and memory once.
func sampleHost(ctx context.Context, n int, interval time.Duration) rawSample {
	raw := rawSample{diskScope: ScopeHost}
	if mem, err := windowsMemory(); err != nil {
		raw.miss("memory", err)
	} else {
		raw.memory = mem
	}

	q, diskErr := openDiskQuery()
	if q != nil {
		defer q.close()
	}
	prevCPU, cpuErr := systemTimes()
	for i := 0; i < n && (cpuErr == nil || diskErr == nil); i++ {
		if sleepCtx(ctx, interval) != nil {
			return raw
		}
		if cpuErr == nil {
			var cur cpuTimes
			if cur, cpuErr = systemTimes(); cpuErr == nil {
				if busy, ok := busyPct(prevCPU, cur); ok {
					raw.cpuBusy = append(raw.cpuBusy, busy)
				}
				prevCPU = cur
			}
		}
		if diskErr == nil {
			var sample map[string]diskRate
			if sample, diskErr = q.sample(); diskErr == nil {
				raw.disks = append(raw.disks, sample)
			}
		}
	}
	if cpuErr != nil {
		raw.miss("cpu", cpuErr)
	}
	if diskErr != nil {
		raw.miss("disk", diskErr)
	}
	return raw
}

func systemTimes() (cpuTimes, error) {
	var idle, kernel, user windows.Filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))) //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
	if r == 0 {
		return cpuTimes{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	ft := func(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	// Kernel time includes idle time.
	return cpuTimes{idle: ft(idle), total: ft(kernel) + ft(user)}, nil
}

func windowsMemory() (*Memory, error) {
	st := memoryStatusEx{Length: memoryStatusExSize}
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st))) //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
	if r == 0 {
		return nil, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	if st.TotalPhys == 0 {
		return nil, fmt.Errorf("GlobalMemoryStatusEx reported zero physical memory")
	}
	return newMemory(st.TotalPhys, st.AvailPhys), nil
}

type diskQuery struct {
	handle                     uintptr
	idle, latency, write, read uintptr
}

// pdhErr formats a PDH_STATUS, a 32-bit LONG returned in a uintptr.
func pdhErr(op string, status uintptr) error {
	return fmt.Errorf("%s: PDH status 0x%08X", op, uint32(status)) //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
}

// openDiskQuery adds the English (locale-independent) PhysicalDisk counters
// and takes the first collection that rate counters need as a baseline.
func openDiskQuery() (*diskQuery, error) {
	if err := pdh.Load(); err != nil {
		return nil, fmt.Errorf("load pdh.dll: %w", err)
	}
	q := &diskQuery{}
	if st, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&q.handle))); uint32(st) != 0 { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		return nil, pdhErr("PdhOpenQuery", st)
	}
	for _, c := range []struct {
		path string
		dst  *uintptr
	}{{counterIdle, &q.idle}, {counterLatency, &q.latency}, {counterWriteBytes, &q.write}, {counterReadBytes, &q.read}} {
		p, err := windows.UTF16PtrFromString(c.path)
		if err != nil {
			return q, err
		}
		if st, _, _ := procPdhAddEnglishCounterW.Call(q.handle, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(c.dst))); uint32(st) != 0 { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
			return q, pdhErr("PdhAddEnglishCounter "+c.path, st)
		}
	}
	if st, _, _ := procPdhCollectQueryData.Call(q.handle); uint32(st) != 0 { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		return q, pdhErr("PdhCollectQueryData", st)
	}
	return q, nil
}

func (q *diskQuery) close() {
	if q.handle != 0 {
		_, _, _ = procPdhCloseQuery.Call(q.handle)
		q.handle = 0
	}
}

// sample collects once more and returns per-disk rates since the previous
// collection, skipping the _Total instance.
func (q *diskQuery) sample() (map[string]diskRate, error) {
	if st, _, _ := procPdhCollectQueryData.Call(q.handle); uint32(st) != 0 { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		return nil, pdhErr("PdhCollectQueryData", st)
	}
	idle, err := counterArray(q.idle)
	if err != nil {
		return nil, err
	}
	latency, err := counterArray(q.latency)
	if err != nil {
		return nil, err
	}
	write, err := counterArray(q.write)
	if err != nil {
		return nil, err
	}
	read, err := counterArray(q.read)
	if err != nil {
		return nil, err
	}
	out := map[string]diskRate{}
	for name, idlePct := range idle {
		if name == "_Total" {
			continue
		}
		r := diskRate{active: f64(clampPct(100 - idlePct))}
		if v, ok := latency[name]; ok {
			r.latencyMs = f64(v * 1000)
		}
		if v, ok := write[name]; ok {
			r.writeMBs = f64(v / bytesPerMB)
		}
		if v, ok := read[name]; ok {
			r.readMBs = f64(v / bytesPerMB)
		}
		out[name] = r
	}
	return out, nil
}

// counterArray reads a wildcard counter's formatted values by instance.
func counterArray(counter uintptr) (map[string]float64, error) {
	var size, count uint32
	st, _, _ := procPdhGetFormattedCounterArrayW.Call(counter, pdhFmtDouble|pdhFmtNoCap100,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0) //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
	if uint32(st) != pdhMoreData { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		if uint32(st) == 0 { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
			return map[string]float64{}, nil
		}
		return nil, pdhErr("PdhGetFormattedCounterArray size", st)
	}
	buf := make([]byte, size)
	st, _, _ = procPdhGetFormattedCounterArrayW.Call(counter, pdhFmtDouble|pdhFmtNoCap100,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0]))) //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
	if uint32(st) != 0 { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		return nil, pdhErr("PdhGetFormattedCounterArray", st)
	}
	if uint64(count)*pdhItemSize > uint64(len(buf)) {
		return nil, fmt.Errorf("PdhGetFormattedCounterArray: %d items overflow a %d-byte buffer", count, len(buf))
	}
	out := make(map[string]float64, count)
	for i := uint32(0); i < count; i++ {
		item := buf[uintptr(i)*pdhItemSize:]
		// The name pointers reference strings inside buf, which stays alive
		// for this loop. unsafe is required to read the Win32 item layout.
		namePtr := *(**uint16)(unsafe.Pointer(&item[0]))              //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		status := *(*uint32)(unsafe.Pointer(&item[pdhItemStatusOff])) //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		value := *(*float64)(unsafe.Pointer(&item[pdhItemValueOff]))  //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		if status != pdhCStatusValid && status != pdhCStatusNewData {
			continue
		}
		out[strings.TrimSpace(windows.UTF16PtrToString(namePtr))] = value
	}
	return out, nil
}

// volumeKey resolves path to its volume root, which is also the label.
func volumeKey(path string) (key, label string, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", "", err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		return "", "", fmt.Errorf("GetVolumePathName: %w", err)
	}
	root := windows.UTF16ToString(buf)
	// Prefer the volume GUID so two mount points of one volume dedupe;
	// drives without one (network or virtual) fall back to their root.
	guid := make([]uint16, 64)
	if err := windows.GetVolumeNameForVolumeMountPoint(&buf[0], &guid[0], uint32(len(guid))); err == nil { //nolint:gosec // Win32 API interop: PDH_STATUS is a 32-bit LONG and buffer lengths are small constants
		return windows.UTF16ToString(guid), root, nil
	}
	return strings.ToUpper(root), root, nil
}

func diskSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &totalBytes, &totalFree); err != nil {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceEx: %w", err)
	}
	return avail, totalBytes, nil
}

// fixedDrives lists the roots of every fixed (DRIVE_FIXED) drive letter.
func fixedDrives() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if windows.GetDriveType(p) == windows.DRIVE_FIXED {
			out = append(out, root)
		}
	}
	return out
}
