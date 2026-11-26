//go:build unix

package lockfile

import (
	"errors"
	"os"
	"syscall"
)

var (
	errLockBusyInternal        = errors.New("lockfile: lock busy")
	errLockUnsupportedInternal = errors.New("lockfile: filesystem does not support locking")
)

// flockExclusiveNB acquires an exclusive non-blocking advisory lock via
// POSIX flock(2). Returns errLockBusyInternal when another holder owns it, and
// errLockUnsupportedInternal when the filesystem cannot lock at all.
func flockExclusiveNB(f *os.File) error {
	fd := int(f.Fd()) //nolint:gosec // POSIX fd fits in int
	err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLockBusyInternal
	}
	// Some filesystems cannot lock at all: an NFS mount with nolock, WSL's DrvFs
	// under /mnt/c, some container and overlay mounts. Treating that as fatal
	// would refuse to install on a setup that works today, so name the condition
	// and let the caller decide whether to proceed unprotected.
	if errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) {
		return errLockUnsupportedInternal
	}
	return err
}

// funlock releases an advisory lock acquired by flockExclusiveNB.
func funlock(f *os.File) error {
	fd := int(f.Fd()) //nolint:gosec // POSIX fd fits in int
	return syscall.Flock(fd, syscall.LOCK_UN)
}
