package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aidev/internal/config"
	"aidev/internal/setup"
	"aidev/internal/store"
	"aidev/migrations"
)

// runSetup prepares aidev on this machine: the database, conf.json and the
// schema. It starts setup's own PostgreSQL container unless --database-url
// names an existing database, writes conf.json unless there is one, migrates,
// then says what to add where.
func runSetup(ctx context.Context, env *Env, args []string) error {
	defaults := setup.DefaultPostgresOptions()

	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	configFlag := fs.String("config", "", "where to write conf.json (default: aidev/conf.json in your user config directory)")
	databaseURL := fs.String("database-url", "", "an existing PostgreSQL database to use instead of starting a container")
	postgresImage := fs.String("postgres-image", defaults.Image, "image for setup's PostgreSQL container, for example a copy in your company's registry")
	postgresPort := fs.Int("postgres-port", defaults.Port, "host port for setup's PostgreSQL container (bound to 127.0.0.1)")
	workspaceRoot := fs.String("workspace-root", "", "where task worktrees go (default: aidev's own default)")
	if err := fs.Parse(args); err != nil {
		return usagef("aidev setup: %v", err)
	}
	if fs.NArg() > 0 {
		return usagef("aidev setup takes no arguments")
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["database-url"] && set["postgres-image"] {
		return usagef("aidev setup: --postgres-image configures setup's own container and cannot be combined with --database-url")
	}
	if set["database-url"] && set["postgres-port"] {
		return usagef("aidev setup: --postgres-port configures setup's own container and cannot be combined with --database-url")
	}

	configPath := *configFlag
	if configPath == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		configPath = filepath.Join(dir, "aidev", "conf.json")
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	configPath = absConfig

	wsRoot := *workspaceRoot
	if wsRoot != "" {
		abs, err := filepath.Abs(wsRoot)
		if err != nil {
			return err
		}
		wsRoot = abs
	}

	postgres := defaults
	postgres.Image = *postgresImage
	postgres.Port = *postgresPort

	opts := setup.Options{
		ConfigPath:    configPath,
		WorkspaceRoot: wsRoot,
		DatabaseURL:   *databaseURL,
		Postgres:      postgres,
	}

	steps := setup.Steps{
		EnsurePostgres: func(ctx context.Context, o setup.PostgresOptions) (setup.PostgresAction, error) {
			fmt.Fprintf(env.Stdout, "Starting PostgreSQL in container %s (image %s) ...\n", o.Container, o.Image)
			return setup.EnsurePostgres(ctx, setup.ExecDocker(), o, time.Second, 2*time.Minute)
		},
		Migrate: func(ctx context.Context, databaseURL string) ([]string, error) {
			loaded, err := store.LoadMigrations(migrations.FS)
			if err != nil {
				return nil, err
			}
			connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			db, err := store.Open(connectCtx, databaseURL)
			if err != nil {
				return nil, err
			}
			defer db.Close()
			result, err := db.Migrate(ctx, loaded)
			if err != nil {
				return nil, err
			}
			return result.Applied, nil
		},
	}

	report, err := setup.Run(ctx, opts, steps)
	if err != nil {
		return err
	}

	printSetupReport(env, opts, report)

	return nil
}

// printSetupReport says what setup did and what to do next. The database URL
// is always redacted so the password never reaches the terminal.
func printSetupReport(env *Env, opts setup.Options, report setup.Report) {
	fmt.Fprintln(env.Stdout, "aidev is set up.")
	fmt.Fprintln(env.Stdout)
	if report.ConfigCreated {
		fmt.Fprintf(env.Stdout, "  config      %s (created)\n", report.ConfigPath)
	} else {
		fmt.Fprintf(env.Stdout, "  config      %s (already there, left as it is)\n", report.ConfigPath)
	}
	fmt.Fprintf(env.Stdout, "  database    %s\n", config.RedactURL(report.DatabaseURL))
	if report.Postgres != "" {
		fmt.Fprintf(env.Stdout, "  PostgreSQL  container %s: %s\n", opts.Postgres.Container, report.Postgres)
	}
	if len(report.Applied) == 0 {
		fmt.Fprintln(env.Stdout, "  migrations  already up to date")
	} else {
		fmt.Fprintf(env.Stdout, "  migrations  %d applied\n", len(report.Applied))
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Next steps:")
	fmt.Fprintln(env.Stdout)
	if os.Getenv("AIDEV_CONFIG") == report.ConfigPath {
		fmt.Fprintln(env.Stdout, "  1. AIDEV_CONFIG already names this file.")
	} else {
		fmt.Fprintln(env.Stdout, "  1. Add this line to your shell profile (~/.bashrc or ~/.zshrc) and open a new terminal:")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintf(env.Stdout, "       export AIDEV_CONFIG=%s\n", quoteShellPath(report.ConfigPath))
		fmt.Fprintln(env.Stdout)
	}
	fmt.Fprintln(env.Stdout, "  2. For Claude Code, add this to the \"env\" object in ~/.claude/settings.json:")
	fmt.Fprintln(env.Stdout)
	encoded, _ := json.Marshal(report.ConfigPath)
	fmt.Fprintf(env.Stdout, "       \"AIDEV_CONFIG\": %s\n", string(encoded))
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "  3. Run `aidev doctor` to check git and the coding agent.")
}

// quoteShellPath renders a path for the shell export line: bare when it holds
// only letters, digits and _ . / -, otherwise single-quoted with an embedded
// single quote closed, escaped and reopened.
func quoteShellPath(path string) string {
	safe := path != ""
	for _, r := range path {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '/' || r == '-' {
			continue
		}
		safe = false
		break
	}
	if safe {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}
