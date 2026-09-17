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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

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

	// failureKind records how the run failed for the trace. It stays
	// FailureNone when the run succeeded or has not failed yet.
	failureKind task.FailureKind
}

func (r *run) execute(ctx context.Context) (outcome Outcome, retErr error) {
	if r.task.Status.Terminal() {
		return Outcome{}, fmt.Errorf("%s is already %s: %w", r.task.Identifier(), r.task.Status, ErrNotRunnable)
	}
	if r.task.Status.Active() {
		// The MVP runs one attempt at a time. The compare-and-set on the status
		// is the real guard; this is the clear message.
		return Outcome{}, fmt.Errorf("%s is already %s: %w", r.task.Identifier(), r.task.Status, ErrNotRunnable)
	}

	// One trace per run, joined via the context: child spans started from this
	// context share its trace. With no provider configured the global tracer
	// is a no-op, so the run behaves exactly as before.
	ctx, root := otel.Tracer("aidev").Start(ctx, "aidev.task.run")
	defer func() {
		r.finishRootSpan(root, retErr, outcome)
	}()
	root.SetAttributes(
		attribute.String("aidev.task.ref", r.task.Ref),
		attribute.String("aidev.task.id", r.task.ID.String()),
		attribute.String("aidev.project.id", r.task.ProjectID.String()),
	)

	if out, stopped, err := r.enforceApproval(ctx); stopped || err != nil {
		outcome, retErr = out, err
		// The gated task never got an attempt, so there is no failure
		// classification to record; the span still ends via the deferred
		// finalizer with the waiting status and an error status.
		return outcome, retErr
	}
	if err := r.becomeReady(ctx); err != nil {
		retErr = err
		return Outcome{}, retErr
	}
	if err := r.startAttempt(ctx); err != nil {
		retErr = err
		return Outcome{}, retErr
	}
	root.SetAttributes(attribute.Int("aidev.attempt.number", r.attempt.AttemptNumber))

	r.log = r.log.With(
		logging.FieldAttemptID, r.attempt.ID.String(),
		logging.FieldAttemptNum, r.attempt.AttemptNumber,
	)
	r.log.InfoContext(ctx, "task started", logging.FieldBackend, r.o.Backend.Name())

	if err := r.prepareWorktree(ctx); err != nil {
		outcome, retErr = r.fail(ctx, task.FailureWorktree, err)
		return outcome, retErr
	}
	if err := r.runAgent(ctx); err != nil {
		outcome, retErr = r.fail(ctx, r.workerFailureKind(), err)
		return outcome, retErr
	}
	outcome, retErr = r.verify(ctx)
	return outcome, retErr
}

// finishRootSpan records the final status on the run span. It runs deferred,
// so every path through execute — success, recorded failure, early error,
// cancellation — ends the span with the outcome visible in the trace.
func (r *run) finishRootSpan(root trace.Span, retErr error, outcome Outcome) {
	if r.attempt.AttemptNumber != 0 {
		root.SetAttributes(attribute.Int("aidev.attempt.number", r.attempt.AttemptNumber))
	}
	status := r.task.Status
	if outcome.Task.Status.Valid() && outcome.Task.Status.String() != "" {
		// A recorded outcome carries the authoritative final status.
		status = outcome.Task.Status
	}
	root.SetAttributes(attribute.String("aidev.task.status", string(status)))
	if r.failureKind != task.FailureNone && r.failureKind != "" {
		root.SetAttributes(attribute.String("aidev.failure_kind", string(r.failureKind)))
	}
	if status != task.StatusSucceeded {
		root.SetStatus(codes.Error, rootErrorDescription(status, r.failureKind, retErr))
	}
	root.End()
}

// rootErrorDescription keeps the span status readable: a backend error view
// shows this string, not a full log, so a recorded failure reports only its
// classification while an early error reports the error itself.
func rootErrorDescription(status task.Status, kind task.FailureKind, retErr error) string {
	if retErr != nil {
		return truncateForSpan(retErr.Error())
	}
	if kind != task.FailureNone && kind != "" {
		return fmt.Sprintf("task %s (%s)", status, kind)
	}
	return fmt.Sprintf("task %s", status)
}

