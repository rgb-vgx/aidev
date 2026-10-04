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

	// shutdownGrace bounds how long Serve waits for the run watchers to
	// notice the shutdown. They stop as soon as the context is cancelled, so
	// this is normally instant; it only covers a watcher mid-read.
	shutdownGrace = 45 * time.Second
)

// Opener connects the server to its dependencies on first use, so that the
// server can start and answer initialize and tools/list while the database is
// still down, and only a tool call pays for connecting.
type Opener func(ctx context.Context) (*worker.Orchestrator, *store.Store, error)

// Option configures a Server at construction time.
type Option func(*Server)

// WithLauncher replaces how aidev_run_task starts a run. Production leaves the
// default — a detached `aidev task run` child (research C2) — alone; the option
// exists so a test can stand a different driver in without spawning anything.
func WithLauncher(l Launcher) Option {
	return func(s *Server) { s.launch = l }
}

// WithInProcessRuns runs tasks inside the server instead of as detached child
// processes: the orchestrator the caller already built is used directly. It is
// what the integration tests use, because they hold that orchestrator and have
// no binary to spawn; it is not what the shipped server does.
func WithInProcessRuns() Option {
	return WithLauncher(inProcessLaunch)
}

// Server adapts the orchestrator to MCP.
type Server struct {
	orchestrator *worker.Orchestrator
	store        *store.Store
	open         Opener
	connMu       sync.Mutex
	attempt      *connectAttempt
	logger       *slog.Logger
	version      string

	// launch starts a run; detachedLaunch by default (research C2).
	launch Launcher
	// exePath is the executable a detached run spawns. Empty means this
	// process's own executable — the same binary, one `task run` deeper.
	// Tests point it at a stand-in.
	exePath string

	// baseCtx bounds the watchers this server keeps on runs, not the runs
	// themselves: a run is a separate process that outlives us. It is
	// cancelled when Serve returns, which stops the watchers so a closing
	// connection does not wait on a run nobody is watching anymore.
	baseCtx context.Context

	mu   sync.Mutex
	runs map[uuid.UUID]*backgroundRun
	wg   sync.WaitGroup
}

// backgroundRun is a task execution in progress: its watcher has not finished
// classifying how the driver ended. err is written before done is closed and
// read only after, so no lock is needed for it.
type backgroundRun struct {
	done chan struct{}
	err  error
}

// New builds a Server that is already connected.
func New(orchestrator *worker.Orchestrator, st *store.Store, version string, logger *slog.Logger, opts ...Option) *Server {
	return NewDeferred(func(context.Context) (*worker.Orchestrator, *store.Store, error) {
		return orchestrator, st, nil
	}, version, logger, opts...)
}

// NewDeferred builds a Server that connects on the first tool call. The opener
// is called at most once successfully; its result is kept for every later call.
// Initialize and tools/list never trigger it, so a client can connect while the
// database is still down.
func NewDeferred(open Opener, version string, logger *slog.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = logging.Discard()
	}
	s := &Server{
		open:    open,
		logger:  logger,
		version: version,
		runs:    map[uuid.UUID]*backgroundRun{},
	}
	s.launch = s.detachedLaunch
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// connectAttempt is one attempt to open the database, shared by every call waiting
// on it.
type connectAttempt struct {
	done         chan struct{}
	orchestrator *worker.Orchestrator
	store        *store.Store
	err          error
}

// connected returns the orchestrator and store, opening them on first use.
//
// Calls that arrive together share one attempt rather than queueing: holding the
// mutex across the open made five concurrent first calls cost five sequential
// attempts, five seconds each against a database that is still starting, and a
// waiter could not give up because a mutex cannot observe a context. A failure is
// not kept, so the next call tries again. An opener error is returned unchanged so
// the caller can report it as a tool error.
func (s *Server) connected(ctx context.Context) (*worker.Orchestrator, *store.Store, error) {
	s.connMu.Lock()
	if s.orchestrator != nil && s.store != nil {
		orchestrator, st := s.orchestrator, s.store
		s.connMu.Unlock()
		return orchestrator, st, nil
	}
	attempt := s.attempt
	if attempt == nil {
		attempt = &connectAttempt{done: make(chan struct{})}
		s.attempt = attempt
		go s.connect(attempt)
	}
	s.connMu.Unlock()

	select {
	case <-attempt.done:
		return attempt.orchestrator, attempt.store, attempt.err
	case <-ctx.Done():
		// The attempt belongs to every waiter, so giving up here does not cancel
		// what the others are waiting for.
		return nil, nil, ctx.Err()
	}
}

// connect runs one attempt. Its context is not any single caller's, because the
// result is shared; the opener applies its own bound.
func (s *Server) connect(attempt *connectAttempt) {
	orchestrator, st, err := s.open(context.Background())

	s.connMu.Lock()
	attempt.orchestrator, attempt.store, attempt.err = orchestrator, st, err
	if err == nil {
		s.orchestrator, s.store = orchestrator, st
	}
	// A failure is not remembered: the next call starts a new attempt.
	s.attempt = nil
	s.connMu.Unlock()

	close(attempt.done)
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
// same lifecycle as production, including how the run watchers are bound to the
// server's context and stopped on shutdown.
func (s *Server) ServeTransport(ctx context.Context, transport sdk.Transport) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.baseCtx = runCtx

	s.logger.InfoContext(ctx, "mcp server starting", "version", s.version)

	err := s.MCPServer().Run(ctx, transport)

	// Stop the watchers. The runs themselves are separate processes and keep
	// going: a session ending is not an interruption of the work, and a run
	// whose process actually died is what the lease notices (research C1/C2).
	cancel()
	s.awaitBackgroundRuns()

	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp server: %w", err)
	}
	s.logger.InfoContext(ctx, "mcp server stopped")
	return nil
}

// awaitBackgroundRuns waits for the run watchers to notice the shutdown. Each
// returns as soon as the context is cancelled, so this is normally instant; the
// grace only bounds a watcher that is mid-read when the client disconnects.
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

A successful task leaves a commit on its own branch, named in
result.worktree.branch: aidev/<ref>, or aidev/<ref>-aN when a retry succeeded
(set max_retries to let aidev retry an attempt whose checks failed or whose agent
stopped early). Nothing is merged, and your working tree is never touched. A
failed task leaves its worktree in place so the partial work can be inspected.`
