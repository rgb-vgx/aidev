package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	aidevmcp "aidev/internal/mcp"
	"aidev/internal/store"
	"aidev/internal/worker"
)

// mcpConnectTimeout bounds one deferred connection attempt. A tool call waits
// on it while the client watches, so it is shorter than the one-shot
// connectTimeout: a database that is still starting must surface as a quick
// tool error the client can retry, not a long stall that looks like a hang.
const mcpConnectTimeout = 5 * time.Second

// mcpRecoverTimeout bounds the recovery that runs when the database first
// connects. It is best-effort: a slow database delays the first tool call at
// most this long, and the outcome is logged to stderr — stdout belongs to the
// protocol (research C1).
const mcpRecoverTimeout = 10 * time.Second

// runMCPServer starts the MCP server on stdio.
//
// Nothing may be written to stdout here: it is the JSON-RPC channel. The logger
// writes to stderr by construction, and this command prints nothing itself
// (docs/research.md §3.3).
func runMCPServer(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev mcp

Starts the MCP server on stdin/stdout for a client such as Claude Code. It is not
meant to be run by hand: a client launches it. Register it with

  claude mcp add --scope user aidev -- %s mcp

Logs go to stderr. Nothing else is written to stdout, which belongs to the
protocol.
`, "/abs/path/to/aidev")
	}
	if err := fs.Parse(args); err != nil {
		return usagef("aidev mcp: %v", err)
	}
	if fs.NArg() > 0 {
		return usagef("aidev mcp takes no arguments")
	}

	cfg, logger, err := loadAppConfig()
	if err != nil {
		return err
	}

	// The database is connected on first use, so the server stays up while it
	// is still starting and a client that never retries a launch stays usable.
	logger.InfoContext(ctx, "mcp server starting; the database connects on first tool use")

	lazy := newLazyApp(func(attemptCtx context.Context) (*app, error) {
		connectCtx, cancel := context.WithTimeout(attemptCtx, mcpConnectTimeout)
		defer cancel()
		a, err := connectApp(connectCtx, cfg, logger)
		if err != nil {
			logger.WarnContext(ctx, "mcp database connection failed; will retry on the next tool call", "error", err.Error())
			return nil, err
		}
		logger.InfoContext(ctx, "mcp database connected")
		// Recovery runs once per connection: a planner reconnecting after a
		// restart must not inherit tasks that a killed run left RUNNING
		// (research C1). Bounded and best-effort — failing to recover must
		// not keep the server from answering tool calls.
		recoverCtx, cancelRecover := context.WithTimeout(attemptCtx, mcpRecoverTimeout)
		defer cancelRecover()
		recovered, recErr := a.orchestrator.Recover(recoverCtx, false)
		switch {
		case recErr != nil:
			logger.WarnContext(ctx, "automatic recovery after connecting failed", "error", recErr.Error())
		case len(recovered) > 0:
			refs := make([]string, 0, len(recovered))
			for _, r := range recovered {
				refs = append(refs, r.Ref)
			}
			logger.InfoContext(ctx, "cancelled tasks whose lease expired", "tasks", strings.Join(refs, ", "))
		}
		return a, nil
	})
	defer lazy.close()

	open := aidevmcp.Opener(func(toolCtx context.Context) (*worker.Orchestrator, *store.Store, error) {
		a, err := lazy.get(toolCtx)
		if err != nil {
			return nil, nil, err
		}
		return a.orchestrator, a.store, nil
	})

	server := aidevmcp.NewDeferred(open, env.Version, logger)
	return server.Serve(ctx)
}
