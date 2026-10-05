package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/google/uuid"

	"aidev/internal/event"
	"aidev/internal/git"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/verification"
	"aidev/prompts"
)

// Automatic retry (research F; design settled with the user on 2026-09-17).
//
// A task with max_retries > 0 whose attempt fails in a way another try can
// fix goes back to READY instead of FAILED, and the same run starts the next
// attempt at once. That attempt continues in the same worktree directory,
// because an OpenCode session can only be continued in the directory it was
// created in (docs/research.md §2.12): the failed attempt's work is committed
// to its own branch, the next attempt gets a new branch from there, and the
// agent's session is continued with a prompt that says why the last attempt
// failed. FAILED stays terminal — a task that will retry never passes
// through it.

// retryOutputLimit bounds how much of each failing step's output goes into
// the retry prompt. The end of the output is where a test runner puts the
// failure; the whole of it would crowd out the task.
const retryOutputLimit = 4000

// retryContext is what the next attempt needs from the one that failed.
type retryContext struct {
	previous  int
	kind      task.FailureKind
	message   string
	sessionID string
	failed    []failedStep
	// prevRecord is the failed attempt's worktree record, now REUSED. If
	// the hand-over itself fails, the failure path retains the directory
	// under it.
	prevRecord *task.Worktree
	// baseCommit is the commit the task's first attempt started from. Every
	// attempt's changed paths are measured against it, so an interception
	// check sees a runner the first attempt rewrote even when a later
	// attempt is the one being verified.
	baseCommit string
}

// failedStep is one verification step that did not pass, as the agent is
// shown it.
type failedStep struct {
	Command string
	Outcome string
	Output  string
}

// retryReason says in a sentence what went wrong, for the prompt.
func retryReason(kind task.FailureKind) string {
	switch kind {
	case task.FailureVerification:
		return "The verification commands ran and at least one of them failed."
	case task.FailureAgentExit:
		return "The agent process exited with an error before the work was finished."
	case task.FailureAgentError:
		return "The agent's turn ended before the work was finished."
	default:
		return fmt.Sprintf("It failed with kind %s.", kind)
	}
}

// willRetry decides whether this failure starts another attempt. Only
// failures another try can fix qualify, and only while attempts remain:
//
//   - the checks ran and failed (verify marks this; a refusal to run them —
//     interception, protected paths — is not a failure another try fixes);
//   - the agent stopped early (AGENT_EXIT, AGENT_ERROR), unless a tool call
//     was refused: a refusal ends the session, and the same session would be
//     refused again.
//
// Never retried: cancellation, timeouts (an attempt that ran out of time
// would most likely run out again, at the same cost), containment breaches,
// worktree and internal errors, a passing base check.
func (r *run) willRetry(ctx context.Context, kind task.FailureKind) bool {
	if !r.retryable || ctx.Err() != nil || r.worktree == nil {
		return false
	}
	switch kind {
	case task.FailureVerification, task.FailureAgentExit, task.FailureAgentError:
	default:
		return false
	}
	return r.attempt.AttemptNumber <= r.task.MaxRetries
}

