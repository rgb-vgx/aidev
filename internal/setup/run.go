package setup

import "context"

// Options says what Run prepares.
type Options struct {
	// ConfigPath is the absolute path of conf.json.
	ConfigPath string
	// WorkspaceRoot is written to a new conf.json when not empty.
	WorkspaceRoot string
	// DatabaseURL names an existing database to use. Empty means setup's own
	// PostgreSQL container, described by Postgres.
	DatabaseURL string
	// Postgres is the container setup runs when DatabaseURL is empty.
	Postgres PostgresOptions
}

// Steps are the parts of setup that reach outside the process, so that Run can
// be tested without Docker or a database.
type Steps struct {
	// EnsurePostgres creates, starts or keeps the container and waits until it
	// is healthy.
	EnsurePostgres func(ctx context.Context, o PostgresOptions) (PostgresAction, error)
	// Migrate applies pending migrations and returns the versions it applied.
	Migrate func(ctx context.Context, databaseURL string) ([]string, error)
}

// Report is what Run did.
type Report struct {
	ConfigPath    string
	ConfigCreated bool
	// DatabaseURL is the database aidev now uses, password included; redact it
	// before showing it.
	DatabaseURL string
	// Postgres is what happened to setup's container, or "" when the database
	// is not setup's container.
	Postgres PostgresAction
	// Applied lists the migrations this run applied.
	Applied []string
}

// Run prepares aidev: the database, conf.json and the schema.
// Specified by run_test.go; not implemented yet.
func Run(ctx context.Context, opts Options, steps Steps) (Report, error) {
	return Report{}, nil
}
