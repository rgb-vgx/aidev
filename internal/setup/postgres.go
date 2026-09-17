package setup

import (
	"context"
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
func DefaultPostgresOptions() PostgresOptions { return PostgresOptions{} }

// DatabaseURL is the connection string for the container, from the host.
func (o PostgresOptions) DatabaseURL() string { return "" }

// EnsurePostgres makes sure the container exists, runs and is healthy.
// Specified by postgres_test.go; not implemented yet.
func EnsurePostgres(ctx context.Context, d Docker, o PostgresOptions, poll, timeout time.Duration) (PostgresAction, error) {
	return "", nil
}

// ExecDocker returns a Docker that runs the real docker executable.
func ExecDocker() Docker { return nil }
