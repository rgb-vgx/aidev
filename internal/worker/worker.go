// Package worker drives a task through its lifecycle: isolate, delegate, verify,
// record.
//
// It is the only package that knows the order of those steps, and it owns every
// state transition and every event. The pieces it composes — git worktrees, an
// agent backend, the verification runner, the store — know nothing about each
// other.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/event"
	"aidev/internal/git"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/verification"
)

// persistTimeout bounds the writes that record an outcome. They run on a context
// detached from the caller's, so that a cancelled task still gets recorded rather
// than leaving a row stuck in RUNNING.
const persistTimeout = 30 * time.Second

// Orchestrator executes tasks.
//
// The store is a concrete type rather than an interface: there is one
// implementation, the integration tests use a real PostgreSQL because that is the
// only way to test persistence honestly, and an interface here would be
// speculation. The agent backend, where a second implementation is genuinely
// foreseen, is an interface.
type Orchestrator struct {
	Store    *store.Store
	Git      *git.Manager
	Backend  agent.Backend
	Verifier *verification.Runner
	Config   config.Config
	Logger   *slog.Logger

	// CancelPoll is how often a run reads its task's status to notice a Cancel
	// made by another process. Zero means the default.
	CancelPoll time.Duration

	// runs maps a task ID to the cancel func of the run currently executing
	// it on this process, so that Cancel stops a local run without waiting
	// for the status poll. It is created on first use so that the zero value
	// is usable; guard it with mu.
	mu   sync.Mutex
	runs map[uuid.UUID]context.CancelFunc
}

// New builds an Orchestrator, defaulting the logger and the verifier.
func New(st *store.Store, gm *git.Manager, backend agent.Backend, cfg config.Config, logger *slog.Logger) *Orchestrator {
	if logger == nil {
		logger = logging.Discard()
	}
	return &Orchestrator{
		Store:    st,
		Git:      gm,
		Backend:  backend,
		Verifier: verification.NewRunner(cfg.DefaultVerificationTimeout, cfg.VerificationTotalTimeout, cfg.MaxOutputBytes),
		Config:   cfg,
		Logger:   logger,
	}
}

// Outcome is what happened to a task, for a CLI or MCP caller to report.
type Outcome struct {
	Task    task.Task
	Attempt *task.TaskAttempt

	WorkerRun    *task.WorkerRun
	Verification *verification.Report
	Worktree     *task.Worktree

	// Approval is set when the run stopped at the approval gate.
	Approval *task.Approval

	// TestsModified lists the changed paths that look like the tests judging
	// the attempt (research §7b tier 1). It is a report, not a verdict: the
	// run is judged as usual. Empty when the attempt left the tests alone or
	// never reached verification.
	TestsModified []string

	// Message is a one-line human summary of the outcome.
	Message string
}

// Errors callers distinguish.
var (
	// ErrNotRunnable means the task's current state does not allow a run.
	ErrNotRunnable = errors.New("task is not runnable")

	// ErrApprovalRequired means the run stopped because policy requires a human
	// decision. The task is left in WAITING_APPROVAL; it is not a failure.
	ErrApprovalRequired = errors.New("task requires approval before it can run")

	// ErrProjectNotRegistered means the caller may only create tasks for
	// repositories aidev already knows, and this one is not among them
	// (research D3). Nothing was written.
	ErrProjectNotRegistered = errors.New("repository is not registered with aidev")
)

// RunTask takes a task from its current state to a terminal one.
//
// The sequence is: check policy, claim the task, create an isolated worktree, run
// the agent there, collect what changed, verify independently, then record the
// outcome. A failure at any step is persisted with its classification rather than
// returned as a bare error, because "what happened to this task" must be
// answerable from the database afterwards.
func (o *Orchestrator) RunTask(ctx context.Context, idOrRef string) (Outcome, error) {
	t, err := o.Store.ResolveTask(ctx, idOrRef)
	if err != nil {
		return Outcome{}, err
	}

	r := &run{
		o:    o,
		task: t,
		log: o.Logger.With(
			logging.FieldTaskID, t.ID.String(),
			logging.FieldTaskRef, t.Ref,
			logging.FieldProjectID, t.ProjectID.String(),
		),
	}
	return r.execute(ctx)
}

// Approve records a human decision and, when granted, returns the task to READY
// so that it can be run.
//
// via says which surface the decision came through ("cli" or "mcp") and is
// written into the approval event: the two have different trust levels — the
// CLI is the operator, MCP may be the planner that created the task — so the
// history has to keep them apart.
func (o *Orchestrator) Approve(ctx context.Context, idOrRef string, granted bool, decidedBy, reason, via string) (Outcome, error) {
	if via == "" {
		return Outcome{}, fmt.Errorf("approval decision needs a via (cli or mcp), because the history must say which surface decided")
	}
	t, err := o.Store.ResolveTask(ctx, idOrRef)
	if err != nil {
		return Outcome{}, err
	}
	if t.Status != task.StatusWaitingApproval {
		return Outcome{}, fmt.Errorf("%s is %s, not WAITING_APPROVAL: %w", t.Identifier(), t.Status, ErrNotRunnable)
	}

	decision := task.ApprovalDenied
	evType := event.TypeApprovalDenied
	next := task.StatusFailed
	if granted {
		decision = task.ApprovalGranted
		evType = event.TypeApprovalGranted
		next = task.StatusReady
	}

	var approval task.Approval
	err = o.Store.InTx(ctx, func(tx *store.Store) error {
		a, err := tx.DecideApproval(ctx, t.ID, decision, decidedBy, reason)
		if err != nil {
			return err
		}
		approval = a

		if err := tx.TransitionTask(ctx, t.ID, t.Status, next); err != nil {
			return err
		}
		if next == task.StatusFailed {
			if err := finishOpenAttempts(ctx, tx, t.ID, task.AttemptFailed, task.FailureApprovalDenied, "approval denied"); err != nil {
				return err
			}
		}
		return appendEvent(ctx, tx, t.ID, nil, evType, map[string]any{
			"decided_by": decidedBy,
			"reason":     reason,
			"via":        via,
		})
	})
	if err != nil {
		return Outcome{}, err
	}

	t.Status = next
	message := fmt.Sprintf("%s approved by %s; it is now READY to run", t.Identifier(), decidedBy)
	if !granted {
		message = fmt.Sprintf("%s denied by %s; it is now FAILED", t.Identifier(), decidedBy)
	}
	o.Logger.InfoContext(ctx, "approval decided",
		logging.FieldTaskRef, t.Ref, "granted", granted, "decided_by", decidedBy)

	return Outcome{Task: t, Approval: &approval, Message: message}, nil
}