// truncateForSpan bounds the error description so a verbose failure cannot
// bloat the span.
func truncateForSpan(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// tokenUsage mirrors the integer fields a backend reports in WorkerRun.Tokens.
// Pointers distinguish "absent" from zero, so only present values become
// attributes.
type tokenUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

// usageAttributes parses the raw token JSON without ever failing the task:
// tracing is observability, not control flow.
func usageAttributes(tokens []byte) []attribute.KeyValue {
	if len(tokens) == 0 {
		return nil
	}
	var usage tokenUsage
	if err := json.Unmarshal(tokens, &usage); err != nil {
		return nil
	}
	var attrs []attribute.KeyValue
	if usage.InputTokens != nil {
		attrs = append(attrs, attribute.Int("gen_ai.usage.input_tokens", *usage.InputTokens))
	}
	if usage.OutputTokens != nil {
		attrs = append(attrs, attribute.Int("gen_ai.usage.output_tokens", *usage.OutputTokens))
	}
	return attrs
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
	ctx, span := otel.Tracer("aidev").Start(ctx, "aidev.worktree.create")
	defer span.End()

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
	// The path and branch identify the checkout in the trace without
	// reading the database.
	span.SetAttributes(
		attribute.String("aidev.worktree.path", wt.Path),
		attribute.String("aidev.worktree.branch", wt.Branch),
	)
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
	ctx, span := otel.Tracer("aidev").Start(ctx, "aidev.agent.run")
	defer span.End()
	// Rendered as an LLM call by backends, hence the generation marker.
	span.SetAttributes(
		attribute.String("aidev.agent.backend", r.o.Backend.Name()),
		attribute.String("langfuse.observation.type", "generation"),
	)

	prompt, err := buildPrompt(r.task)
	if err != nil {
		return err
	}

	req := agent.Request{
		TaskRef:        r.task.Identifier(),
		Prompt:         prompt,
		WorkingDir:     r.worktree.Path,
		Agent:          r.task.Agent,
		Model:          resolveModel(r.task, r.o.Config.Routing),
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

	r.setAgentUsageSpan(span, record)

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
		logging.FieldWorkerRunID, workerRunID(record),
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

// resolveModel picks the model for a run: the task's own model first (a
// person overriding the policy), then the routing table for the task's
// hardness, then nothing at all, which leaves the choice to the backend.
func resolveModel(t task.Task, routing map[string]string) string {
	if t.Model != "" {
		return t.Model
	}
	if model := routing[string(t.Hardness)]; model != "" {
		return model
	}
	return ""
}

// setAgentUsageSpan records what the backend reported about the invocation.
// Usage is observability, so a missing or unparseable value only means the
// attribute is omitted, never a task failure.
func (r *run) setAgentUsageSpan(span trace.Span, record *task.WorkerRun) {
	backend := r.o.Backend.Name()
	sessionID := ""
	var tokens []byte
	var cost *float64
	if record != nil {
		if record.Backend != "" {
			backend = record.Backend
		}
		sessionID = record.SessionID
		tokens = record.Tokens
		cost = record.Cost
	}
	attrs := []attribute.KeyValue{
		attribute.String("aidev.agent.backend", backend),
	}
	if sessionID != "" {
		attrs = append(attrs, attribute.String("aidev.agent.session_id", sessionID))
	}
	attrs = append(attrs, usageAttributes(tokens)...)
	if cost != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.usage.cost", *cost))
	}
	span.SetAttributes(attrs...)
}

// persistWorkerRun records the agent invocation together with the diff aidev
// collected. A failure to collect the diff does not fail the task: the diff is
// evidence about the run, and losing it is not a reason to discard the run.
func (r *run) persistWorkerRun(ctx context.Context, result agent.Result) *task.WorkerRun {
	record := task.WorkerRun{
		ID:              uuid.Must(uuid.NewV7()),
		AttemptID:       r.attempt.ID,
		Backend:         result.Backend,
		Model:           result.Model,
		Agent:           r.task.Agent,
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
	ctx, span := otel.Tracer("aidev").Start(ctx, "aidev.verification")
	defer span.End()

	if err := r.transition(ctx, task.StatusVerifying, event.TypeVerificationStarted, map[string]any{
		"steps": len(r.task.Verification),
	}); err != nil {
		return Outcome{}, err
	}

	// A runner the agent changed is not independent evidence, so verification
	// does not run at all when one is found.
	changed, err := r.worktree.ChangedPaths(ctx)
	if err != nil {
		span.SetAttributes(attribute.Bool("aidev.verification.passed", false))
		return r.fail(ctx, task.FailureInternal,
			fmt.Errorf("verification not run: cannot establish independence: %w", err))
	}
	if intercepted := verification.Interceptions(r.task.Verification, changed); len(intercepted) > 0 {
		span.SetAttributes(attribute.Bool("aidev.verification.passed", false))
		r.emit(ctx, event.TypeVerificationIntercepted, map[string]any{
			"interceptions": interceptionPayload(intercepted),
		})
		return r.fail(ctx, task.FailureVerification, interceptionError(intercepted))
	}

	report, err := r.o.Verifier.Run(ctx, verification.Request{
		AttemptID:  r.attempt.ID,
		WorkingDir: r.worktree.Path,
		Steps:      r.task.Verification,
	})
	if err != nil {
		// The pass never ran, so it did not pass.
		span.SetAttributes(attribute.Bool("aidev.verification.passed", false))
		return r.fail(ctx, task.FailureInternal, err)
	}
	r.report = &report
	r.traceVerificationSteps(ctx, report)
	span.SetAttributes(attribute.Bool("aidev.verification.passed", report.Passed))

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

// interceptionPayload lists what was replaced for the audit log, so a reviewer
// can see which runner each step would have loaded.
func interceptionPayload(ins []verification.Interception) []map[string]any {
	out := make([]map[string]any, 0, len(ins))
	for _, in := range ins {
		out = append(out, map[string]any{
			"step_index": in.StepIndex,
			"step":       in.Step,
			"path":       in.Path,
		})
	}
	return out
}

// interceptionError names every step and path, so the failure reason tells a
// reviewer what was replaced without opening the audit log. Steps are numbered
// from 1 here because a person reads this; the event payload keeps the stored
// index, which matches verification_runs.step_index.
func interceptionError(ins []verification.Interception) error {
	parts := make([]string, 0, len(ins))
	for _, in := range ins {
		parts = append(parts, fmt.Sprintf("step %d `%s` would run %s, which changed during this attempt",
			in.StepIndex+1, in.Step, in.Path))
	}
	return fmt.Errorf("verification not run: %s", strings.Join(parts, "; "))
}

// traceVerificationSteps emits one span per declared step, including steps that
// were skipped, so a reader can account for every step from the trace alone.
func (r *run) traceVerificationSteps(ctx context.Context, report verification.Report) {
	tracer := otel.Tracer("aidev")
	for _, vr := range report.Runs {
		_, stepSpan := tracer.Start(ctx, "aidev.verification.step")
		attrs := []attribute.KeyValue{
			attribute.String("aidev.verification.command", vr.Command),
			attribute.String("aidev.verification.status", string(vr.Status)),
		}
		if vr.ExitCode != nil {
			attrs = append(attrs, attribute.Int("aidev.verification.exit_code", *vr.ExitCode))
		}
		stepSpan.SetAttributes(attrs...)
		stepSpan.End()
	}
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

// succeed delivers the work, records success, and cleans up per policy, in that
// order. The commit comes first so that a failure to deliver is a failure of
// the task, not a success with a warning; the removal comes last so that a
// task that is no longer VERIFYING — cancelled while verification ran — keeps
// its worktree.
func (r *run) succeed(ctx context.Context) (Outcome, error) {
	committed, err := r.commitWork(ctx)
	if err != nil {
		return r.fail(ctx, task.FailureWorktree, fmt.Errorf("commit failed: %w", err))
	}

	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	err = r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
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
		// Someone else finished the task first — Cancel only changes the
		// database — so the worktree stays where it is and the outcome
		// reports what the task actually is now, not a success.
		reloadCtx, reloadCancel := writeContext(ctx)
		defer reloadCancel()
		current, reloadErr := r.o.Store.GetTask(reloadCtx, r.task.ID)
		if reloadErr != nil || !current.Status.Terminal() {
			// Still VERIFYING, or unreadable: the write itself failed, and
			// that is an error. The commit and the worktree are both kept.
			return Outcome{}, err
		}
		// A recorded ending is an outcome, as it is in fail.
		r.task = current
		if wt, wtErr := r.o.Store.GetWorktreeByAttempt(reloadCtx, r.attempt.ID); wtErr == nil {
			r.record = &wt
		}
		r.log.InfoContext(ctx, "task ended elsewhere while verification ran",
			"status", current.Status.String())
		return r.outcome(fmt.Sprintf("%s is %s, not SUCCEEDED: it ended while verification ran; the worktree was kept at %s",
			r.task.Identifier(), r.task.Status, r.worktree.Path)), nil
	}

	r.task.Status = task.StatusSucceeded
	r.cleanupAfterSuccess(ctx)
	r.log.InfoContext(ctx, "task succeeded",
		"branch", r.worktree.Branch, "head_commit", r.headCommit())

	if committed {
		return r.outcome(fmt.Sprintf("%s succeeded: %s, work committed on %s",
			r.task.Identifier(), r.report.Summary(), r.worktree.Branch)), nil
	}
	return r.outcome(fmt.Sprintf("%s succeeded: %s; no changes to commit",
		r.task.Identifier(), r.report.Summary())), nil
}

// commitWork records the agent's work on the task branch and returns whether a
// commit was made. An empty commit means the agent changed nothing, which is
// still a success once verification passed. Nothing is merged — only the
// task's own branch is written.
func (r *run) commitWork(ctx context.Context) (bool, error) {
	// Detached from the caller's context so a cancelled task still delivers,
	// and bounded by git's own timeout rather than the shorter write budget
	// so a slow commit is not cut short by aidev.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), git.DefaultTimeout)
	defer cancel()

	commit, err := r.worktree.Commit(commitCtx, fmt.Sprintf("%s %s", r.task.Identifier(), r.task.Title))
	if err != nil {
		return false, err
	}
	if commit == "" {
		return false, nil
	}
	writeCtx, writeCancel := writeContext(ctx)
	defer writeCancel()
	if r.record != nil {
		if err := r.o.Store.SetWorktreeHead(writeCtx, r.record.ID, commit); err != nil {
			r.log.WarnContext(ctx, "could not record the commit", "error", err.Error())
		}
	}
	r.setHeadCommit(commit)
	return true, nil
}

// cleanupAfterSuccess removes the worktree when the policy asks for it. It runs
// only after SUCCEEDED is recorded: the commit is the deliverable, and deleting
// the worktree before the task owns its outcome would destroy work a cancelled
// task promised to keep.
func (r *run) cleanupAfterSuccess(ctx context.Context) {
	if r.o.Config.WorktreeCleanup != config.CleanupOnSuccess {
		r.log.InfoContext(ctx, "keeping the worktree", "policy", r.o.Config.WorktreeCleanup.String())
		return
	}

	writeCtx, cancel := writeContext(ctx)
	defer cancel()

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
	// Remembered for the root span, which is finalized when execute returns.
	r.failureKind = kind

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

// workerRunID names the audit record of this agent invocation, so a log line can
// be tied to the row holding its captured output and diff.
func workerRunID(run *task.WorkerRun) string {
	if run == nil {
		return ""
	}
	return run.ID.String()
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
