//go:build linux || darwin

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// freeSpace returns the bytes available to this user on the filesystem that
// holds dir. A workspace that does not exist yet is measured where it will be
// created: the nearest existing parent.
func freeSpace(dir string) (uint64, error) {
	path := filepath.Clean(dir)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
