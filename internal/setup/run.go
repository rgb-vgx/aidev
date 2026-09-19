package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"aidev/internal/config"
)

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
//
// It validates the config path, reuses an existing conf.json (refusing a
// contradictory DatabaseURL), starts setup's PostgreSQL container only when
// the database in use is the container's own, writes conf.json only when none
// exists and only after the database started, then migrates the database.
func Run(ctx context.Context, opts Options, steps Steps) (Report, error) {
	if opts.ConfigPath == "" || !filepath.IsAbs(opts.ConfigPath) {
		return Report{}, fmt.Errorf("setup: ConfigPath must be absolute: %q", opts.ConfigPath)
	}

	_, statErr := os.Stat(opts.ConfigPath)
	existed := statErr == nil

	var url string
	if existed {
		cfg, err := config.LoadFile(opts.ConfigPath)
		if err != nil {
			return Report{}, fmt.Errorf("%s: %w", opts.ConfigPath, err)
		}
		if opts.DatabaseURL != "" && opts.DatabaseURL != cfg.DatabaseURL {
			return Report{}, fmt.Errorf("config file %s already names a different database: edit it, or choose another config path", opts.ConfigPath)
		}
		url = cfg.DatabaseURL
	} else {
		if opts.DatabaseURL != "" {
			url = opts.DatabaseURL
		} else {
			url = opts.Postgres.DatabaseURL()
		}
	}

	var action PostgresAction
	if url == opts.Postgres.DatabaseURL() {
		var err error
		action, err = steps.EnsurePostgres(ctx, opts.Postgres)
		if err != nil {
			return Report{}, fmt.Errorf("starting PostgreSQL: %w", err)
		}
	}

	var created bool
	if !existed {
		var err error
		created, err = WriteConfig(ConfigOptions{Path: opts.ConfigPath, DatabaseURL: url, WorkspaceRoot: opts.WorkspaceRoot})
		if err != nil {
			return Report{}, err
		}
	}

	applied, err := steps.Migrate(ctx, url)
	if err != nil {
		return Report{}, fmt.Errorf("migrating the database: %w", err)
	}

	return Report{
		ConfigPath:    opts.ConfigPath,
		ConfigCreated: created,
		DatabaseURL:   url,
		Postgres:      action,
		Applied:       applied,
	}, nil
}
