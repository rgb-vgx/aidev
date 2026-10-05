package cli

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/git"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/tracing"
	"aidev/internal/worker"
	"aidev/migrations"
)

// connectTimeout bounds startup so a wrong database.url fails quickly instead of
// appearing to hang.
const connectTimeout = 15 * time.Second

// app is the wiring every task command needs: configuration, a database, a
// worktree manager, an agent backend, and the orchestrator over them.
type app struct {
	cfg          config.Config
	store        *store.Store
	orchestrator *worker.Orchestrator
	close        func()
}

// tracingFlushTimeout bounds how long a command waits on exit for spans to reach the
// collector. A command must not hang because a backend is slow, and losing a trace is
// preferable to losing the command's own output.
const tracingFlushTimeout = 5 * time.Second

// openApp builds the application. The caller must call close.
//
// Logs go to stderr while command output goes to stdout, so that `aidev task get
// --json | jq` works while diagnostics remain visible in the terminal.
func openApp(ctx context.Context) (*app, error) {
	cfg, logger, err := loadAppConfig()
	if err != nil {
		return nil, err
	}
	return connectApp(ctx, cfg, logger)
}

// loadAppConfig resolves the configuration without touching the network, so a
// misconfiguration fails at once even when the database is reached lazily.
func loadAppConfig() (config.Config, *slog.Logger, error) {
	cfg, err := config.Load(config.OSLookup)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, logging.New(cfg.LogLevel), nil
}

// connectApp wires everything that needs the database. It is the connection
// stage of openApp, reused by the MCP server's deferred opener.
func connectApp(ctx context.Context, cfg config.Config, logger *slog.Logger) (*app, error) {
	if logger == nil {
		logger = logging.New(cfg.LogLevel)
	}

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	db, err := store.Open(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("%w\n\nIs PostgreSQL running? `make db-up` starts it, and `aidev migrate` applies the schema", err)
	}
	// Refuse a schema this binary does not recognise before anything is built
	// on top of it: otherwise a stale database fails halfway through a command,
	// or mid-run inside a worker, as a missing column.
	if err := requireCurrentSchema(connectCtx, db); err != nil {
		db.Close()
		return nil, err
	}

	gitManager, err := git.NewManager(cfg.WorkspaceRoot)
	if err != nil {
		db.Close()
		return nil, err
	}
	gitManager.MaxOutputBytes = cfg.MaxOutputBytes

	// The backend is selectable so that the Codex implementation is reachable;
	// OpenCode remains the default.
	opencode := agent.NewOpenCode(cfg.OpenCodeCommand, cfg.OpenCodeModel)
	// One OpenCode database per task, so tasks can run side by side
	// (docs/research.md §7k).
	opencode.DBDir = opencodeDBDir(cfg)
	var backend agent.Backend = opencode
	if cfg.AgentBackend == config.BackendCodex {
		backend = agent.NewCodex(agent.CodexOptions{
			Command: cfg.CodexCommand,
			Profile: cfg.CodexProfile,
			Model:   cfg.CodexModel,
			Sandbox: cfg.CodexSandbox,
		})
	}

	// Tracing is wired here because this is the only place that knows a command is
	// starting and ending. internal/worker creates spans against the global
	// provider, so without this every one of them is a no-op — which is exactly
	// what happened when the instrumentation was added and this was forgotten: the
	// tests passed because they install their own provider, and a real run produced
	// no trace at all.
	stopTracing := func(context.Context) error { return nil }
	tracingCfg, err := tracing.FromSettings(cfg.Tracing)
	if err != nil {
		// A misconfigured exporter must not stop aidev from doing its job, but it
		// must be visible rather than silently disabling observability.
		logger.WarnContext(ctx, "tracing is disabled: its configuration is invalid", "error", err.Error())
	} else if tracingCfg.Enabled {
		if _, shutdown, err := tracing.Start(ctx, tracingCfg); err != nil {
			logger.WarnContext(ctx, "tracing is disabled: the exporter could not be created",
				"endpoint", tracingCfg.TracesEndpoint, "error", err.Error())
		} else {
			stopTracing = shutdown
			logger.DebugContext(ctx, "tracing enabled", "endpoint", tracingCfg.TracesEndpoint)
		}
	}

	return &app{
		cfg:          cfg,
		store:        db,
		orchestrator: worker.New(db, gitManager, backend, cfg, logger),
		close: func() {
			// Flush first: the spans describe work the database rows also
			// describe, and a detached context is used so that a cancelled
			// command still exports what it recorded.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tracingFlushTimeout)
			defer cancel()
			if err := stopTracing(flushCtx); err != nil {
				logger.WarnContext(ctx, "could not flush traces", "error", err.Error())
			}
			db.Close()
		},
	}, nil
}

// requireCurrentSchema reports the migrations this binary carries that the
// database has not applied yet. `aidev migrate`, `aidev doctor` and `aidev
// setup` do not connect through connectApp, so the commands that repair the
// schema stay reachable exactly when this refuses everything else.
func requireCurrentSchema(ctx context.Context, db *store.Store) error {
	loaded, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		return fmt.Errorf("read the embedded migrations: %w", err)
	}
	pending, err := db.PendingMigrations(ctx, loaded)
	if err != nil {
		// Includes an edited applied migration: Migrate refuses that too, and a
		// command must not be the first thing to trip over it.
		return fmt.Errorf("check the database schema: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}
	noun := "migrations"
	if len(pending) == 1 {
		noun = "migration"
	}
	return fmt.Errorf("database schema is behind: %d %s pending (%s); run `aidev migrate` to apply them",
		len(pending), noun, strings.Join(pending, ", "))
}

// opencodeDBDir is where each task's OpenCode database lives: next to the
// worktrees, so it is cleaned up with them and never inside a repository.
func opencodeDBDir(cfg config.Config) string {
	return filepath.Join(cfg.WorkspaceRoot, "opencode-db")
}
