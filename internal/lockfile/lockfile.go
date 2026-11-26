// Package lockfile provides a non-blocking exclusive advisory file lock
// (flock on Unix, LockFileEx on Windows) used to keep one watcher per host.
package lockfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrBusy means another process holds the lock.
var ErrBusy = errors.New("lockfile: lock is held by another process")

// ErrUnsupported means the filesystem cannot lock (NFS nolock, WSL DrvFs...).
// The returned Lock is a no-op so Unlock stays unconditional.
var ErrUnsupported = errors.New("lockfile: filesystem does not support locking")

// Lock is a held advisory lock.
type Lock struct{ f *os.File }

// AcquirePath locks path, retrying every 25ms for up to maxWait. A zero
// maxWait tries exactly once.
func AcquirePath(ctx context.Context, path string, maxWait time.Duration) (*Lock, error) {
	if path == "" {
		return nil, fmt.Errorf("lockfile: empty lock path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("lockfile: mkdir: %w", err)
	}
	deadline := time.Now().Add(maxWait)
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // lock file under the state dir
		if err != nil {
			return nil, fmt.Errorf("lockfile: open: %w", err)
		}
		lockErr := flockExclusiveNB(f)
		if lockErr == nil {
			return &Lock{f: f}, nil
		}
		_ = f.Close()
		if errors.Is(lockErr, errLockUnsupportedInternal) {
			return &Lock{}, ErrUnsupported
		}
		if !errors.Is(lockErr, errLockBusyInternal) {
			return nil, fmt.Errorf("lockfile: acquire: %w", lockErr)
		}
		if !time.Now().Before(deadline) {
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Unlock releases the lock. Safe on a nil or no-op Lock.
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = funlock(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}
