package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aidev/internal/event"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/task"
)

// defaultCancelPoll is how often a run re-reads its task's status when
// Orchestrator.CancelPoll is unset. It is the worst case delay before a Cancel
// from another process stops the agent or the verification commands.
const defaultCancelPoll = 2 * time.Second

// registerRun records the cancel func of a run so that Cancel on this process
// stops it immediately. The map is created on first use so that both the zero
// value and New work.
func (o *Orchestrator) registerRun(id uuid.UUID, cancel context.CancelFunc) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.runs == nil {
		o.runs = map[uuid.UUID]context.CancelFunc{}
	}
	o.runs[id] = cancel
}

// unregisterRun removes a finished run. A later Cancel finds nothing, which is
// the correct answer once the run owns its own ending.
func (o *Orchestrator) unregisterRun(id uuid.UUID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.runs, id)
}

// stopLocalRun cancels the registered run for id, if there is one. The call
// happens after Cancel's transaction committed, so the run's own recording
// observes CANCELLED rather than racing it.
func (o *Orchestrator) stopLocalRun(id uuid.UUID) {
	o.mu.Lock()
	cancel, ok := o.runs[id]
	o.mu.Unlock()
	if ok {
		cancel()
	}
}

// cancelPoll is how often a run checks whether another process cancelled its
// task. A non-positive CancelPoll means the default.
func (o *Orchestrator) cancelPoll() time.Duration {
	if o.CancelPoll > 0 {
		return o.CancelPoll
	}
	return defaultCancelPoll
}

// startCancelWatch polls the task's status until the run ends and cancels the
// run's context when the task was cancelled elsewhere. It returns a function
// that stops the watcher and waits for it, so the watcher never outlives the
// run.
func (r *run) startCancelWatch(ctx context.Context, stop context.CancelFunc) (wait func()) {
	// The run goes on changing r.task and r.log while the watcher polls, so the
	// watcher gets its own copies and never touches r.
	// The attempt is the exception: a retry starts a new one while the
	// watcher keeps running, and RenewLease only renews a RUNNING attempt,
	// so the watcher follows r.leaseAttempt rather than a copy — otherwise
	// the new attempt's lease would lapse and recovery would cancel a live
	// run.
	st := r.o.Store
	if r.leaseAttempt.Load() == nil {
		id := r.attempt.ID
		r.leaseAttempt.Store(&id)
	}
	every, log := r.o.cancelPoll(), r.log
	owner, ttl := processLeaseOwner(), leaseTTL(every)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Each poll renews the lease and reads the status in one statement:
		// a run that keeps its claim alive is a run that would notice a
		// cancel, so the two cannot drift apart (research C1). A dead process
		// stops renewing, and recovery claims its task back.
		watchForCancel(ctx, every, func(ctx context.Context) (task.Status, error) {
			return st.RenewLease(ctx, *r.leaseAttempt.Load(), owner, ttl)
		}, stop, log)
	}()
	return func() {
		stop()
		<-done
	}
}

// Cancel stops a task that has not finished.
//
// A run in progress is stopped as well: immediately when it runs on this
// process, and within the poll interval when it runs in another process and
// notices the CANCELLED status. The cancelled attempt is finished as
// CANCELLED and its worktree is retained: work in progress may still be
// useful, and discarding it would be the one thing the cleanup policy
// promises never to do.
func (o *Orchestrator) Cancel(ctx context.Context, idOrRef, reason string) (Outcome, error) {
	// A cancel can lose its compare-and-set to a transition the run itself is
	// making at the same instant — READY → RUNNING as the task starts,
	// RUNNING → VERIFYING as the agent finishes. That is normal concurrency
	// between two processes looking at one task, not an error to hand the
	// user: re-read the task and cancel whatever status it is in now, up to a
	// few times, before giving up.
	const maxAttempts = 3

	var t task.Task
	var err error
	for attempt := 1; ; attempt++ {
		t, err = o.Store.ResolveTask(ctx, idOrRef)
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
		if err == nil || !errors.Is(err, store.ErrConflict) || attempt == maxAttempts {
			break
		}
	}
	if err != nil {
		return Outcome{}, err
	}

	previous := t.Status
	t.Status = task.StatusCancelled
	o.Logger.InfoContext(ctx, "task cancelled",
		logging.FieldTaskRef, t.Ref, "previous_status", previous.String(), "reason", reason)

	// A run on this process is already registered (or finished) either way;
	// polling remains the path for runs in other processes.
	o.stopLocalRun(t.ID)

	return Outcome{
		Task:    t,
		Message: fmt.Sprintf("%s cancelled (was %s); any worktree was kept for inspection", t.Identifier(), previous),
	}, nil
}

