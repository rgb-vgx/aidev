package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/event"
	"aidev/internal/git"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/verification"
)

// run carries the state of one execution through the lifecycle.
type run struct {
	o    *Orchestrator
	task task.Task
	log  *slog.Logger

	attempt  task.TaskAttempt
	worktree *git.Worktree
	record   *task.Worktree

	// agentTree is the tree the agent's work was snapshotted to the moment
	// it finished (research A6): what the success commit carries, and what
	// clean verification checks out. Verification runs afterwards, so
	// nothing the checks write can reach it.
	agentTree string
	// agentHead is the commit HEAD was on when agentTree was taken — the
	// tree's parent, and the value the branch must still hold when the
	// success commit is published. Usually the base commit; an agent that
	// committed its own work moved it.
	agentHead string

	// sharedBefore is the shared git state recorded once the worktree exists,
	// for the containment check that runs after the agent (research §7i).
	sharedBefore git.SharedState

	workerRun *task.WorkerRun
	report    *verification.Report

	// testsModified is set in verify when the changed paths include the tests
	// judging the attempt, and rides along in the outcome (research §7b tier 1).
	testsModified []string

	// failureKind records how the run failed for the trace. It stays
	// FailureNone when the run succeeded or has not failed yet.
	failureKind task.FailureKind

	// retryable marks the failure about to be reported as one another
	// attempt could fix: set right before fail by the two places that know —
	// verify when the checks ran and failed, execute when the agent stopped
	// early without a refusal (see willRetry).
	retryable bool
	// retryPending is set by scheduleRetry: the attempt failed, the task is
	// READY again, and execute starts the next attempt.
	retryPending bool
	// retry carries what the next attempt needs from the failed one; nil on
	// a first attempt.
	retry *retryContext

	// attemptOpen is true from startAttempt until the run records the
	// attempt's ending (or learns someone else did). While it is, every
	// write passes the lease fence (see fence).
	attemptOpen bool

	// leaseAttempt is the attempt whose lease the cancel watch renews. It
	// changes when a retry starts a new attempt, while the watcher keeps
	// running, so it is the one field the watcher reads and is atomic.
	leaseAttempt atomic.Pointer[uuid.UUID]
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
	// The run's own context: a Cancel from any process stops the agent and
	// the verification commands running under it. The watcher notices a
	// Cancel from another process by polling; the registry delivers a Cancel
	// on this process without waiting for the poll.
	runCtx, stopRun := context.WithCancel(ctx)
	r.o.registerRun(r.task.ID, stopRun)
	defer r.o.unregisterRun(r.task.ID)
	stopWatch := r.startCancelWatch(runCtx, stopRun)
	defer stopWatch()
	ctx = runCtx
	root.SetAttributes(attribute.Int("aidev.attempt.number", r.attempt.AttemptNumber))

	// Each attempt logs with its own id and number; a retry starts again
	// from the run's logger rather than stacking a second set of fields.
	baseLog := r.log
	r.log = r.log.With(
		logging.FieldAttemptID, r.attempt.ID.String(),
		logging.FieldAttemptNum, r.attempt.AttemptNumber,
	)
	r.log.InfoContext(ctx, "task started", logging.FieldBackend, r.o.Backend.Name())

	for {
		outcome, retErr = r.attemptOnce(ctx)
		if !r.retryPending || retErr != nil {
			return outcome, retErr
		}
		// The attempt failed in a way another try can fix and the task is
		// READY again (scheduleRetry). The next attempt starts now, in the
		// same run: its own attempt row and lease, the same worktree
		// directory, the same agent session.
		r.resetForAttempt()
		if err := r.startAttempt(ctx); err != nil {
			// A Cancel between the attempts owns the ending.
			if out, ok := r.cancelledElsewhere(ctx, err); ok {
				return out, nil
			}
			retErr = err
			return Outcome{}, retErr
		}
		root.SetAttributes(attribute.Int("aidev.attempt.number", r.attempt.AttemptNumber))
		r.log = baseLog.With(
			logging.FieldAttemptID, r.attempt.ID.String(),
			logging.FieldAttemptNum, r.attempt.AttemptNumber,
			logging.FieldWorktreePath, r.worktree.Path,
		)
		r.log.InfoContext(ctx, "retry started")
	}
}

