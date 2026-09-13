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
		Verifier: verification.NewRunner(cfg.DefaultVerificationTimeout, cfg.MaxOutputBytes),
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
func (o *Orchestrator) Approve(ctx context.Context, idOrRef string, granted bool, decidedBy, reason string) (Outcome, error) {
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

// Cancel stops a task that has not finished.
//
// A worktree belonging to a cancelled attempt is retained: work in progress may
// still be useful, and discarding it would be the one thing the cleanup policy
// promises never to do.
func (o *Orchestrator) Cancel(ctx context.Context, idOrRef, reason string) (Outcome, error) {
	t, err := o.Store.ResolveTask(ctx, idOrRef)
	if err != nil {
		return Outcome{}, err
	}
	if t.Status.Terminal() {
		return Outcome{}, fmt.Errorf("%s is already %s: %w", t.Identifier(), t.Status, ErrNotRunnable)
	}

	err = o.Store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.TransitionTask(ctx, t.ID, t.Status, task.StatusCancelled); err != nil {
			return err
		}
		if err := finishOpenAttempts(ctx, tx, t.ID, task.AttemptCancelled, task.FailureCancelled, reason); err != nil {
			return err
		}
		if err := retainWorktrees(ctx, tx, t.ID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, t.ID, nil, event.TypeTaskCancelled, map[string]any{
			"reason":            reason,
			"previous_status":   t.Status.String(),
			"worktree_retained": true,
		})
	})
	if err != nil {
		return Outcome{}, err
	}

	previous := t.Status
	t.Status = task.StatusCancelled
	o.Logger.InfoContext(ctx, "task cancelled",
		logging.FieldTaskRef, t.Ref, "previous_status", previous.String(), "reason", reason)

	return Outcome{
		Task:    t,
		Message: fmt.Sprintf("%s cancelled (was %s); any worktree was kept for inspection", t.Identifier(), previous),
	}, nil
}

// run carries the state of one execution through the lifecycle.
type run struct {
	o    *Orchestrator
	task task.Task
	log  *slog.Logger

	attempt  task.TaskAttempt
	worktree *git.Worktree
	record   *task.Worktree

	workerRun *task.WorkerRun
	report    *verification.Report
}

func (r *run) execute(ctx context.Context) (Outcome, error) {
	if r.task.Status.Terminal() {
		return Outcome{}, fmt.Errorf("%s is already %s: %w", r.task.Identifier(), r.task.Status, ErrNotRunnable)
	}
	if r.task.Status.Active() {
		// The MVP runs one attempt at a time. The compare-and-set on the status
		// is the real guard; this is the clear message.
		return Outcome{}, fmt.Errorf("%s is already %s: %w", r.task.Identifier(), r.task.Status, ErrNotRunnable)
	}

	if outcome, stopped, err := r.enforceApproval(ctx); stopped || err != nil {
		return outcome, err
	}
	if err := r.becomeReady(ctx); err != nil {
		return Outcome{}, err
	}
	if err := r.startAttempt(ctx); err != nil {
		return Outcome{}, err
	}

	r.log = r.log.With(
		logging.FieldAttemptID, r.attempt.ID.String(),
		logging.FieldAttemptNum, r.attempt.AttemptNumber,
	)
	r.log.InfoContext(ctx, "task started", logging.FieldBackend, r.o.Backend.Name())

	if err := r.prepareWorktree(ctx); err != nil {
		return r.fail(ctx, task.FailureWorktree, err)
	}
	if err := r.runAgent(ctx); err != nil {
		return r.fail(ctx, r.workerFailureKind(), err)
	}
	return r.verify(ctx)
}

