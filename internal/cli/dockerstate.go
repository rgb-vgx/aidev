package cli

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"aidev/internal/doctor"
	"aidev/internal/setup"
)

// dockerProbeTimeout bounds each docker command the doctor runs: a daemon
// that hangs must not hang the diagnosis meant to explain it.
const dockerProbeTimeout = 10 * time.Second

// dockerState asks Docker whether its daemon answers and what state the
// container `aidev setup` creates is in, and systemd how Docker is started.
func dockerState(ctx context.Context) doctor.DockerState {
	return probeDocker(ctx, setup.ExecDocker(), systemctlUnits(), setup.DefaultPostgresOptions().Container)
}

// unitQuery answers `systemctl <verb> <unit>` with the state systemctl
// prints, or "" when it cannot tell.
type unitQuery func(ctx context.Context, verb, unit string) string

// systemctlUnits returns a unitQuery backed by systemctl, or nil where there is
// no systemctl (macOS, a container without systemd).
func systemctlUnits() unitQuery {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return nil
	}
	return func(ctx context.Context, verb, unit string) string {
		ctx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
		defer cancel()
		// is-active and is-enabled exit non-zero for "inactive" and
		// "disabled" and still print the state, so stdout is read either way.
		out, _ := exec.CommandContext(ctx, path, verb, "--", unit).Output()
		state, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return state
	}
}

// probeDocker is dockerState with its dependencies passed in, for tests.
func probeDocker(ctx context.Context, d setup.Docker, units unitQuery, container string) doctor.DockerState {
	st := doctor.DockerState{Name: container}

	// systemd first: `docker info` below can start the daemon through
	// docker.socket, after which docker.service would always read active.
	if units != nil {
		st.ServiceActive = units(ctx, "is-active", "docker.service")
		st.ServiceEnabled = units(ctx, "is-enabled", "docker.service")
		st.SocketEnabled = units(ctx, "is-enabled", "docker.socket")
	}

	infoCtx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	if _, err := d.Run(infoCtx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			st.Daemon = doctor.DaemonDenied
		} else {
			st.Daemon = doctor.DaemonDown
		}
		return st
	}
	st.Daemon = doctor.DaemonUp

	inspectCtx, cancelInspect := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancelInspect()
	out, err := d.Run(inspectCtx, "inspect", "-f", "{{.State.Status}}", container)
	if err == nil {
		st.Container = strings.TrimSpace(out)
	}
	return st
}