// attemptOnce takes one attempt from its worktree to its outcome: prepare
// (or, on a retry, continue) the worktree, run the agent, check containment,
// snapshot, verify. A failure another attempt could fix comes back with
// retryPending set instead of an ending (see fail and scheduleRetry).
func (r *run) attemptOnce(ctx context.Context) (Outcome, error) {
	if r.retry != nil {
		// A retry continues in the failed attempt's directory (see
		// retry.go). Were that to break, the failure path should keep the
		// directory under the record that still describes it.
		if err := r.continueWorktree(ctx); err != nil {
			if r.record == nil {
				r.record = r.retry.prevRecord
			}
			return r.fail(ctx, task.FailureWorktree, err)
		}
	} else {
		if err := r.prepareWorktree(ctx); err != nil {
			return r.fail(ctx, task.FailureWorktree, err)
		}
		// The red-before-green gate (research B3): on a task that asked for it,
		// the verification commands run on the base commit first, while the
		// worktree still is the base commit. A pass there means the commands
		// cannot distinguish before from after, so the run stops without ever
		// calling the agent. A retry skips it: the base has not changed, and
		// the gate already passed before the first attempt.
		if r.task.ExpectFailOnBase {
			if out, stopped, err := r.baseCheck(ctx); stopped || err != nil {
				return out, err
			}
		}
	}
	if err := r.runAgent(ctx); err != nil {
		// An agent that stopped early may finish on another try; one cut
		// short by a refused tool call would only be refused again.
		r.retryable = !errors.Is(err, agent.ErrToolRefused)
		return r.fail(ctx, r.workerFailureKind(), err)
	}
	if err := r.checkContainment(ctx); err != nil {
		kind := task.FailureContainment
		if errors.Is(err, errSharedStateUnreadable) {
			// The check could not run. Calling that a breach would blame the
			// agent for aidev's own inability to look, so it is classified
			// with the other inspection failures.
			kind = task.FailureWorktree
		}
		return r.fail(ctx, kind, err)
	}
	// The agent's delivery, as a tree object (research A6). Everything
	// verification does happens after this point, so whatever the checks
	// write — coverage output, a regenerated fixture, a gofmt run — cannot
	// reach the commit. Failing to snapshot is a plumbing failure, not a
	// check that failed, so it is classified like the other worktree errors.
	if err := r.snapshotAgentTree(ctx); err != nil {
		return r.fail(ctx, task.FailureWorktree, err)
	}
	return r.verify(ctx)
}

// snapshotAgentTree records what the agent delivered as a tree object and
// keeps its id for the commit. The database copy is best-effort: the
// in-memory one is what the commit uses, and losing the row's copy costs a
// later reader the ability to inspect the snapshot, which is not worth
// failing a run over (the same trade SetWorktreeHead makes).
func (r *run) snapshotAgentTree(ctx context.Context) error {
	// HEAD first: CurrentTree seeds its index from HEAD, so reading HEAD
	// before and the tree after names the pair the tree was built on. Nothing
	// runs in the worktree between the two — the agent has exited.
	head, err := r.worktree.HeadCommit(ctx)
	if err != nil {
		return fmt.Errorf("snapshot the agent's work: %w", err)
	}
	tree, err := r.worktree.CurrentTree(ctx)
	if err != nil {
		return fmt.Errorf("snapshot the agent's work: %w", err)
	}
	r.agentHead, r.agentTree = head, tree

	if r.record == nil {
		return nil
	}
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := r.o.Store.SetWorktreeAgentTree(writeCtx, r.record.ID, tree); err != nil {
		r.log.WarnContext(ctx, "could not record the agent's tree", "error", err.Error())
	}
	return nil
}

// requiredBy reports why this task must be approved before it runs: "task",
// "project", "task+project", or "" when neither gate applies. The two flags
// are OR'd here at run time and deliberately never merged into one stored
// flag, so "explicitly requested by the creator" stays distinguishable from
// "inherited from project policy" and switching the policy off does not
// rewrite any task (migration 0008).
//
// A project lookup failure blocks the task: a gate that cannot read its own
// policy must not let the task through.
func (r *run) requiredBy(ctx context.Context) (string, error) {
	p, err := r.o.Store.GetProject(ctx, r.task.ProjectID)
	if err != nil {
		return "", fmt.Errorf("read project approval policy: %w", err)
	}
	switch {
	case r.task.RequiresApproval && p.RequiresApproval:
		return "task+project", nil
	case r.task.RequiresApproval:
		return "task", nil
	case p.RequiresApproval:
		return "project", nil
	}
	return "", nil
}

// approvalReason renders requiredBy as the sentence recorded on the approval
// request, so an operator reading the request knows which gate produced it.
func approvalReason(requiredBy string) string {
	switch requiredBy {
	case "task":
		return "task is marked as requiring approval"
	case "project":
		return "project policy requires approval"
	default:
		return "task and project policy require approval"
	}
}