// scheduleRetry records the failed attempt and returns the task to READY for
// the next one. It is fail's alternative when willRetry says so: the attempt
// is FAILED with its kind and message, exactly as without retry, but the task
// is not. The failed attempt's work is committed to its own branch first, so
// the next attempt starts from it and a reviewer can see each attempt's end
// state; the commit is unverified and its message says so.
//
// It reports done=false when the retry could not be recorded and the caller
// should fail the task as usual.
func (r *run) scheduleRetry(ctx context.Context, kind task.FailureKind, cause error) (Outcome, bool, error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	// As in succeed, the git work — commit, reset — happens behind the
	// lease fence inside the transaction that records the retry: a run that
	// lost its attempt must not touch the branch or the worktree a Cancel
	// promised to keep as it was.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*git.DefaultTimeout+persistTimeout)
	defer cancel()

	var (
		partial   string
		commitErr error
	)
	next := r.attempt.AttemptNumber + 1
	err := r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := r.fence(writeCtx, tx); err != nil {
			return err
		}
		if partial, commitErr = r.commitPartialWork(ctx, kind); commitErr != nil {
			return commitErr
		}
		if err := tx.TransitionTask(writeCtx, r.task.ID, r.task.Status, task.StatusReady); err != nil {
			return err
		}
		if err := tx.FinishAttempt(writeCtx, r.attempt.ID, task.AttemptFailed, kind, message); err != nil {
			return err
		}
		if r.record != nil {
			if partial != "" {
				if err := tx.SetWorktreeHead(writeCtx, r.record.ID, partial); err != nil {
					return err
				}
			}
			if err := tx.SetWorktreeStatus(writeCtx, r.record.ID, task.WorktreeReused); err != nil {
				return err
			}
		}
		payload := map[string]any{
			"attempt":      r.attempt.AttemptNumber,
			"next_attempt": next,
			"max_retries":  r.task.MaxRetries,
			"failure_kind": string(kind),
			"error":        message,
			"branch":       r.worktree.Branch,
		}
		if partial != "" {
			payload["partial_commit"] = partial
		}
		if r.report != nil {
			payload["verification"] = r.report.Summary()
		}
		return appendEvent(writeCtx, tx, r.task.ID, &r.attempt.ID, event.TypeRetryScheduled, payload)
	})
	if commitErr != nil {
		// Without the commit the next attempt would start from a branch
		// that does not hold this attempt's work; failing as usual keeps
		// the worktree, and the work in it, for a person instead.
		r.log.WarnContext(ctx, "could not commit the failed attempt's work; not retrying", "error", commitErr.Error())
		return Outcome{}, false, nil
	}
	if err != nil {
		// A Cancel that landed first owns the ending.
		if out, ok := r.cancelledElsewhere(ctx, err); ok {
			return out, true, nil
		}
		return Outcome{}, true, err
	}

	r.log.InfoContext(ctx, "attempt failed; retrying",
		logging.FieldFailureKind, string(kind), "next_attempt", next, "error", message)

	rc := &retryContext{
		previous:   r.attempt.AttemptNumber,
		kind:       kind,
		message:    message,
		prevRecord: r.record,
		baseCommit: r.worktree.BaseCommit,
	}
	if r.workerRun != nil {
		rc.sessionID = r.workerRun.SessionID
	}
	if r.report != nil && kind == task.FailureVerification {
		rc.failed = failedSteps(*r.report)
	}
	r.task.Status = task.StatusReady
	r.attemptOpen = false
	r.retry = rc
	r.retryPending = true
	return Outcome{}, true, nil
}

// commitPartialWork commits what the failed attempt left to its own branch
// and returns the commit, or "" when it changed nothing. The tree is the
// snapshot taken when the agent finished, when there is one, so output the
// checks wrote is left out as it is on success (research A6).
func (r *run) commitPartialWork(ctx context.Context, kind task.FailureKind) (string, error) {
	tree, head := r.agentTree, r.agentHead
	snapshotted := tree != ""
	if !snapshotted {
		var err error
		if head, err = r.worktree.HeadCommit(ctx); err != nil {
			return "", err
		}
		if tree, err = r.worktree.CurrentTree(ctx); err != nil {
			return "", err
		}
	}
	headTree, err := r.worktree.TreeOf(ctx, head)
	if err != nil {
		return "", err
	}
	commit := ""
	if headTree != tree {
		message := fmt.Sprintf("%s attempt %d, not verified (%s): %s",
			r.task.Identifier(), r.attempt.AttemptNumber, kind, r.task.Title)
		if commit, err = r.worktree.CommitTreeOnto(ctx, tree, head, message); err != nil {
			return "", err
		}
		if err := r.worktree.UpdateBranch(ctx, r.worktree.Branch, commit, head); err != nil {
			return "", err
		}
	}
	if snapshotted {
		// HEAD now holds the agent's snapshot. Anything different on disk
		// was written by the checks that ran after it, and must not ride
		// into the next attempt's commit (research A6).
		if err := r.worktree.RestoreToHead(ctx); err != nil {
			return "", err
		}
	} else if commit != "" {
		if err := r.worktree.SyncIndex(ctx); err != nil {
			r.log.WarnContext(ctx, "could not realign the index after committing the failed attempt", "error", err.Error())
		}
	}
	return commit, nil
}

// failedSteps lists the steps that did not pass, with the end of their
// output, for the retry prompt.
func failedSteps(report verification.Report) []failedStep {
	var out []failedStep
	for _, s := range report.Runs {
		if s.Status == task.VerificationPassed || s.Status == task.VerificationSkipped {
			continue
		}
		outcome := string(s.Status)
		if s.ExitCode != nil {
			outcome = fmt.Sprintf("%s, exit code %d", s.Status, *s.ExitCode)
		}
		output := strings.TrimSpace(s.Stdout)
		if e := strings.TrimSpace(s.Stderr); e != "" {
			if output != "" {
				output += "\n"
			}
			output += e
		}
		out = append(out, failedStep{Command: s.Command, Outcome: outcome, Output: tailBytes(output, retryOutputLimit)})
	}
	return out
}

// tailBytes keeps the last max bytes of s, starting at a line boundary when
// one is near so the agent is not shown half a line.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < 200 {
		cut = cut[i+1:]
	}
	return "…\n" + cut
}

