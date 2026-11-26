//go:build windows

package lockfile

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	errLockBusyInternal        = errors.New("lockfile: lock busy")
	errLockUnsupportedInternal = errors.New("lockfile: filesystem does not support locking")
)

// Network redirectors and a few filesystems answer LockFileEx with these rather
// than locking. Same meaning as the POSIX ENOLCK family: no lock is available,
// and refusing to run would be worse than running unprotected.
const (
	errorInvalidFunction = syscall.Errno(1)
	errorNotSupported    = syscall.Errno(50)
)

const (
	winLockfileExclusiveLock   = 0x00000002
	winLockfileFailImmediately = 0x00000001
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

// flockExclusiveNB acquires an exclusive non-blocking lock via LockFileEx.
func flockExclusiveNB(f *os.File) error {
	var ol syscall.Overlapped
	r1, _, e1 := procLockFileEx.Call(
		f.Fd(),
		uintptr(winLockfileExclusiveLock|winLockfileFailImmediately),
		0,
		^uintptr(0),
		^uintptr(0),
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 != 0 {
		return nil
	}
	if errno, ok := e1.(syscall.Errno); ok && errno == syscall.Errno(33) {
		return errLockBusyInternal
	}
	if errors.Is(e1, syscall.ERROR_IO_PENDING) {
		return errLockBusyInternal
	}
	if errors.Is(e1, errorInvalidFunction) || errors.Is(e1, errorNotSupported) {
		return errLockUnsupportedInternal
	}
	return e1
}

// funlock releases the entire-file lock.
func funlock(f *os.File) error {
	var ol syscall.Overlapped
	r1, _, e1 := procUnlockFileEx.Call(
		f.Fd(),
		0,
		^uintptr(0),
		^uintptr(0),
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 != 0 {
		return nil
	}
	return e1
}