// enforceApproval applies the approval policy. It is checked before anything is
// claimed or created, so a gated task leaves no worktree and no attempt behind.
func (r *run) enforceApproval(ctx context.Context) (Outcome, bool, error) {
	requiredBy, err := r.requiredBy(ctx)
	if err != nil {
		return Outcome{}, true, err
	}
	if requiredBy == "" {
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
		a, reqErr := tx.RequestApproval(ctx, r.task.ID, approvalReason(requiredBy))
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
			"required_by": requiredBy,
		})
	})
	if err != nil {
		return Outcome{}, true, err
	}

	r.task.Status = task.StatusWaitingApproval
	r.log.InfoContext(ctx, "task gated pending approval",
		"approval_id", approval.ID.String(), "required_by", requiredBy)

	return Outcome{
		Task:     r.task,
		Approval: &approval,
		Message: fmt.Sprintf("%s requires approval (%s) and was not run; approve it to continue",
			r.task.Identifier(), requiredBy),
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
		attempt := task.NewAttempt(r.task.ID, number)
		// The lease proves a live process holds this attempt: the cancel
		// watch renews it every poll, and recovery claims the task back once
		// the renewals stop (research C1).
		attempt.LeaseOwner = processLeaseOwner()
		expires := time.Now().UTC().Add(leaseTTL(r.o.cancelPoll()))
		attempt.LeaseExpiresAt = &expires
		attempt, err = tx.CreateAttempt(writeCtx, attempt)
		if err != nil {
			return err
		}
		r.attempt = attempt
		r.attemptOpen = true
		id := attempt.ID
		r.leaseAttempt.Store(&id)
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

	baseRef := effectiveBaseRef(r.task.BaseRef, project.DefaultBranch)

	wt, err := r.o.Git.Create(ctx, git.CreateRequest{
		Repository: repo,
		Name:       worktreeName(r.task, r.attempt),
		Branch:     branchName(r.task, r.attempt),
		BaseRef:    baseRef,
		// A failure to load submodules is a failure to prepare the checkout,
		// so it lands in FailureWorktree with every other one: the caller
		// already classifies whatever prepareWorktree returns that way.
		LoadSubmodules: project.Submodules == task.SubmodulesReadOnly,
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
		attribute.Int("aidev.worktree.submodules", len(wt.Submodules)),
	)
	r.log = r.log.With(logging.FieldWorktreePath, wt.Path)

	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	if err := r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		// The worktree record is state someone else owns (a reviewer reads
		// it, Cancel and recovery retain it), so it is written behind the
		// lease fence like everything else of an open attempt.
		if err := r.fence(writeCtx, tx); err != nil {
			return err
		}
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

		payload := map[string]any{
			"path":        wt.Path,
			"branch":      wt.Branch,
			"base_commit": wt.BaseCommit,
		}
		// Only when there are some: an empty list in every event of every
		// single-repository project would be noise.
		if len(wt.Submodules) > 0 {
			payload["submodules"] = wt.Submodules
		}
		return appendEvent(writeCtx, tx, r.task.ID, &r.attempt.ID, event.TypeWorktreeCreated, payload)
	}); err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			// The attempt was ended while the checkout was being made, so
			// nothing recorded it: remove it rather than leave a directory
			// on disk that no row points at.
			cleanupCtx, cancelCleanup := writeContext(ctx)
			defer cancelCleanup()
			if rmErr := r.o.Git.Remove(cleanupCtx, wt, true); rmErr != nil {
				r.log.WarnContext(ctx, "could not remove the worktree of an attempt that was ended", "path", wt.Path, "error", rmErr.Error())
			}
		}
		return err
	}

	// The base ref is resolved now, not when the task was created (research
	// D2). A ref that moved in between is not an error — rebasing work onto
	// the latest code is often exactly what was wanted — but the agent is
	// starting from code the task's author never saw, so the reviewer is told.
	if at := r.task.BaseCommitAtCreate; at != "" && at != wt.BaseCommit {
		r.log.WarnContext(ctx, "the base ref moved since the task was created",
			"base_ref", baseRef, "at_create", at, "at_run", wt.BaseCommit)
		r.emit(ctx, event.TypeBaseMoved, map[string]any{
			"base_ref":  baseRef,
			"at_create": at,
			"at_run":    wt.BaseCommit,
		})
	}

	// Baseline for the containment check: taken now, after the worktree exists
	// and is recorded, so a snapshot failure here is a prepared worktree the
	// failure path can retain rather than a half-created one. From this point
	// until the check runs, anything that changes this state changed it
	// deliberately (docs/research.md §7i).
	shared, err := r.worktree.SnapshotSharedState(ctx)
	if err != nil {
		return fmt.Errorf("snapshot shared repository state: %w", err)
	}
	r.sharedBefore = shared
	return nil
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

	prompt, err := r.prompt()
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
	// A retry continues the failed attempt's agent session, in the same
	// directory it was created in, so the agent keeps what it learned.
	if r.retry != nil {
		req.SessionID = r.retry.sessionID
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

// errSharedStateUnreadable marks a containment check that could not run: the
// shared state could not be re-read, so nothing was found and nothing was
// proven. Callers classify it as a worktree failure, not as a breach.
var errSharedStateUnreadable = errors.New("shared repository state could not be re-read")

// checkContainment compares the shared git state the agent ran against with
// what is there now and reports tampering. It runs after the agent and before
// verification, because a workspace whose shared state was edited cannot be
// trusted: verification would be judging evidence the agent has already
// shaped (docs/research.md §7i).
//
// A breach returns an error, which the caller records as FailureContainment.
// A foreign ref moving is only warned about. The worktree's own branch is
// ignored — that is where its work belongs, and verification judges it — and
// so are the branches of other attempts whose runs overlapped this one: tasks
// of one repository running side by side create and advance their branches
// while each other's agents work (docs/research.md §7k), and warning about
// each would bury the change that matters. What remains is a ref no
// concurrent aidev run accounts for.
func (r *run) checkContainment(ctx context.Context) error {
	after, err := r.worktree.SnapshotSharedState(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errSharedStateUnreadable, err)
	}
	changes := r.sharedBefore.Diff(after)

	// The branch must still contain its base commit; otherwise the diff and
	// the verification would be judged against a history that no longer
	// exists. A git error here means the check could not conclude.
	ancestor, err := r.worktree.BaseIsAncestor(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errSharedStateUnreadable, err)
	}

	ownRef := "refs/heads/" + r.worktree.Branch
	foreign := make(map[string]git.RefChange)
	for ref, ch := range changes.RefChanges {
		if ref != ownRef {
			foreign[ref] = ch
		}
	}
	r.dropConcurrentBranches(ctx, foreign)
	if len(foreign) > 0 {
		r.emit(ctx, event.TypeSharedRefsChanged, map[string]any{
			"refs": foreign,
		})
	}

	var reasons []string
	if changes.CommonDirMoved {
		reasons = append(reasons, fmt.Sprintf("the worktree's gitdir moved from %q to %q", r.sharedBefore.CommonDir, after.CommonDir))
	}
	if changes.ConfigChanged {
		reasons = append(reasons, "the shared config changed")
	}
	if len(changes.InfoChanged) > 0 {
		reasons = append(reasons, "shared info files changed: "+strings.Join(changes.InfoChanged, ", "))
	}
	if len(changes.HooksChanged) > 0 {
		reasons = append(reasons, "shared hooks changed: "+strings.Join(changes.HooksChanged, ", "))
	}
	if changes.HeadMoved {
		reasons = append(reasons, fmt.Sprintf("HEAD moved from %q to %q", r.sharedBefore.HeadRef, after.HeadRef))
	}
	if !ancestor {
		reasons = append(reasons, "the base commit is no longer an ancestor of HEAD")
	}
	if len(reasons) == 0 {
		return nil
	}

	payload := map[string]any{
		"reasons":        reasons,
		"config_changed": changes.ConfigChanged,
		"head_before":    r.sharedBefore.HeadRef,
		"head_after":     after.HeadRef,
	}
	// Only when there are some: an empty list in every breach event of a
	// clean run would be noise.
	if len(changes.InfoChanged) > 0 {
		payload["info_changed"] = changes.InfoChanged
	}
	if len(changes.HooksChanged) > 0 {
		payload["hooks_changed"] = changes.HooksChanged
	}
	if len(foreign) > 0 {
		payload["refs"] = foreign
	}
	r.emit(ctx, event.TypeContainmentBreach, payload)

	return fmt.Errorf("the agent modified state shared with the main repository: %s", strings.Join(reasons, "; "))
}

