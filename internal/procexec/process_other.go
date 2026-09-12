//go:build !unix

package procexec

import "os/exec"

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