// enforceApproval applies the approval policy. It is checked before anything is
// claimed or created, so a gated task leaves no worktree and no attempt behind.
func (r *run) enforceApproval(ctx context.Context) (Outcome, bool, error) {
	if !r.task.RequiresApproval {
		return Outcome{}, false, nil
	}

	latest, err := r.o.Store.LatestApproval(ctx, r.task.ID)
	switch {
	case err == nil && latest.Status == task.ApprovalGranted:
		// Already approved: carry on.
		return Outcome{}, false, nil
	case err == nil && latest.Status == task.ApprovalDenied:
		return Outcome{}, true, fmt.Errorf("%s was denied by %s: %w", r.task.Identifier(), latest.DecidedBy, ErrApprovalRequired)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return Outcome{}, true, err
	}

	// No decision yet: gate the task and open a request if there is not one.
	var approval task.Approval
	err = r.o.Store.InTx(ctx, func(tx *store.Store) error {
		if r.task.Status != task.StatusWaitingApproval {
			if err := tx.TransitionTask(ctx, r.task.ID, r.task.Status, task.StatusWaitingApproval); err != nil {
				return err
			}
		}
		a, reqErr := tx.RequestApproval(ctx, r.task.ID, "task is marked as requiring approval")
		switch {
		case reqErr == nil:
			approval = a
		case errors.Is(reqErr, store.ErrAlreadyExists):
			existing, getErr := tx.LatestApproval(ctx, r.task.ID)
			if getErr != nil {
				return getErr
			}
			approval = existing
		default:
			return reqErr
		}
		return appendEvent(ctx, tx, r.task.ID, nil, event.TypeApprovalRequired, map[string]any{
			"approval_id": approval.ID.String(),
		})
	})
	if err != nil {
		return Outcome{}, true, err
	}

	r.task.Status = task.StatusWaitingApproval
	r.log.InfoContext(ctx, "task gated pending approval", "approval_id", approval.ID.String())

	return Outcome{
		Task:     r.task,
		Approval: &approval,
		Message: fmt.Sprintf("%s requires approval and was not run; approve it to continue",
			r.task.Identifier()),
	}, true, fmt.Errorf("%s: %w", r.task.Identifier(), ErrApprovalRequired)
}

// becomeReady moves a task to READY if it is not there already. PENDING exists as
// "not yet eligible"; in the MVP nothing blocks a task, so this is where the
// future eligibility check will live.
func (r *run) becomeReady(ctx context.Context) error {
	if r.task.Status == task.StatusReady {
		return nil
	}
	if err := r.transition(ctx, task.StatusReady, event.TypeTaskReady, nil); err != nil {
		return err
	}
	return nil
}

// startAttempt claims the task and opens an attempt in one transaction, so a task
// can never be RUNNING without a record of which attempt is running it.
func (r *run) startAttempt(ctx context.Context) error {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	return r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := tx.TransitionTask(writeCtx, r.task.ID, r.task.Status, task.StatusRunning); err != nil {
			return err
		}
		number, err := tx.NextAttemptNumber(writeCtx, r.task.ID)
		if err != nil {
			return err
		}
		attempt, err := tx.CreateAttempt(writeCtx, task.NewAttempt(r.task.ID, number))
		if err != nil {
			return err
		}
		r.attempt = attempt
		r.task.Status = task.StatusRunning

		return appendEvent(writeCtx, tx, r.task.ID, &attempt.ID, event.TypeTaskStarted, map[string]any{
			"attempt": attempt.AttemptNumber,
			"backend": r.o.Backend.Name(),
			"agent":   r.task.Agent,
		})
	})
}

// prepareWorktree creates the isolated checkout the agent will run in.
func (r *run) prepareWorktree(ctx context.Context) error {
	project, err := r.o.Store.GetProject(ctx, r.task.ProjectID)
	if err != nil {
		return err
	}
	repo, err := r.o.Git.OpenRepository(ctx, project.RepoPath)
	if err != nil {
		return err
	}

	baseRef := r.task.BaseRef
	if baseRef == "" {
		baseRef = project.DefaultBranch
	}

	wt, err := r.o.Git.Create(ctx, git.CreateRequest{
		Repository: repo,
		Name:       worktreeName(r.task, r.attempt),
		Branch:     branchName(r.task, r.attempt),
		BaseRef:    baseRef,
	})
	if err != nil {
		return err
	}
	r.worktree = wt
	r.log = r.log.With(logging.FieldWorktreePath, wt.Path)

	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	return r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		recorded, err := tx.CreateWorktree(writeCtx, task.Worktree{
			ID:         uuid.Must(uuid.NewV7()),
			AttemptID:  r.attempt.ID,
			Path:       wt.Path,
			Branch:     wt.Branch,
			BaseCommit: wt.BaseCommit,
			Status:     task.WorktreeActive,
		})
		if err != nil {
			return err
		}
		r.record = &recorded

		return appendEvent(writeCtx, tx, r.task.ID, &r.attempt.ID, event.TypeWorktreeCreated, map[string]any{
			"path":        wt.Path,
			"branch":      wt.Branch,
			"base_commit": wt.BaseCommit,
		})
	})
}

