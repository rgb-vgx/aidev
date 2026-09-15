// Package agent defines the boundary between aidev's orchestration and whichever
// coding agent actually does the work.
//
// Nothing above this package knows that OpenCode exists. That is the point: the
// orchestration layer depends on Backend, and the decision about how to invoke a
// particular agent — its flags, its output format, its failure modes — is
// confined to one implementation.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"aidev/internal/task"
)

// Request is what aidev asks an agent to do.
type Request struct {
	// TaskRef is the human-facing task identifier, for logs and for the agent's
	// own session title. It is not interpreted.
	TaskRef string

	// Prompt is the complete instruction for the agent.
	Prompt string

	// WorkingDir is the isolated worktree. It is required, and a backend must
	// run the agent there and nowhere else: it is the only thing standing
	// between the agent and the user's real working tree.
	WorkingDir string

	// Agent selects the agent persona or profile, when the backend has them.
	Agent string

	// Model selects the model. Empty means "let the backend decide", which
	// OpenCode supports (docs/research.md §2.10).
	Model string

	// Timeout bounds the run. Required.
	Timeout time.Duration

	// MaxOutputBytes bounds each captured stream. Required.
	MaxOutputBytes int

	// SessionID asks the backend to continue an existing session.
	//
	// Recorded and plumbed through but never set by the MVP: Phase 0 confirmed
	// OpenCode can resume a session by id (docs/research.md §2.12), which is what
	// retry-with-context will need. Having the field now means retry does not
	// require touching this interface.
	SessionID string
}

// Validate checks a request before a process is launched.
func (r Request) Validate() error {
	var problems []string
	if strings.TrimSpace(r.Prompt) == "" {
		problems = append(problems, "prompt is empty")
	}
	switch {
	case strings.TrimSpace(r.WorkingDir) == "":
		problems = append(problems, "working directory is required")
	case !filepath.IsAbs(r.WorkingDir):
		problems = append(problems, fmt.Sprintf("working directory %q must be absolute", r.WorkingDir))
	}
	if r.Timeout <= 0 {
		problems = append(problems, "timeout must be positive")
	}
	if r.MaxOutputBytes <= 0 {
		problems = append(problems, "max output bytes must be positive")
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid agent request: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Result is what the backend observed. It is deliberately close to
// task.WorkerRun: this is the audit record of an agent invocation, and a lossy
// translation in between would be a place for detail to disappear.
type Result struct {
	// Backend names the implementation that produced this result.
	Backend string

	Status      task.WorkerRunStatus
	FailureKind task.FailureKind

	// Command is the rendered argv, safe to persist: it holds no environment.
	Command    string
	WorkingDir string

	// ExitCode is nil when the agent produced none, for example when it could
	// not be started.
	ExitCode *int

	Stdout          string
	StdoutTruncated bool
	Stderr          string
	StderrTruncated bool

	// SessionID is the agent's own session identifier, when it exposes one.
	SessionID string

	// Summary is the agent's closing message, when it produced one. It is the
	// agent's account of its work and is never treated as evidence.
	Summary string

	// FinishReason is the backend's own completion reason, passed through
	// without interpretation so that a value aidev does not recognise is still
	// recorded.
	FinishReason string

	// Model is the model that actually ran: the request's when it names one,
	// otherwise the backend's own configured model. Only the backend knows.
	Model string

	// Tokens is usage as reported by the backend, kept as raw JSON so aidev does
	// not have to model every backend's accounting.
	Tokens json.RawMessage

	Cost *float64

	// ToolCalls counts the tool invocations observed, which is a cheap signal
	// for "did the agent actually try to do anything".
	ToolCalls int

	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration

	// Err carries the underlying failure. Status and FailureKind are the
	// decision; this is the detail.
	Err error
}

// Succeeded reports whether the agent ran to completion without an error the
// backend could detect.
//
// It says nothing about whether the work is correct. Only verification decides
// that, and the task lifecycle enforces it: there is no transition from RUNNING
// to SUCCEEDED.
func (r Result) Succeeded() bool { return r.Status == task.WorkerSucceeded }

// Backend runs a coding agent.
//
// Implementations must honour ctx cancellation, enforce Request.Timeout, capture
// both streams within Request.MaxOutputBytes, and run the agent with
// Request.WorkingDir as its working directory.
//
// A returned error means aidev could not conduct the run at all — an invalid
// request, a missing executable. An agent that ran and failed is reported through
// Result.Status, because that is an outcome aidev expects and records rather than
// an error in aidev itself.
type Backend interface {
	// Name identifies the backend in audit records, for example "opencode".
	Name() string

	// Run executes the request.
	Run(ctx context.Context, req Request) (Result, error)
}

// Validator is implemented by backends that can check a configuration value
// before a task is created, rather than letting it fail at run time.
//
// It exists because of a specific measured hazard: OpenCode accepts an unknown
// agent name, prints a warning, silently falls back to its default, and exits 0
// (docs/research.md §2.5). A task would appear to have run with the requested
// agent when it had not, so aidev checks the name itself.
type Validator interface {
	// ValidateAgentName reports whether the backend recognises the agent name.
	ValidateAgentName(ctx context.Context, name string) error
}
