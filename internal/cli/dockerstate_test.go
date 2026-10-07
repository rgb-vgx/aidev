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
			got := probeDocker(context.Background(), tc.d, nil, "aidev-postgres")
			if got.Daemon != tc.daemon || got.Container != tc.state || got.Name != "aidev-postgres" {
				t.Errorf("probeDocker = %+v, want daemon %q container %q", got, tc.daemon, tc.state)
			}
		})
	}
}

// The systemd units are read before any docker command, because `docker info`
// can itself start the daemon through docker.socket: read afterwards, the
// service would always look active.
func TestProbeDockerReadsSystemdBeforeTouchingDocker(t *testing.T) {
	var order []string
	units := func(_ context.Context, verb, unit string) string {
		order = append(order, verb+" "+unit)
		switch verb + " " + unit {
		case "is-active docker.service":
			return "inactive"
		case "is-enabled docker.service":
			return "disabled"
		case "is-enabled docker.socket":
			return "enabled"
		}
		return ""
	}
	d := recordingDocker{fakeDocker: fakeDocker{info: "29.8.2\n", inspect: "running\n"}, order: &order}

	got := probeDocker(context.Background(), d, units, "aidev-postgres")
	if got.ServiceActive != "inactive" || got.ServiceEnabled != "disabled" || got.SocketEnabled != "enabled" {
		t.Errorf("units = %q/%q/%q, want inactive/disabled/enabled", got.ServiceActive, got.ServiceEnabled, got.SocketEnabled)
	}
	if len(order) == 0 || !strings.HasPrefix(order[0], "is-active") {
		t.Fatalf("calls = %v, want systemd asked first", order)
	}
	for i, call := range order {
		if strings.HasPrefix(call, "docker ") {
			for _, later := range order[i:] {
				if strings.HasPrefix(later, "is-") {
					t.Fatalf("calls = %v: systemd was asked after a docker command", order)
				}
			}
			break
		}
	}
}

// Without systemd the units stay empty and the probe works as before.
func TestProbeDockerWithoutSystemd(t *testing.T) {
	got := probeDocker(context.Background(), fakeDocker{info: "29.8.2\n", inspect: "exited\n"}, nil, "aidev-postgres")
	if got.ServiceActive != "" || got.ServiceEnabled != "" || got.SocketEnabled != "" || got.Container != "exited" {
		t.Errorf("probeDocker = %+v, want empty units and the container state", got)
	}
}

type recordingDocker struct {
	fakeDocker
	order *[]string
}

func (r recordingDocker) Run(ctx context.Context, args ...string) (string, error) {
	*r.order = append(*r.order, "docker "+strings.Join(args, " "))
	return r.fakeDocker.Run(ctx, args...)
}
