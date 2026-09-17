//go:build unix

package procexec

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
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

// killGroupAfter makes sure the child's process group is gone by deadline.
//
// It is the escalation after terminateGroup: members still running at the
// deadline — ones that ignored SIGTERM — are sent SIGKILL. Until then the group is
// polled, so a group that exits within its grace period is never killed and Run
// does not wait longer than it has to. Only the group aidev created is ever
// signalled, never a process matched by name (docs/research.md §2.7).
func killGroupAfter(cmd *exec.Cmd, deadline time.Time) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := -cmd.Process.Pid
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pgid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(groupPollInterval)
	}
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// groupPollInterval is how often killGroupAfter checks whether the group is gone.
const groupPollInterval = 50 * time.Millisecond
