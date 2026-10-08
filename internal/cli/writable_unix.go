//go:build unix

package cli

import "syscall"

// userCanWrite reports whether this user may write to path. The agent runs as
// this user, so it can write wherever the user can.
func userCanWrite(path string) bool {
	const wOK = 0x2
	return syscall.Access(path, wOK) == nil
}