// RecoveredTask is one task a recover pass found and what it did about it.
type RecoveredTask struct {
	Ref            string     `json:"ref"`
	Status         string     `json:"status"`
	Action         string     `json:"action"` // "cancelled" or "would_cancel"
	LeaseOwner     string     `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	Reason         string     `json:"reason"`
}

// Recover cancels tasks whose lease has expired: RUNNING or VERIFYING with no
// process checking in any more (research C1). Each one goes through Cancel, so
// the worktree is retained, the open attempt is closed, and the history
// records a task.cancelled naming the lease — exactly what an operator's
// manual cancel would do, with the evidence as the reason. A task whose lease
// is still alive belongs to a process running right now and is left alone.
//
// With dryRun nothing is cancelled: the tasks that would be are listed, which
// is the question to ask before letting an unattended command act.
func (o *Orchestrator) Recover(ctx context.Context, dryRun bool) ([]RecoveredTask, error) {
	stuck, err := o.Store.StuckLeases(ctx)
	if err != nil {
		return nil, fmt.Errorf("find tasks with an expired lease: %w", err)
	}
	recovered := make([]RecoveredTask, 0, len(stuck))
	for _, s := range stuck {
		owner := s.LeaseOwner
		if owner == "" {
			owner = "unknown"
		}
		lastSeen := "never"
		if s.LeaseExpiresAt != nil {
			lastSeen = s.LeaseExpiresAt.UTC().Format(time.RFC3339)
		}
		reason := fmt.Sprintf("lease expired: owner %s, last seen %s", owner, lastSeen)
		item := RecoveredTask{
			Ref:            s.TaskRef,
			Status:         s.Status.String(),
			LeaseOwner:     owner,
			LeaseExpiresAt: s.LeaseExpiresAt,
			Reason:         reason,
		}
		if dryRun {
			item.Action = "would_cancel"
			recovered = append(recovered, item)
			continue
		}
		if _, err := o.Cancel(ctx, s.TaskRef, reason); err != nil {
			// The task finished or vanished between the listing and the
			// cancel. There is nothing left to recover, and that is not a
			// failure of this pass.
			if errors.Is(err, ErrNotRunnable) || errors.Is(err, store.ErrNotFound) {
				continue
			}
			return recovered, fmt.Errorf("recover %s: %w", s.TaskRef, err)
		}
		item.Action = "cancelled"
		recovered = append(recovered, item)
	}
	return recovered, nil
}

// cancelledElsewhere adopts the ending Cancel already recorded after a
// compare-and-set lost to it. Cancel finishes the attempt, retains the
// worktree and appends task.cancelled; the run must neither fail on the lost
// write nor record a second ending, so a conflict whose current status is
// CANCELLED is the expected outcome, reported with a nil error. Any other
// error, or a conflict with any other status, is not.
func (r *run) cancelledElsewhere(ctx context.Context, cause error) (Outcome, bool) {
	if !errors.Is(cause, store.ErrConflict) {
		return Outcome{}, false
	}
	reloadCtx, reloadCancel := writeContext(ctx)
	defer reloadCancel()
	current, err := r.o.Store.GetTask(reloadCtx, r.task.ID)
	if err != nil || current.Status != task.StatusCancelled {
		return Outcome{}, false
	}
	r.task = current
	if wt, wtErr := r.o.Store.GetWorktreeByAttempt(reloadCtx, r.attempt.ID); wtErr == nil {
		r.record = &wt
	}
	summary := fmt.Sprintf("%s is CANCELLED: it was cancelled while the run was in progress", r.task.Identifier())
	if r.worktree != nil {
		summary += fmt.Sprintf("; the worktree was kept at %s", r.worktree.Path)
	}
	r.log.InfoContext(ctx, "task was cancelled while the run was in progress",
		"status", current.Status.String())
	return r.outcome(summary), true
}
