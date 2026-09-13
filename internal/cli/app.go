package cli

import (
	"context"
	"fmt"
	"time"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/git"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/worker"
)

// connectTimeout bounds startup so a wrong DATABASE_URL fails quickly instead of
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

// openApp builds the application. The caller must call close.
//
// Logs go to stderr while command output goes to stdout, so that `aidev task get
// --json | jq` works while diagnostics remain visible in the terminal.
func openApp(ctx context.Context) (*app, error) {
	cfg, err := config.Load(config.OSLookup)
	if err != nil {
		return nil, err
	}
	logger := logging.New(cfg.LogLevel)

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	db, err := store.Open(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("%w\n\nIs PostgreSQL running? `make db-up` starts it, and `aidev migrate` applies the schema", err)
	}

	gitManager, err := git.NewManager(cfg.WorkspaceRoot)
	if err != nil {
		db.Close()
		return nil, err
	}
	gitManager.MaxOutputBytes = cfg.MaxOutputBytes

	backend := agent.NewOpenCode(cfg.OpenCodeCommand, cfg.OpenCodeModel)

	return &app{
		cfg:          cfg,
		store:        db,
		orchestrator: worker.New(db, gitManager, backend, cfg, logger),
		close:        db.Close,
	}, nil
}
