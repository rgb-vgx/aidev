package cli

import (
	"context"
	"flag"
	"fmt"

	aidevmcp "aidev/internal/mcp"
)

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

  claude mcp add --scope project aidev -- %s mcp

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

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	logger := app.orchestrator.Logger
	server := aidevmcp.New(app.orchestrator, app.store, env.Version, logger)
	return server.Serve(ctx)
}
