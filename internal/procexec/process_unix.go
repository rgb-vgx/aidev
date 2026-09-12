//go:build unix

package procexec

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a new process group whose id equals its pid,
// so that signalling the group cannot reach aidev itself.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateGroup asks the child's whole process group to terminate.
//
// Signalling the group rather than the pid is what catches helper processes:
// `go test` starts compilers and test binaries, and killing only the parent
// would leave them running. Only the group aidev created is ever signalled —
// never a process matched by name, which during Phase 0 research nearly led to
// killing an unrelated session (docs/research.md §2.7).
func terminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		// The group may already be gone, which is not a failure. Fall back to
		// the process itself in case the group could not be established.
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}
