//go:build !linux

package procexec

import (
	"errors"
	"os"
	"syscall"
)

// ProcessRunning reports whether pid is a live process. Without /proc a zombie
// cannot be told from a running process; that is the conservative answer.
func ProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
