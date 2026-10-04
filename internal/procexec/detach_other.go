//go:build !unix

package procexec

import "os/exec"

// Detach is a no-op here: there are no POSIX sessions to join. aidev is
// developed and tested on Linux; this exists so the package still builds
// elsewhere rather than silently pretending the child was detached.
func Detach(*exec.Cmd) {}
