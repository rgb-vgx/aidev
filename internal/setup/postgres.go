package setup

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// Docker runs the docker command line.
type Docker interface {
	// Run executes `docker args...` and returns its standard output. When docker
	// exits non-zero, the error includes its standard error.
	Run(ctx context.Context, args ...string) (string, error)
}

// PostgresOptions describes the PostgreSQL container setup manages. The
// defaults match docker-compose.yml, so either way of starting it gives the
// same database.
type PostgresOptions struct {
	Image     string // for example postgres:16-alpine, or a private registry's copy
	Container string // container name
	Volume    string // named volume holding the data
	Port      int    // host port published to the container's 5432
	User      string
	Password  string
	Database  string
}

// PostgresAction says what EnsurePostgres had to do.
type PostgresAction string

const (
	PostgresCreated        PostgresAction = "created"
	PostgresStarted        PostgresAction = "started"
	PostgresAlreadyRunning PostgresAction = "already running"
)

// DefaultPostgresOptions returns the options docker-compose.yml uses by default.
func DefaultPostgresOptions() PostgresOptions {
	return PostgresOptions{
		Image:     "postgres:16-alpine",
		Container: "aidev-postgres",
		Volume:    "aidev-pgdata",
		Port:      5434,
		User:      "aidev",
		Password:  "aidev",
		Database:  "aidev",
	}
}

// DatabaseURL is the connection string for the container, from the host.
func (o PostgresOptions) DatabaseURL() string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(o.User, o.Password),
		Host:     fmt.Sprintf("127.0.0.1:%d", o.Port),
		Path:     "/" + o.Database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// EnsurePostgres makes sure the container exists, runs and is healthy,
// creating or starting it as needed and then waiting for its healthcheck.
func EnsurePostgres(ctx context.Context, d Docker, o PostgresOptions, poll, timeout time.Duration) (PostgresAction, error) {
	statusOut, err := d.Run(ctx, "inspect", "-f", "{{.State.Status}}", o.Container)
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such object") {
			return "", fmt.Errorf("docker: %w", err)
		}
		// The container is missing: create it with the compose settings.
		// The compose project name is not pinned, so compose's volume is
		// <project>_aidev-pgdata, and task worktrees leave more of them; see
		// docs/architecture.md. Creating the container on a new, empty volume
		// while compose volumes exist would hide earlier data, and picking one
		// would be a guess, so stop and let the user choose.
		if _, err := d.Run(ctx, "volume", "inspect", o.Volume); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "no such volume") {
				return "", fmt.Errorf("docker: %w", err)
			}
			lsOut, err := d.Run(ctx, "volume", "ls", "-q", "--filter", "label=com.docker.compose.volume=aidev-pgdata")
			if err != nil {
				return "", fmt.Errorf("docker: %w", err)
			}
			var names []string
			for _, line := range strings.Split(lsOut, "\n") {
				if name := strings.TrimSpace(line); name != "" {
					names = append(names, name)
				}
			}
			if len(names) > 0 {
				return "", fmt.Errorf("docker compose volumes exist that may hold earlier aidev data: %s; run `aidev setup --postgres-volume <name>` to use one of them, or run `docker volume create %s` and then `aidev setup` again to start with an empty database", strings.Join(names, ", "), o.Volume)
			}
		}
		if _, err := d.Run(ctx, "run",
			"-d",
			"--name", o.Container,
			"--restart", "unless-stopped",
			"-e", "POSTGRES_USER="+o.User,
			"-e", "POSTGRES_PASSWORD="+o.Password,
			"-e", "POSTGRES_DB="+o.Database,
			"-e", "POSTGRES_INITDB_ARGS=--encoding=UTF8 --locale=C",
			"-p", fmt.Sprintf("127.0.0.1:%d:5432", o.Port),
			"-v", o.Volume+":/var/lib/postgresql/data",
			"--health-cmd", "pg_isready -U "+o.User+" -d "+o.Database,
			"--health-interval", "2s",
			"--health-timeout", "3s",
			"--health-retries", "30",
			"--health-start-period", "5s",
			o.Image,
		); err != nil {
			return "", fmt.Errorf("docker: %w", err)
		}
		return waitHealthy(ctx, d, o, PostgresCreated, poll, timeout)
	}

	status := strings.TrimSpace(statusOut)
	var action PostgresAction
	switch status {
	case "running":
		// Already up; just wait for it to be healthy.
		action = PostgresAlreadyRunning
	case "exited", "created":
		// Stopped but present: start it rather than recreating it.
		if _, err := d.Run(ctx, "start", o.Container); err != nil {
			return "", fmt.Errorf("docker: %w", err)
		}
		action = PostgresStarted
	default:
		return "", fmt.Errorf("postgres container %s has unexpected status %q: inspect it with `docker ps -a`", o.Container, status)
	}

	return waitHealthy(ctx, d, o, action, poll, timeout)
}

// waitHealthy polls the container's health status until it is healthy,
// the timeout expires, or the context is cancelled.
func waitHealthy(ctx context.Context, d Docker, o PostgresOptions, action PostgresAction, poll, timeout time.Duration) (PostgresAction, error) {
	deadline := time.Now().Add(timeout)
	for {
		hOut, err := d.Run(ctx, "inspect", "-f", "{{.State.Health.Status}}", o.Container)
		if err != nil {
			return "", fmt.Errorf("docker: %w", err)
		}
		switch strings.TrimSpace(hOut) {
		case "healthy":
			return action, nil
		case "unhealthy":
			return "", fmt.Errorf("PostgreSQL in container %s is unhealthy: run `docker logs %s` to investigate", o.Container, o.Container)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("postgres container %s did not become healthy within %s: run `docker logs %s` to investigate", o.Container, timeout, o.Container)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for postgres container %s: %w: run `docker logs %s` to investigate", o.Container, ctx.Err(), o.Container)
		case <-time.After(poll):
		}
	}
}

// execDocker is a Docker that runs the real docker executable.
type execDocker struct{}

// ExecDocker returns a Docker that runs the real docker executable.
func ExecDocker() Docker { return execDocker{} }

// Run executes `docker args...`, returning stdout. On failure the error wraps
// the exit error with the trimmed standard error.
func (execDocker) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