// dropConcurrentBranches removes from foreign the branches that belong to
// other attempts running alongside this one. If the lookup fails the
// warnings stay: a noisy warning is better than a missing one.
func (r *run) dropConcurrentBranches(ctx context.Context, foreign map[string]git.RefChange) {
	var names []string
	for ref := range foreign {
		if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			names = append(names, branch)
		}
	}
	if len(names) == 0 {
		return
	}
	readCtx, cancel := writeContext(ctx)
	defer cancel()
	concurrent, err := r.o.Store.OverlappingBranches(readCtx, r.attempt.ID, r.attempt.StartedAt, names)
	if err != nil {
		r.log.WarnContext(ctx, "could not tell concurrent tasks' branches apart; reporting every ref change", "error", err.Error())
		return
	}
	for branch := range concurrent {
		delete(foreign, "refs/heads/"+branch)
	}
}

// baseCheck runs the task's verification on the base commit before the agent
// touches anything (research B3, the red-before-green gate). Commands that
// already pass on the base cannot tell the before state from the after state:
// such a task would report success while nothing changed, so the attempt stops
// here — FAILED (VERIFICATION), agent never called. Returns stopped=false when
// the base is red and the run may proceed to the agent.
//
// The pass runs in a detached checkout of the base commit, not in the agent's
// worktree: setup steps leave artefacts (npm ci, a touched fixture), and an
// artefact present before the agent ran would later read as a change the agent
// made — tripping interception or the protected paths on evidence aidev itself
// produced. The checkout is removed on every path; it is scaffolding, like
// clean verification's.
//
// Plumbing failures fail closed: a base check that could not run has not
// established that the base is red, and silently skipping the gate would be
// the exact bug this gate exists to prevent.
func (r *run) baseCheck(ctx context.Context) (Outcome, bool, error) {
	ctx, span := otel.Tracer("aidev").Start(ctx, "aidev.base_check")
	defer span.End()

	wt, err := r.o.Git.CreateDetached(ctx, r.worktree.Repository(),
		"basecheck-"+r.attempt.ID.String(), r.worktree.BaseCommit)
	if err != nil {
		span.SetAttributes(attribute.Bool("aidev.base_check.passed", false))
		out, failErr := r.fail(ctx, task.FailureWorktree, fmt.Errorf("base check not run: %w", err))
		return out, true, failErr
	}
	defer func() {
		// Always removed — success, failure and cancellation alike. Not the
		// caller's context either: a cancelled task must still lose its
		// scaffolding, or every cancelled run leaks a worktree. Force is
		// right — running the checks left build artefacts in it.
		writeCtx, cancel := writeContext(ctx)
		defer cancel()
		if err := r.o.Git.Remove(writeCtx, wt, true); err != nil {
			r.log.ErrorContext(ctx, "could not remove the temporary base-check worktree",
				"path", wt.Path, "error", err.Error())
		}
	}()

	report, err := r.o.Verifier.Run(ctx, verification.Request{
		AttemptID:  r.attempt.ID,
		WorkingDir: wt.Path,
		SetupSteps: r.task.SetupSteps,
		Steps:      r.task.Verification,
	})
	if err != nil {
		// The pass never ran, so it did not prove the base is red.
		span.SetAttributes(attribute.Bool("aidev.base_check.passed", false))
		out, failErr := r.fail(ctx, task.FailureInternal, fmt.Errorf("base check not run: %w", err))
		return out, true, failErr
	}
	span.SetAttributes(attribute.Bool("aidev.base_check.passed", report.Passed))
	r.traceVerificationSteps(ctx, report)

	// One event for the whole pass, on both paths: a reviewer must be able to
	// see that the gate ran and what it found, not only that it fired. The
	// steps are deliberately not persisted as verification_runs rows — the
	// UNIQUE (attempt_id, step_index) sequence belongs to the post-agent
	// verdict, and on the firing path the attempt ends before that verdict
	// ever exists, so the payload's summary is the record.
	r.emit(ctx, event.TypeBaseCheckCompleted, map[string]any{
		"passed":       report.Passed,
		"failure_kind": string(report.FailureKind),
		"summary":      report.Summary(),
		"duration_ms":  report.Duration.Milliseconds(),
	})
	r.log.InfoContext(ctx, "base check finished",
		"passed", report.Passed,
		logging.FieldFailureKind, string(report.FailureKind),
		logging.FieldDurationMS, report.Duration.Milliseconds())

	if report.Passed {
		// Carried into the failed payload and the outcome, so a reader sees
		// what "already passes on the base" meant without re-running it.
		r.report = &report
		out, failErr := r.fail(ctx, task.FailureVerification, fmt.Errorf(
			"verification already passes on the base commit: the commands do not distinguish "+
				"before from after (%s)", report.Summary()))
		return out, true, failErr
	}
	r.log.InfoContext(ctx, "base is red as required; the commands can tell before from after",
		"summary", report.Summary())
	return Outcome{}, false, nil
}

