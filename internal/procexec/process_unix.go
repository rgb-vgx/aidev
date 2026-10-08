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

// terminateGroup asks the child's whole process group to terminate, and every
// descendant that carries the run's tag.
//
// Signalling the group rather than the pid is what catches helper processes:
// `go test` starts compilers and test binaries, and killing only the parent
// would leave them running. The tag reaches what the group cannot: a
// descendant that started its own session (setsid, as a test runner does when
// it starts an application under xvfb-run) or its own group. Only the group
// aidev created and the processes carrying the marker aidev gave it are ever
// signalled — never a process matched by name, which during Phase 0 research
// nearly led to killing an unrelated session (docs/research.md §2.7).
func terminateGroup(cmd *exec.Cmd, tag string) error {
	if cmd.Process == nil {
		return nil
	}
	signalTagged(tag, syscall.SIGTERM)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		// The group may already be gone, which is not a failure. Fall back to
		// the process itself in case the group could not be established.
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}

// killGroupAfter makes sure the child's process group, and every descendant
// carrying the run's tag, is gone by deadline.
//
// It is the escalation after terminateGroup: what is still running at the
// deadline — ones that ignored SIGTERM — is sent SIGKILL. Until then both are
// polled, so a run whose processes exit within the grace period is never
// killed and Run does not wait longer than it has to.
func killGroupAfter(cmd *exec.Cmd, tag string, deadline time.Time) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := -cmd.Process.Pid
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pgid, 0); errors.Is(err, syscall.ESRCH) && len(taggedProcesses(tag)) == 0 {
			return nil
		}
		time.Sleep(groupPollInterval)
	}
	signalTagged(tag, syscall.SIGKILL)
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// signalTagged sends sig to every process carrying tag.
func signalTagged(tag string, sig syscall.Signal) {
	for _, pid := range taggedProcesses(tag) {
		_ = syscall.Kill(pid, sig)
	}
}

// groupPollInterval is how often killGroupAfter checks whether the group is gone.
const groupPollInterval = 50 * time.Millisecond

// groupAlive reports whether any member of the child's process group, or any
// descendant carrying the run's tag, is still running. It is the existence
// probe that decides whether a finished run owes a reap: ESRCH means the group
// is already gone, which is the normal case, and anything else counts as
// alive — EPERM included, because a group we cannot signal is still a group
// that is running.
func groupAlive(cmd *exec.Cmd, tag string) bool {
	if cmd.Process == nil {
		return false
	}
	err := syscall.Kill(-cmd.Process.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM) || len(taggedProcesses(tag)) > 0
}
