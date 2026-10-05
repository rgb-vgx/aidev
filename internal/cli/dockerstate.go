package cli

import (
	"context"
	"strings"
	"time"

	"aidev/internal/doctor"
	"aidev/internal/setup"
)

// dockerProbeTimeout bounds each docker command the doctor runs: a daemon
// that hangs must not hang the diagnosis meant to explain it.
const dockerProbeTimeout = 10 * time.Second

// dockerState asks Docker whether its daemon answers and what state the
// container `aidev setup` creates is in.
func dockerState(ctx context.Context) doctor.DockerState {
	return probeDocker(ctx, setup.ExecDocker(), setup.DefaultPostgresOptions().Container)
}

// probeDocker is dockerState with its dependencies passed in, for tests.
func probeDocker(ctx context.Context, d setup.Docker, container string) doctor.DockerState {
	st := doctor.DockerState{Name: container}

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
