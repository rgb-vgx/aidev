//go:build !unix

package procexec

import (
	"os/exec"
	"time"
)

// On platforms without POSIX process groups, fall back to signalling the process
// itself. aidev is developed and tested on Linux; this exists so the package
// still builds elsewhere rather than silently pretending to have group cleanup.
func setProcessGroup(*exec.Cmd) {}

func terminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// killGroupAfter is the escalation after terminateGroup. Without process
// groups there is nothing to signal but the process itself, which WaitDelay has
// already killed by the time this runs.
func killGroupAfter(cmd *exec.Cmd, _ time.Time) error {
	return nil
}

// groupAlive is the existence probe that drives the reap. Without process
// groups there is only the direct child, which Wait has already reaped, so
// nothing can be left over.
func groupAlive(*exec.Cmd) bool { return false }