// verify runs the task's own commands and decides the outcome.
func (r *run) verify(ctx context.Context) (Outcome, error) {
	ctx, span := otel.Tracer("aidev").Start(ctx, "aidev.verification")
	defer span.End()

	if err := r.transition(ctx, task.StatusVerifying, event.TypeVerificationStarted, map[string]any{
		"steps": len(r.task.Verification),
		"mode":  string(r.task.VerificationMode),
	}); err != nil {
		// A Cancel that landed after the agent finished owns the ending.
		if out, ok := r.cancelledElsewhere(ctx, err); ok {
			return out, nil
		}
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

	// Report the edited tests before the interception check: an attempt that
	// edited tests and was then caught still edited them, and a reviewer needs
	// that. The classifier only reports — it never decides the outcome
	// (research §7b tier 1); refusing a run for touching a test path is the
	// separate protected_paths decision (tier 2).
	if paths := verification.TestPaths(changed); len(paths) > 0 {
		r.testsModified = paths
		r.emit(ctx, event.TypeVerificationTestsModified, map[string]any{"paths": paths})
	}

	// Setup commands deserve the same independence protection as the checks
	// they prepare for: both run from the task, and both are refused before
	// anything executes if their target changed during the attempt. The
	// combined list shares one step_index numbering with verification_runs,
	// so the indexes in the refusal match the rows a reader will find.
	allSteps := make([]task.VerificationStep, 0, len(r.task.SetupSteps)+len(r.task.Verification))
	allSteps = append(allSteps, r.task.SetupSteps...)
	allSteps = append(allSteps, r.task.Verification...)

	intercepted := verification.Interceptions(allSteps, changed)
	violated, err := verification.Violations(r.task.ProtectedPaths, changed)
	if err != nil {
		// A stored pattern that cannot be evaluated means the guard the task's
		// creator asked for is gone: fail closed rather than run checks that
		// were supposed to be protected (research §7b tier 2).
		span.SetAttributes(attribute.Bool("aidev.verification.passed", false))
		return r.fail(ctx, task.FailureInternal, fmt.Errorf("verification not run: %w", err))
	}
	if len(intercepted) > 0 || len(violated) > 0 {
		span.SetAttributes(attribute.Bool("aidev.verification.passed", false))
		// One event type for both refusals — a reviewer reads
		// task.verification_intercepted as "this run was not trusted"; the
		// payload says which reason fired, and only the reasons that did.
		payload := map[string]any{}
		if len(intercepted) > 0 {
			payload["interceptions"] = interceptionPayload(intercepted)
		}
		if len(violated) > 0 {
			payload["protected_paths"] = protectedPayload(violated)
		}
		r.emit(ctx, event.TypeVerificationIntercepted, payload)
		return r.fail(ctx, task.FailureVerification, refusalError(intercepted, violated))
	}

	// In clean mode the checks run on a detached checkout of the snapshot —
	// the exact tree a commit would carry — so a file git ignores, or a file
	// the agent never added, cannot make them pass. Everything from snapshot
	// to checkout is plumbing: if any of it breaks, the failure kind says so
	// (FailureWorktree) instead of pretending a check failed.
	workingDir := r.worktree.Path
	if r.task.VerificationMode == task.VerificationClean {
		path, cleanup, err := r.cleanCheckout(ctx)
		if err != nil {
			span.SetAttributes(attribute.Bool("aidev.verification.passed", false))
			return r.fail(ctx, task.FailureWorktree, fmt.Errorf("verification not run: %w", err))
		}
		// Always removed — on success, on failure and on cancellation
		// alike. The detached checkout is scaffolding, not a record: the
		// agent's worktree on the task branch is what a reviewer inspects.
		defer cleanup()
		workingDir = path
	}

	report, err := r.o.Verifier.Run(ctx, verification.Request{
		AttemptID:  r.attempt.ID,
		WorkingDir: workingDir,
		SetupSteps: r.task.SetupSteps,
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

	if report.Passed && r.task.VerificationMode != task.VerificationClean {
		r.reportWorktreeDrift(ctx)
	}

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
		// The checks ran and failed: the one verification outcome another
		// attempt can change. A timed-out or cancelled step is not.
		r.retryable = report.FailureKind == task.FailureVerification
		return r.fail(ctx, report.FailureKind, fmt.Errorf("verification did not pass: %s", report.Summary()))
	}
	return r.succeed(ctx)
}

// maxDriftPaths bounds the paths a drift warning lists. A verification that
// regenerates a whole tree would otherwise write an event the size of the
// tree; the count still says how many there were.
const maxDriftPaths = 50

// reportWorktreeDrift warns when the passing checks left the worktree
// different from the tree snapshotted before they ran (research A6). The
// commit carries the snapshot, so a gofmt, a codegen step or a stray output
// file the checks produced is not in it — what passed is not exactly what
// the branch receives. It is a report: it never changes the outcome, and an
// error working it out is logged rather than failing a run that passed.
//
// Clean mode skips it, because there the checks never touched this worktree.
func (r *run) reportWorktreeDrift(ctx context.Context) {
	if r.agentTree == "" {
		return
	}
	after, err := r.worktree.CurrentTree(ctx)
	if err != nil {
		r.log.WarnContext(ctx, "could not check whether verification changed the worktree", "error", err.Error())
		return
	}
	if after == r.agentTree {
		return
	}
	changes, err := r.worktree.TreeChanges(ctx, r.agentTree, after)
	if err != nil {
		r.log.WarnContext(ctx, "could not list what verification changed in the worktree", "error", err.Error())
		return
	}
	listed := changes
	if len(listed) > maxDriftPaths {
		listed = listed[:maxDriftPaths]
	}
	paths := make([]map[string]any, 0, len(listed))
	for _, c := range listed {
		paths = append(paths, map[string]any{"status": c.Status, "path": c.Path})
	}
	r.emit(ctx, event.TypeVerificationWorktreeModified, map[string]any{
		"agent_tree": r.agentTree,
		"after_tree": after,
		"paths":      paths,
		"total":      len(changes),
	})
	r.log.WarnContext(ctx, "verification changed the worktree; the commit carries the agent's snapshot",
		"paths", len(changes))
}

// cleanCheckout builds the detached worktree clean verification runs in:
// the post-agent tree snapshotted to a tree object, committed without
// touching any ref, and checked out under the workspace root. It returns the
// path to verify in and a cleanup that removes the checkout — the caller
// defers it, so the scaffolding disappears on success, failure and
// cancellation alike.
//
// Every failure is plumbing, not a check: the caller reports them as
// FailureWorktree so nobody mistakes "the snapshot broke" for "the tests
// failed".
func (r *run) cleanCheckout(ctx context.Context) (string, func(), error) {
	// The tree checked is the tree the commit will carry (research A6): the
	// snapshot execute took when the agent finished. Taking a second one here
	// would read the same files, but reusing the first makes "verified" and
	// "committed" the same object by construction rather than by timing.
	tree := r.agentTree
	if tree == "" {
		snap, err := r.worktree.CurrentTree(ctx)
		if err != nil {
			return "", nil, err
		}
		tree = snap
	}
	if err := r.worktree.CheckGitlinks(ctx, tree); err != nil {
		return "", nil, err
	}
	commit, err := r.worktree.CommitTree(ctx, tree,
		fmt.Sprintf("aidev verification snapshot for %s", r.task.Identifier()))
	if err != nil {
		return "", nil, err
	}
	wt, err := r.o.Git.CreateDetached(ctx, r.worktree.Repository(),
		"verify-"+r.attempt.ID.String(), commit)
	if err != nil {
		return "", nil, err
	}

	cleanup := func() {
		// Deliberately not the caller's context: a cancelled task must
		// still lose its scaffolding, or every cancelled clean run leaks a
		// worktree. Force is right here — the checkout exists to be
		// discarded, and running the checks left build artefacts in it.
		writeCtx, cancel := writeContext(ctx)
		defer cancel()
		if err := r.o.Git.Remove(writeCtx, wt, true); err != nil {
			r.log.ErrorContext(ctx, "could not remove the temporary verification worktree",
				"path", wt.Path, "error", err.Error())
		}
	}
	return wt.Path, cleanup, nil
}

// refusalError names every reason verification refused to run, so the failure
// reason tells a reviewer what happened without opening the audit log.
func refusalError(ins []verification.Interception, vs []verification.Violation) error {
	if len(vs) == 0 {
		return interceptionError(ins)
	}
	parts := make([]string, 0, len(ins)+len(vs))
	for _, in := range ins {
		parts = append(parts, fmt.Sprintf("step %d `%s` would run %s, which changed during this attempt",
			in.StepIndex+1, in.Step, in.Path))
	}
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%s changed during this attempt but is protected by %q",
			v.Path, v.Pattern))
	}
	return fmt.Errorf("verification not run: %s", strings.Join(parts, "; "))
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