// runAgent delegates the implementation and records what the agent did, including
// the diff aidev collected itself rather than the one the agent claimed.
func (r *run) runAgent(ctx context.Context) error {
	prompt, err := buildPrompt(r.task)
	if err != nil {
		return err
	}

	req := agent.Request{
		TaskRef:        r.task.Identifier(),
		Prompt:         prompt,
		WorkingDir:     r.worktree.Path,
		Agent:          r.task.Agent,
		Model:          r.o.Config.OpenCodeModel,
		Timeout:        r.task.EffectiveTimeout(r.o.Config.DefaultTaskTimeout),
		MaxOutputBytes: r.o.Config.MaxOutputBytes,
	}

	r.emit(ctx, event.TypeWorkerStarted, map[string]any{
		"backend":       r.o.Backend.Name(),
		"agent":         r.task.Agent,
		"working_dir":   r.worktree.Path,
		"timeout":       req.Timeout.String(),
		"prompt_length": len(prompt),
	})

	result, runErr := r.o.Backend.Run(ctx, req)

	// Persist what the agent did even when the run failed: the audit record is
	// most valuable precisely then.
	record := r.persistWorkerRun(ctx, result)
	r.workerRun = record

	r.emit(ctx, event.TypeWorkerCompleted, map[string]any{
		"status":        string(result.Status),
		"failure_kind":  string(result.FailureKind),
		"exit_code":     result.ExitCode,
		"session_id":    result.SessionID,
		"tool_calls":    result.ToolCalls,
		"changed_files": changedFiles(record),
		"duration_ms":   result.Duration.Milliseconds(),
	})

	r.log.InfoContext(ctx, "agent finished",
		logging.FieldBackend, r.o.Backend.Name(),
		"status", string(result.Status),
		logging.FieldFailureKind, string(result.FailureKind),
		logging.FieldExitCode, result.ExitCode,
		logging.FieldDurationMS, result.Duration.Milliseconds(),
		"tool_calls", result.ToolCalls)

	if runErr != nil {
		return runErr
	}
	if result.Status != task.WorkerSucceeded {
		if result.Err != nil {
			return result.Err
		}
		return fmt.Errorf("agent run %s", result.Status)
	}
	return nil
}

// persistWorkerRun records the agent invocation together with the diff aidev
// collected. A failure to collect the diff does not fail the task: the diff is
// evidence about the run, and losing it is not a reason to discard the run.
func (r *run) persistWorkerRun(ctx context.Context, result agent.Result) *task.WorkerRun {
	record := task.WorkerRun{
		ID:              uuid.Must(uuid.NewV7()),
		AttemptID:       r.attempt.ID,
		Backend:         result.Backend,
		Status:          result.Status,
		FailureKind:     result.FailureKind,
		Command:         result.Command,
		WorkingDir:      result.WorkingDir,
		ExitCode:        result.ExitCode,
		Stdout:          result.Stdout,
		StdoutTruncated: result.StdoutTruncated,
		Stderr:          result.Stderr,
		StderrTruncated: result.StderrTruncated,
		SessionID:       result.SessionID,
		Summary:         result.Summary,
		FinishReason:    result.FinishReason,
		Tokens:          result.Tokens,
		Cost:            result.Cost,
		StartedAt:       result.StartedAt,
	}
	if record.Backend == "" {
		record.Backend = r.o.Backend.Name()
	}
	if record.WorkingDir == "" {
		record.WorkingDir = r.worktree.Path
	}
	if record.StartedAt.IsZero() {
		record.StartedAt = time.Now().UTC()
	}
	if !result.FinishedAt.IsZero() {
		finished := result.FinishedAt
		record.FinishedAt = &finished
	}
	if result.Err != nil {
		record.Stderr = appendDetail(record.Stderr, result.Err.Error())
	}

	if diff, err := r.worktree.Diff(ctx); err != nil {
		r.log.WarnContext(ctx, "could not collect the worktree diff", "error", err.Error())
	} else {
		record.Diff = diff.Patch
		record.DiffTruncated = diff.Truncated
		record.ChangedFiles = diff.ChangedFiles
	}
	if head, err := r.worktree.HeadCommit(ctx); err == nil && r.record != nil {
		writeCtx, cancel := writeContext(ctx)
		defer cancel()
		if err := r.o.Store.SetWorktreeHead(writeCtx, r.record.ID, head); err != nil {
			r.log.WarnContext(ctx, "could not record the worktree head", "error", err.Error())
		}
	}

	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	stored, err := r.o.Store.CreateWorkerRun(writeCtx, record)
	if err != nil {
		// Losing this record is serious but not a reason to abandon the work in
		// the worktree; the task outcome is still recorded below.
		r.log.ErrorContext(ctx, "could not persist the worker run", "error", err.Error())
		return &record
	}
	return &stored
}

