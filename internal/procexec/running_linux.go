//go:build linux

package procexec

import (
	"os"
	"strconv"
	"strings"
)

// ProcessRunning reports whether pid is a live process. A zombie — exited,
// not yet reaped by its parent — is not running: it holds no resources and
// does no work, and treating it as running would make a caller wait for a
// process that already stopped.
func ProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the command name, which is in parentheses and may
	// itself contain spaces or parentheses: read after the last ')'.
	stat := string(raw)
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return true
	}
	return stat[i+2] != 'Z' && stat[i+2] != 'X'
}
