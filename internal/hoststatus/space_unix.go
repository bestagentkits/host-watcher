//go:build linux || darwin

package hoststatus

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// volumeKey identifies the filesystem holding path by device number.
func volumeKey(path string) (key, label string, err error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return fmt.Sprint(st.Dev), abs, nil
}

func diskSpace(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize) //nolint:gosec // block size is a small positive value on every supported kernel
	return st.Bavail * bsize, st.Blocks * bsize, nil
}

func fixedDrives() []string { return nil }