// verify runs the task's own commands and decides the outcome.
func (r *run) verify(ctx context.Context) (Outcome, error) {
	if err := r.transition(ctx, task.StatusVerifying, event.TypeVerificationStarted, map[string]any{
		"steps": len(r.task.Verification),
	}); err != nil {
		return Outcome{}, err
	}

	report, err := r.o.Verifier.Run(ctx, verification.Request{
		AttemptID:  r.attempt.ID,
		WorkingDir: r.worktree.Path,
		Steps:      r.task.Verification,
	})
	if err != nil {
		return r.fail(ctx, task.FailureInternal, err)
	}
	r.report = &report

	r.persistVerification(ctx, report)

	r.emit(ctx, event.TypeVerificationCompleted, map[string]any{
		"passed":       report.Passed,
		"failure_kind": string(report.FailureKind),
		"summary":      report.Summary(),
		"duration_ms":  report.Duration.Milliseconds(),
	})
	r.log.InfoContext(ctx, "verification finished",
		"passed", report.Passed,
		logging.FieldFailureKind, string(report.FailureKind),
		logging.FieldDurationMS, report.Duration.Milliseconds())

	if !report.Passed {
		return r.fail(ctx, report.FailureKind, fmt.Errorf("verification did not pass: %s", report.Summary()))
	}
	return r.succeed(ctx)
}

func (r *run) persistVerification(ctx context.Context, report verification.Report) {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	for _, vr := range report.Runs {
		if _, err := r.o.Store.CreateVerificationRun(writeCtx, vr); err != nil {
			r.log.ErrorContext(ctx, "could not persist a verification result",
				"step", vr.StepIndex, "error", err.Error())
			continue
		}
		r.emit(ctx, event.TypeVerificationStepRan, map[string]any{
			"step":      vr.StepIndex,
			"command":   vr.Command,
			"status":    string(vr.Status),
			"exit_code": vr.ExitCode,
		})
	}
}

// succeed commits the work, cleans up per policy, and marks the task succeeded.
func (r *run) succeed(ctx context.Context) (Outcome, error) {
	r.cleanupAfterSuccess(ctx)

	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	err := r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := tx.TransitionTask(writeCtx, r.task.ID, task.StatusVerifying, task.StatusSucceeded); err != nil {
			return err
		}
		if err := tx.FinishAttempt(writeCtx, r.attempt.ID, task.AttemptSucceeded, task.FailureNone, ""); err != nil {
			return err
		}
		return appendEvent(writeCtx, tx, r.task.ID, &r.attempt.ID, event.TypeTaskSucceeded, map[string]any{
			"attempt":       r.attempt.AttemptNumber,
			"branch":        r.worktree.Branch,
			"head_commit":   r.headCommit(),
			"changed_files": changedFiles(r.workerRun),
			"verification":  r.report.Summary(),
		})
	})
	if err != nil {
		return Outcome{}, err
	}

	r.task.Status = task.StatusSucceeded
	r.log.InfoContext(ctx, "task succeeded",
		"branch", r.worktree.Branch, "head_commit", r.headCommit())

	return r.outcome(fmt.Sprintf("%s succeeded: %s, work committed on %s",
		r.task.Identifier(), r.report.Summary(), r.worktree.Branch)), nil
}

