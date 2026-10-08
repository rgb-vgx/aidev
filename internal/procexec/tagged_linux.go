//go:build linux

package procexec

import (
	"bytes"
	"os"
	"strconv"
)

// taggedProcesses returns the processes whose environment carries tag — the
// marker Run gives each child, which every descendant inherits whatever
// process group or session it moved to.
//
// It reads /proc/<pid>/environ, which the kernel only lets the process's own
// user read, so it can only ever find this user's processes; and it matches a
// random marker that only a descendant of the run can carry, never a process
// name (docs/research.md §2.7).
func taggedProcesses(tag string) []int {
	if tag == "" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	needle := []byte(tag + "=1")
	self := os.Getpid()
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, entry := range bytes.Split(env, []byte{0}) {
			if bytes.Equal(entry, needle) {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}
