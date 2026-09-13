// Package logging builds aidev's structured logger.
//
// Everything is written to stderr as JSON, without exception. Phase 0
// established that Claude Code speaks MCP to aidev over stdin/stdout
// (docs/research.md §3.3), so stdout belongs to the JSON-RPC stream: a single
// stray log line there corrupts the protocol. Keeping stderr as the only log
// sink removes the possibility rather than relying on remembering it.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// Field names used for correlation across a task execution. They are constants
// so that a query over the logs can rely on exact spellings.
const (
	FieldTaskID       = "task_id"
	FieldTaskRef      = "task_ref"
	FieldAttemptID    = "attempt_id"
	FieldAttemptNum   = "attempt"
	FieldProjectID    = "project_id"
	FieldWorkerRunID  = "worker_run_id"
	FieldWorktreePath = "worktree_path"
	FieldBackend      = "backend"
	FieldExitCode     = "exit_code"
	FieldDurationMS   = "duration_ms"
	FieldFailureKind  = "failure_kind"
)

// There is deliberately no separate correlation id. A task execution is already
// identified by task_id and attempt_id, which are the values a reader searches on
// and which tie a log line to a row in the database; a third identifier would be
// one more thing to keep consistent and nothing to gain.

// New returns a JSON logger writing to stderr at the given level.
func New(level slog.Level) *slog.Logger {
	return NewTo(os.Stderr, level)
}

// NewTo returns a JSON logger writing to w. Tests use it to assert on output;
// production code should call New.
func NewTo(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// Discard returns a logger that drops everything, for tests that do not assert
// on log output.
func Discard() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type contextKey struct{}

// Into stores a logger in ctx so that deep call paths can pick up correlation
// fields without threading a logger through every signature.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, l)
}

// From retrieves the logger stored by Into, or a discarding logger when none is
// present. It never returns nil.
func From(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return Discard()
}