// cleanupAfterSuccess commits the agent's work to the task branch and removes the
// worktree when the policy asks for it.
//
// Committing first is what makes removal safe: the work is the deliverable, and
// deleting an uncommitted worktree would destroy it. Nothing is merged — only the
// task's own branch is written.
func (r *run) cleanupAfterSuccess(ctx context.Context) {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	commit, err := r.worktree.Commit(writeCtx, fmt.Sprintf("%s %s", r.task.Identifier(), r.task.Title))
	if err != nil {
		r.log.ErrorContext(ctx, "could not commit the worktree; keeping it for inspection", "error", err.Error())
		r.retainWorktree(ctx, "commit failed")
		return
	}
	if commit != "" {
		if r.record != nil {
			if err := r.o.Store.SetWorktreeHead(writeCtx, r.record.ID, commit); err != nil {
				r.log.WarnContext(ctx, "could not record the commit", "error", err.Error())
			}
		}
		r.setHeadCommit(commit)
	}

	if r.o.Config.WorktreeCleanup != config.CleanupOnSuccess {
		r.log.InfoContext(ctx, "keeping the worktree", "policy", r.o.Config.WorktreeCleanup.String())
		return
	}

	if err := r.o.Git.Remove(writeCtx, r.worktree, false); err != nil {
		// Removal without --force can only fail if something is still
		// uncommitted, which means keeping it is the right answer.
		r.log.WarnContext(ctx, "could not remove the worktree; keeping it", "error", err.Error())
		r.retainWorktree(ctx, err.Error())
		return
	}

	if r.record != nil {
		if err := r.o.Store.SetWorktreeStatus(writeCtx, r.record.ID, task.WorktreeRemoved); err != nil {
			r.log.WarnContext(ctx, "could not record the worktree removal", "error", err.Error())
		} else {
			r.record.Status = task.WorktreeRemoved
		}
	}
	r.emit(ctx, event.TypeWorktreeRemoved, map[string]any{
		"path":   r.worktree.Path,
		"branch": r.worktree.Branch,
		"commit": r.headCommit(),
	})
}

// fail records a terminal failure, or a cancellation when that is what happened.
func (r *run) fail(ctx context.Context, kind task.FailureKind, cause error) (Outcome, error) {
	cancelled := kind == task.FailureCancelled || errors.Is(ctx.Err(), context.Canceled)

	next := task.StatusFailed
	attemptStatus := task.AttemptFailed
	evType := event.TypeTaskFailed
	if cancelled {
		next = task.StatusCancelled
		attemptStatus = task.AttemptCancelled
		evType = event.TypeTaskCancelled
		kind = task.FailureCancelled
	}

	message := ""
	if cause != nil {
		message = cause.Error()
	}

	// A worktree is only retained when one was created; a task that failed
	// before that has nothing to keep.
	if r.worktree != nil {
		r.retainWorktree(ctx, message)
	}

	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	err := r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := tx.TransitionTask(writeCtx, r.task.ID, r.task.Status, next); err != nil {
			return err
		}
		if err := tx.FinishAttempt(writeCtx, r.attempt.ID, attemptStatus, kind, message); err != nil {
			return err
		}
		payload := map[string]any{
			"attempt":      r.attempt.AttemptNumber,
			"failure_kind": string(kind),
			"error":        message,
		}
		if r.worktree != nil {
			payload["worktree_retained"] = r.worktree.Path
			payload["branch"] = r.worktree.Branch
		}
		if r.report != nil {
			payload["verification"] = r.report.Summary()
		}
		return appendEvent(writeCtx, tx, r.task.ID, &r.attempt.ID, evType, payload)
	})
	if err != nil {
		return Outcome{}, err
	}

	r.task.Status = next
	r.log.InfoContext(ctx, "task finished unsuccessfully",
		"status", next.String(), logging.FieldFailureKind, string(kind), "error", message)

	summary := fmt.Sprintf("%s %s (%s): %s", r.task.Identifier(), next, kind, message)
	if r.worktree != nil {
		summary += fmt.Sprintf("; the worktree was kept at %s", r.worktree.Path)
	}
	// A recorded failure is the expected outcome of a task that did not work, so
	// it is reported as an Outcome rather than as an error from RunTask.
	return r.outcome(summary), nil
}

// retainWorktree marks the worktree as deliberately kept.
func (r *run) retainWorktree(ctx context.Context, reason string) {
	if r.record == nil {
		return
	}
	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	if err := r.o.Store.SetWorktreeStatus(writeCtx, r.record.ID, task.WorktreeRetained); err != nil {
		r.log.WarnContext(ctx, "could not mark the worktree retained", "error", err.Error())
		return
	}
	r.record.Status = task.WorktreeRetained
	r.emit(ctx, event.TypeWorktreeRetained, map[string]any{
		"path":   r.record.Path,
		"branch": r.record.Branch,
		"reason": reason,
	})
}

// transition applies a state change and its event atomically, so history can
// never disagree with state.
func (r *run) transition(ctx context.Context, next task.Status, evType event.Type, payload any) error {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	var attemptID *uuid.UUID
	if r.attempt.ID != uuid.Nil {
		attemptID = &r.attempt.ID
	}

	current := r.task.Status
	if err := r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := tx.TransitionTask(writeCtx, r.task.ID, current, next); err != nil {
			return err
		}
		return appendEvent(writeCtx, tx, r.task.ID, attemptID, evType, payload)
	}); err != nil {
		return err
	}
	r.task.Status = next
	return nil
}