// succeed delivers the work, records success, and cleans up per policy, in that
// order. The commit comes first so that a failure to deliver is a failure of
// the task, not a success with a warning; the removal comes last so that a
// task that is no longer VERIFYING — cancelled while verification ran — keeps
// its worktree.
func (r *run) succeed(ctx context.Context) (Outcome, error) {
	// The commit happens inside the transaction that records the success,
	// after the lease fence: with the task and attempt rows locked, a Cancel
	// or a recovery cannot end the attempt between "the branch moved" and
	// "the task succeeded", and a run that already lost its attempt moves
	// nothing. The budget is git's plus the usual write budget, detached from
	// the caller's context like every write that records an ending.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), git.DefaultTimeout+persistTimeout)
	defer cancel()

	var (
		committed bool
		commitErr error
	)
	err := r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := r.fence(writeCtx, tx); err != nil {
			return err
		}
		if committed, commitErr = r.commitWork(ctx, tx); commitErr != nil {
			return commitErr
		}
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
	if commitErr != nil {
		// Nothing was recorded; the failure path fences again and records it.
		return r.fail(ctx, task.FailureWorktree, fmt.Errorf("commit failed: %w", commitErr))
	}
	if err != nil {
		r.attemptOpen = false
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
	r.attemptOpen = false
	r.cleanupAfterSuccess(ctx)
	r.log.InfoContext(ctx, "task succeeded",
		"branch", r.worktree.Branch, "head_commit", r.headCommit())

	// After a retry the branch is the last attempt's, and saying which
	// attempt passed tells the reader why it is not aidev/<ref>.
	on := ""
	if r.attempt.AttemptNumber > 1 {
		on = fmt.Sprintf(" on attempt %d", r.attempt.AttemptNumber)
	}
	if committed {
		return r.outcome(fmt.Sprintf("%s succeeded%s: %s, work committed on %s",
			r.task.Identifier(), on, r.report.Summary(), r.worktree.Branch)), nil
	}
	return r.outcome(fmt.Sprintf("%s succeeded%s: %s; no changes to commit",
		r.task.Identifier(), on, r.report.Summary())), nil
}

// commitWork records the agent's work on the task branch and returns whether a
// commit was made. An empty commit means the agent changed nothing, which is
// still a success once verification passed. Nothing is merged — only the
// task's own branch is written.
func (r *run) commitWork(ctx context.Context, tx *store.Store) (bool, error) {
	// Detached from the caller's context so a cancelled task still delivers,
	// and bounded by git's own timeout rather than the shorter write budget
	// so a slow commit is not cut short by aidev.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), git.DefaultTimeout)
	defer cancel()

	message := fmt.Sprintf("%s %s", r.task.Identifier(), r.task.Title)
	var (
		commit string
		err    error
	)
	if r.agentTree != "" {
		commit, err = r.commitAgentTree(commitCtx, message)
	} else {
		// No snapshot means the run reached success without passing through
		// execute's snapshot — not a path today, but committing the working
		// directory as it stands is the behaviour aidev had before research
		// A6, and is still better than delivering nothing.
		commit, err = r.worktree.Commit(commitCtx, message)
	}
	if err != nil {
		return false, err
	}
	if commit == "" {
		return false, nil
	}
	// Recorded in the caller's transaction, so the head commit and the
	// success it belongs to land together. A failure here fails that
	// transaction too: a SUCCEEDED row whose worktree does not name the
	// delivered commit would mislead anyone reading it later.
	if r.record != nil {
		writeCtx, writeCancel := writeContext(ctx)
		defer writeCancel()
		if err := tx.SetWorktreeHead(writeCtx, r.record.ID, commit); err != nil {
			return false, err
		}
	}
	r.setHeadCommit(commit)
	return true, nil
}

// commitAgentTree publishes the tree snapshotted when the agent finished
// (research A6) and returns the new commit, or "" when that tree is what the
// branch already holds. The commit is built from objects, not from the
// working directory: whatever verification wrote afterwards stays out.
//
// The branch moves by compare-and-set against the commit the snapshot was
// taken on. If anything moved it in between, publishing fails rather than
// stacking the agent's tree on history nobody verified.
func (r *run) commitAgentTree(ctx context.Context, message string) (string, error) {
	headTree, err := r.worktree.TreeOf(ctx, r.agentHead)
	if err != nil {
		return "", err
	}
	if headTree == r.agentTree {
		return "", nil
	}
	commit, err := r.worktree.CommitTreeOnto(ctx, r.agentTree, r.agentHead, message)
	if err != nil {
		return "", err
	}
	if err := r.worktree.UpdateBranch(ctx, r.worktree.Branch, commit, r.agentHead); err != nil {
		return "", err
	}
	// The branch now holds the commit but the real index still describes
	// its parent; without realigning it every delivered worktree would look
	// dirty and keep-on-dirty removal would never remove one. A failure here
	// leaves the commit published, so it is a warning: the worst outcome is
	// a worktree retained that could have been removed.
	if err := r.worktree.SyncIndex(ctx); err != nil {
		r.log.WarnContext(ctx, "could not realign the index after committing", "error", err.Error())
	}
	return commit, nil
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
		if err := r.retainWorktree(ctx, err.Error()); err != nil {
			r.log.WarnContext(ctx, "could not mark the worktree retained", "error", err.Error())
		}
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
	// A failure another attempt could fix, with attempts left, is recorded
	// as the attempt's failure and the task goes back to READY instead
	// (automatic retry). If the retry cannot be recorded, the task fails
	// below as it always did.
	if !cancelled && r.willRetry(ctx, kind) {
		if out, done, err := r.scheduleRetry(ctx, kind, cause); done {
			return out, err
		}
	}

	// Remembered for the root span, which is finalized when execute returns.
	r.failureKind = kind

	message := ""
	if cause != nil {
		message = cause.Error()
	}

	writeCtx, cancel := writeContext(ctx)
	defer cancel()

	err := r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := r.fence(writeCtx, tx); err != nil {
			return err
		}
		// The worktree is retained inside the same fenced transaction, not
		// before it: a run that lost its attempt must not mark anything of
		// the task's, and the retention and the failure belong together.
		if err := r.retainWorktreeTx(writeCtx, tx, message); err != nil {
			return err
		}
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
		// Cancel recorded the ending first: adopt it rather than failing on
		// the lost write or recording a second one.
		if out, ok := r.cancelledElsewhere(ctx, err); ok {
			return out, nil
		}
		return Outcome{}, err
	}

	r.task.Status = next
	r.attemptOpen = false
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

// workerFailureKind reports how the agent failed, falling back to UNKNOWN rather
// than guessing when the backend did not say.
func (r *run) workerFailureKind() task.FailureKind {
	if r.workerRun != nil && r.workerRun.FailureKind != task.FailureNone {
		return r.workerRun.FailureKind
	}
	return task.FailureUnknown
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
