package worker

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"aidev/internal/agent"
	"aidev/internal/event"
	"aidev/internal/git"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/verification"
)

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

	// A cancelled run's context is already done by the time this runs, and a
	// cancel is exactly when the diff matters most: it is the only record of
	// what the agent managed to do. The git calls therefore get a detached
	// context with their own deadline — the same treatment commitWork gives
	// them — instead of inheriting a context that is already closed.
	gitCtx, cancelGit := context.WithTimeout(context.WithoutCancel(ctx), git.DefaultTimeout)
	defer cancelGit()

	if diff, err := r.worktree.Diff(gitCtx); err != nil {
		r.log.WarnContext(ctx, "could not collect the worktree diff", "error", err.Error())
	} else {
		record.Diff = diff.Patch
		record.DiffTruncated = diff.Truncated
		record.ChangedFiles = diff.ChangedFiles
	}
	// Not fenced, deliberately: this row is the audit record of an agent
	// run that really happened under this attempt, and it is most valuable
	// exactly when someone else ended the attempt meanwhile. It changes no
	// state another party owns. Events, status and anything in git are what
	// the fence keeps a run from writing after losing its attempt.
	if head, err := r.worktree.HeadCommit(gitCtx); err == nil && r.record != nil {
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

// protectedPayload quotes both sides of a ring-fence refusal — the pattern the
// creator declared and the path the attempt touched — so a reviewer can judge
// the refusal from the audit log alone.
func protectedPayload(vs []verification.Violation) []map[string]any {
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, map[string]any{
			"pattern": v.Pattern,
			"path":    v.Path,
		})
	}
	return out
}

func (r *run) persistVerification(ctx context.Context, report verification.Report) {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	// The rows are audit records of checks that really ran, kept like the
	// worker run (see persistWorkerRun); the step events go through emit and
	// are fenced.
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
		if err := r.fence(writeCtx, tx); err != nil {
			return err
		}
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

// fence is the lease check every write of an open attempt starts with
// (store.HoldLease): inside tx, the task and attempt rows are locked and the
// attempt must still be RUNNING under this process. Once the run has recorded
// its attempt's ending — or before it has one — there is nothing to fence.
func (r *run) fence(ctx context.Context, tx *store.Store) error {
	if !r.attemptOpen {
		return nil
	}
	return tx.HoldLease(ctx, r.task.ID, r.attempt.ID, processLeaseOwner())
}

// write runs fn in a transaction behind the fence, on the write budget. A run
// that lost its attempt gets store.ErrLeaseLost and fn never runs.
func (r *run) write(ctx context.Context, fn func(context.Context, *store.Store) error) error {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	return r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := r.fence(writeCtx, tx); err != nil {
			return err
		}
		return fn(writeCtx, tx)
	})
}

// emit appends an informational event.
//
// A failure here is logged rather than propagated: these events describe progress
// rather than state, and abandoning a task because a progress note could not be
// written would trade a real result for a bookkeeping gap. Events that must agree
// with state are written inside the transition transaction instead.
func (r *run) emit(ctx context.Context, evType event.Type, payload any) {
	var attemptID *uuid.UUID
	if r.attempt.ID != uuid.Nil {
		attemptID = &r.attempt.ID
	}
	err := r.write(ctx, func(ctx context.Context, tx *store.Store) error {
		return appendEvent(ctx, tx, r.task.ID, attemptID, evType, payload)
	})
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		// Someone else ended the attempt; its history ends with them.
		r.log.WarnContext(ctx, "the attempt was ended elsewhere; not appending", "event", evType.String())
	case err != nil:
		r.log.ErrorContext(ctx, "could not append an event", "event", evType.String(), "error", err.Error())
	}
}

func (r *run) outcome(message string) Outcome {
	attempt := r.attempt
	out := Outcome{
		Task:          r.task,
		Attempt:       &attempt,
		WorkerRun:     r.workerRun,
		Verification:  r.report,
		Worktree:      r.record,
		TestsModified: r.testsModified,
		Message:       message,
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