// continueWorktree hands the failed attempt's worktree to the new attempt:
// a new branch at the failed attempt's commit, a new worktree record at the
// same path, and a fresh containment baseline. The base commit stays the
// task's original one (see retryContext.baseCommit).
func (r *run) continueWorktree(ctx context.Context) error {
	branch := branchName(r.task, r.attempt)
	if err := r.worktree.ContinueOnNewBranch(ctx, branch); err != nil {
		return err
	}
	r.worktree.BaseCommit = r.retry.baseCommit

	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := r.o.Store.InTx(writeCtx, func(tx *store.Store) error {
		if err := r.fence(writeCtx, tx); err != nil {
			return err
		}
		recorded, err := tx.CreateWorktree(writeCtx, task.Worktree{
			ID:         uuid.Must(uuid.NewV7()),
			AttemptID:  r.attempt.ID,
			Path:       r.worktree.Path,
			Branch:     branch,
			BaseCommit: r.worktree.BaseCommit,
			Status:     task.WorktreeActive,
		})
		if err != nil {
			return err
		}
		r.record = &recorded
		return appendEvent(writeCtx, tx, r.task.ID, &r.attempt.ID, event.TypeWorktreeCreated, map[string]any{
			"path":           r.worktree.Path,
			"branch":         branch,
			"base_commit":    r.worktree.BaseCommit,
			"continued_from": r.retry.previous,
		})
	}); err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			// The branch was created for an attempt that no longer exists;
			// leaving it would put an empty branch beside the task's own and
			// make the next attempt's name collide. The worktree detaches to
			// the same commit first, so nothing is lost.
			cleanupCtx, cancelCleanup := writeContext(ctx)
			defer cancelCleanup()
			if delErr := r.worktree.AbandonBranch(cleanupCtx, branch); delErr != nil {
				r.log.WarnContext(ctx, "could not delete the branch of an abandoned retry", "branch", branch, "error", delErr.Error())
			}
		}
		return err
	}

	shared, err := r.worktree.SnapshotSharedState(ctx)
	if err != nil {
		return fmt.Errorf("snapshot shared repository state: %w", err)
	}
	r.sharedBefore = shared
	return nil
}

// prompt is the instruction for the current attempt: the task on a first
// attempt, why the last one failed on a retry.
func (r *run) prompt() (string, error) {
	if r.retry == nil {
		return buildPrompt(r.task)
	}
	return buildRetryPrompt(r.task, r.attempt.AttemptNumber, r.retry)
}

// resetForAttempt clears what belonged to the attempt that just failed, so
// nothing of it leaks into the next one's records or outcome.
func (r *run) resetForAttempt() {
	r.record = nil
	r.workerRun = nil
	r.report = nil
	r.testsModified = nil
	r.agentTree = ""
	r.agentHead = ""
	r.failureKind = task.FailureNone
	r.retryable = false
	r.retryPending = false
}

// retryPromptData is what the retry template is rendered with.
type retryPromptData struct {
	Ref          string
	Attempt      int
	Reason       string
	Error        string
	FailedSteps  []failedStep
	Verification []string
	// Resumed says the agent's session is being continued, so it already
	// has the task; otherwise the full task prompt comes first.
	Resumed bool
	Task    string
}

// buildRetryPrompt renders the instruction for an attempt after the first.
func buildRetryPrompt(t task.Task, attempt int, rc *retryContext) (string, error) {
	data := retryPromptData{
		Ref:         t.Identifier(),
		Attempt:     attempt,
		Reason:      retryReason(rc.kind),
		FailedSteps: rc.failed,
		Resumed:     rc.sessionID != "",
	}
	// The verification failure is shown step by step; repeating its summary
	// as the error would only say the same thing twice.
	if len(rc.failed) == 0 {
		data.Error = strings.TrimSpace(rc.message)
	}
	for _, step := range t.Verification {
		data.Verification = append(data.Verification, step.String())
	}
	if !data.Resumed {
		full, err := buildPrompt(t)
		if err != nil {
			return "", err
		}
		data.Task = full
	}

	tmpl, err := template.New(prompts.RetryTask).
		Option("missingkey=error").
		ParseFS(prompts.FS, prompts.RetryTask)
	if err != nil {
		return "", fmt.Errorf("parse retry prompt template: %w", err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render retry prompt for %s: %w", t.Identifier(), err)
	}
	prompt := strings.TrimSpace(out.String())
	if prompt == "" {
		return "", fmt.Errorf("rendered retry prompt for %s is empty", t.Identifier())
	}
	return prompt, nil
}