// emit appends an informational event.
//
// A failure here is logged rather than propagated: these events describe progress
// rather than state, and abandoning a task because a progress note could not be
// written would trade a real result for a bookkeeping gap. Events that must agree
// with state are written inside the transition transaction instead.
func (r *run) emit(ctx context.Context, evType event.Type, payload any) {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	var attemptID *uuid.UUID
	if r.attempt.ID != uuid.Nil {
		attemptID = &r.attempt.ID
	}
	if err := appendEvent(writeCtx, r.o.Store, r.task.ID, attemptID, evType, payload); err != nil {
		r.log.ErrorContext(ctx, "could not append an event", "event", evType.String(), "error", err.Error())
	}
}

func (r *run) outcome(message string) Outcome {
	attempt := r.attempt
	out := Outcome{
		Task:         r.task,
		Attempt:      &attempt,
		WorkerRun:    r.workerRun,
		Verification: r.report,
		Worktree:     r.record,
		Message:      message,
	}
	return out
}

func (r *run) headCommit() string {
	if r.record == nil {
		return ""
	}
	return r.record.HeadCommit
}

func (r *run) setHeadCommit(commit string) {
	if r.record != nil {
		r.record.HeadCommit = commit
	}
}

// workerFailureKind reports how the agent failed, falling back to UNKNOWN rather
// than guessing when the backend did not say.
func (r *run) workerFailureKind() task.FailureKind {
	if r.workerRun != nil && r.workerRun.FailureKind != task.FailureNone {
		return r.workerRun.FailureKind
	}
	return task.FailureUnknown
}

// writeContext detaches a context for the writes that record an outcome.
//
// Without this, cancelling a task would cancel the very writes that record the
// cancellation, leaving a row in RUNNING forever.
func writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
}

func appendEvent(ctx context.Context, st *store.Store, taskID uuid.UUID, attemptID *uuid.UUID, evType event.Type, payload any) error {
	e, err := event.New(taskID, attemptID, evType, payload)
	if err != nil {
		return err
	}
	_, err = st.AppendEvent(ctx, e)
	return err
}

// finishOpenAttempts closes any attempt still marked running, so that cancelling
// or denying a task cannot leave an attempt open forever.
func finishOpenAttempts(ctx context.Context, tx *store.Store, taskID uuid.UUID, status task.AttemptStatus, kind task.FailureKind, message string) error {
	attempts, err := tx.ListAttempts(ctx, taskID)
	if err != nil {
		return err
	}
	for _, a := range attempts {
		if a.Status != task.AttemptRunning {
			continue
		}
		if err := tx.FinishAttempt(ctx, a.ID, status, kind, message); err != nil {
			return err
		}
	}
	return nil
}

// retainWorktrees marks every active worktree of a task as retained.
func retainWorktrees(ctx context.Context, tx *store.Store, taskID uuid.UUID) error {
	attempts, err := tx.ListAttempts(ctx, taskID)
	if err != nil {
		return err
	}
	for _, a := range attempts {
		wt, err := tx.GetWorktreeByAttempt(ctx, a.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if wt.Status != task.WorktreeActive {
			continue
		}
		if err := tx.SetWorktreeStatus(ctx, wt.ID, task.WorktreeRetained); err != nil {
			return err
		}
	}
	return nil
}

// worktreeName is the directory under the workspace root. The attempt number is
// included so that a future retry does not collide with the attempt before it.
func worktreeName(t task.Task, attempt task.TaskAttempt) string {
	return fmt.Sprintf("%s-a%d", t.Identifier(), attempt.AttemptNumber)
}

// branchName keeps the common case readable: the first attempt gets a clean
// aidev/<ref> branch, and only a retry needs a suffix.
func branchName(t task.Task, attempt task.TaskAttempt) string {
	if attempt.AttemptNumber <= 1 {
		return "aidev/" + t.Identifier()
	}
	return fmt.Sprintf("aidev/%s-a%d", t.Identifier(), attempt.AttemptNumber)
}

func changedFiles(run *task.WorkerRun) int {
	if run == nil {
		return 0
	}
	return run.ChangedFiles
}

func appendDetail(existing, detail string) string {
	if detail == "" {
		return existing
	}
	if existing == "" {
		return detail
	}
	return existing + "\n" + detail
}
