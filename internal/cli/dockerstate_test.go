package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aidev/internal/doctor"
)

// fakeDocker answers `docker info` and `docker inspect` from a script.
type fakeDocker struct {
	info, inspect       string
	infoErr, inspectErr error
}

func (f fakeDocker) Run(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "info":
		return f.info, f.infoErr
	case "inspect":
		return f.inspect, f.inspectErr
	}
	return "", errors.New("unexpected docker " + strings.Join(args, " "))
}

func TestProbeDocker(t *testing.T) {
	cases := []struct {
		name          string
		d             fakeDocker
		daemon, state string
	}{
		{"daemon down", fakeDocker{infoErr: errors.New("exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")}, doctor.DaemonDown, ""},
		{"permission denied", fakeDocker{infoErr: errors.New("exit status 1: permission denied while trying to connect to the Docker daemon socket")}, doctor.DaemonDenied, ""},
		{"stopped container", fakeDocker{info: "27.0.1\n", inspect: "exited\n"}, doctor.DaemonUp, "exited"},
		{"no container", fakeDocker{info: "27.0.1\n", inspectErr: errors.New("exit status 1: Error: No such object: aidev-postgres")}, doctor.DaemonUp, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := probeDocker(context.Background(), tc.d, "aidev-postgres")
			if got.Daemon != tc.daemon || got.Container != tc.state || got.Name != "aidev-postgres" {
				t.Errorf("probeDocker = %+v, want daemon %q container %q", got, tc.daemon, tc.state)
			}
		})
	}
}
