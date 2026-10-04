//go:build unix

package procexec

import (
	"os/exec"
	"syscall"
)

// Detach puts the command in its own session so that it outlives us: a new
// session means no controlling terminal, its own process group, and none of the
// signals sent to this process — Ctrl-C, a closing terminal's SIGHUP, the MCP
// server shutting down — reaching the child (research C2).
func Detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
