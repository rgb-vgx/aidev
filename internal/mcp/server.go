// Package mcp exposes aidev's orchestration to an MCP client such as Claude Code.
//
// The planner on the other side of this protocol never learns how OpenCode is
// invoked; it asks aidev for a task to be created, run and verified, and reads a
// structured result. That division is the point of the whole system, so the tool
// descriptions here are written to make the honest use obvious: verification is
// mandatory, and the agent's own report is never the evidence.
//
// Transport is stdio, which means stdout carries JSON-RPC. Nothing in aidev writes
// to stdout in this mode; logging goes to stderr (docs/research.md §3.3).
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/worker"
)

// ServerName identifies aidev to the client.
const ServerName = "aidev"

// Waiting bounds for aidev_run_task.
//
// A task takes as long as the agent does — measured between 9 and 656 seconds
// (docs/research.md §7c) — while an MCP client will not wait indefinitely for a
// tool call. So a run continues in the background and the tool waits only for a
// bounded time before returning what it knows, with instructions to poll.
const (
	DefaultWaitSeconds = 120
	MaxWaitSeconds     = 900

	// shutdownGrace bounds how long Serve waits for background runs to record
	// their outcome once the client disconnects.
	shutdownGrace = 45 * time.Second
)

// Opener connects the server to its dependencies on first use, so that the
// server can start and answer initialize and tools/list while the database is
// still down, and only a tool call pays for connecting.
type Opener func(ctx context.Context) (*worker.Orchestrator, *store.Store, error)

// Server adapts the orchestrator to MCP.
type Server struct {
	orchestrator *worker.Orchestrator
	store        *store.Store
	open         Opener
	connMu       sync.Mutex
	logger       *slog.Logger
	version      string

	// baseCtx outlives an individual tool call, so a run started by one call
	// survives that call returning. It is cancelled when Serve returns, which is
	// what lets an in-flight task be recorded as cancelled rather than abandoned
	// in RUNNING.
	baseCtx context.Context

	mu   sync.Mutex
	runs map[uuid.UUID]*backgroundRun
	wg   sync.WaitGroup
}

// backgroundRun is a task execution in progress.
type backgroundRun struct {
	done    chan struct{}
	outcome worker.Outcome
	err     error
}

// New builds a Server that is already connected.
func New(orchestrator *worker.Orchestrator, st *store.Store, version string, logger *slog.Logger) *Server {
	return NewDeferred(func(context.Context) (*worker.Orchestrator, *store.Store, error) {
		return orchestrator, st, nil
	}, version, logger)
}

// NewDeferred builds a Server that connects on the first tool call. The opener
// is called at most once successfully; its result is kept for every later call.
// Initialize and tools/list never trigger it, so a client can connect while the
// database is still down.
func NewDeferred(open Opener, version string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = logging.Discard()
	}
	return &Server{
		open:    open,
		logger:  logger,
		version: version,
		runs:    map[uuid.UUID]*backgroundRun{},
	}
}

// connected returns the orchestrator and store, opening them on first use. The
// mutex is held across the open so two concurrent first calls cannot open
// twice; a failure is not kept, so the next call tries again. An opener error
// is returned unchanged so the caller can report it as a tool error.
func (s *Server) connected(ctx context.Context) (*worker.Orchestrator, *store.Store, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.orchestrator != nil && s.store != nil {
		return s.orchestrator, s.store, nil
	}
	orchestrator, st, err := s.open(ctx)
	if err != nil {
		return nil, nil, err
	}
	s.orchestrator = orchestrator
	s.store = st
	return orchestrator, st, nil
}

// MCPServer builds the protocol server with every tool registered.
func (s *Server) MCPServer() *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{
		Name:    ServerName,
		Title:   "aidev task orchestration",
		Version: s.version,
	}, &sdk.ServerOptions{
		Instructions: instructions,
	})
	s.register(server)
	return server
}

// Serve runs the server on stdio until the client disconnects or ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	return s.ServeTransport(ctx, &sdk.StdioTransport{})
}

// ServeTransport runs the server over any transport.
//
// Serve delegates to it so that a test using an in-memory transport exercises the
// same lifecycle as production, including how background runs are bound to the
// server's context and drained on shutdown.
func (s *Server) ServeTransport(ctx context.Context, transport sdk.Transport) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.baseCtx = runCtx

	s.logger.InfoContext(ctx, "mcp server starting", "version", s.version)

	err := s.MCPServer().Run(ctx, transport)

	// Stop accepting work, then give in-flight runs a chance to record their
	// outcome. Without this, quitting mid-task would leave a row in RUNNING and
	// a worktree unaccounted for.
	cancel()
	s.awaitBackgroundRuns()

	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp server: %w", err)
	}
	s.logger.InfoContext(ctx, "mcp server stopped")
	return nil
}

func (s *Server) awaitBackgroundRuns() {
	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(shutdownGrace):
		s.logger.Warn("gave up waiting for background task runs to finish recording",
			"grace", shutdownGrace.String())
	}
}

// instructions are sent to the client on initialize. They exist to make the
// correct use of aidev evident without the planner having to infer it.
const instructions = `aidev runs implementation tasks in isolated git worktrees and verifies them itself.

Delegate a task with aidev_create_task, then aidev_run_task. aidev creates a git
worktree, runs a coding agent inside it, and then runs the task's own verification
commands. Only those commands decide the outcome: a task reaches SUCCEEDED only if
they pass, regardless of what the agent reports about its own work.

Every task therefore needs at least one verification command, and a task whose
success you could not check with a command is not a good fit for aidev.

A run takes as long as the agent does, often minutes. aidev_run_task waits for a
bounded time and then returns with status RUNNING; call aidev_get_task_result to
find out how it ended, or aidev_get_task_events to see how far it has got.

A successful task leaves a commit on its own branch (aidev/<ref>). Nothing is
merged, and your working tree is never touched. A failed task leaves its worktree
in place so the partial work can be inspected.`
