// Package verification runs a task's own test commands and decides whether the
// task actually succeeded.
//
// This is the package that makes an agent's claim of success irrelevant. Nothing
// here reads the agent's output or its summary; it runs the commands the task
// declared and reports their exit codes. The task lifecycle then allows SUCCEEDED
// only from VERIFYING, so this result is the only thing that can produce a
// successful task.
package verification

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aidev/internal/procexec"
	"aidev/internal/task"
)

// Runner executes verification steps.
type Runner struct {
	// DefaultTimeout bounds a step that does not specify its own.
	DefaultTimeout time.Duration

	// MaxOutputBytes bounds each captured stream per step.
	MaxOutputBytes int
}

// NewRunner returns a runner with the given bounds.
func NewRunner(defaultTimeout time.Duration, maxOutputBytes int) *Runner {
	return &Runner{DefaultTimeout: defaultTimeout, MaxOutputBytes: maxOutputBytes}
}

// Request is one verification pass over a worktree.
type Request struct {
	// AttemptID labels the resulting records.
	AttemptID uuid.UUID

	// WorkingDir is the worktree to verify. Verification runs in the same
	// isolated checkout the agent worked in, never in the main repository.
	WorkingDir string

	// Steps are the commands to run, in order.
	Steps []task.VerificationStep
}

// Report is the outcome of a verification pass.
type Report struct {
	// Passed is true only when every step ran and passed. It is false for an
	// empty step list, a failure, a timeout, or a cancellation.
	Passed bool

	// Runs holds one record per declared step, including steps that were
	// skipped, so that a reader can always account for every step.
	Runs []task.VerificationRun

	Duration time.Duration

	// FailureKind classifies why verification did not pass. It is
	// task.FailureNone when Passed is true.
	FailureKind task.FailureKind
}

// FirstFailure returns the first step that did not pass, if any.
func (r Report) FirstFailure() (task.VerificationRun, bool) {
	for _, run := range r.Runs {
		if run.Status != task.VerificationPassed && run.Status != task.VerificationSkipped {
			return run, true
		}
	}
	return task.VerificationRun{}, false
}

// Summary describes the outcome in one line, for logs and for the task result.
func (r Report) Summary() string {
	passed := 0
	for _, run := range r.Runs {
		if run.Status == task.VerificationPassed {
			passed++
		}
	}
	if r.Passed {
		return fmt.Sprintf("%d/%d verification steps passed", passed, len(r.Runs))
	}
	if failure, ok := r.FirstFailure(); ok {
		return fmt.Sprintf("%d/%d verification steps passed; %q %s",
			passed, len(r.Runs), failure.Command, describe(failure))
	}
	return fmt.Sprintf("%d/%d verification steps passed", passed, len(r.Runs))
}

func describe(run task.VerificationRun) string {
	switch run.Status {
	case task.VerificationFailed:
		if run.ExitCode != nil {
			return fmt.Sprintf("exited %d", *run.ExitCode)
		}
		return "failed"
	case task.VerificationTimedOut:
		return "timed out"
	case task.VerificationCancelled:
		return "was cancelled"
	default:
		return string(run.Status)
	}
}

// Run executes every step in order and stops at the first one that does not pass.
//
// Stopping early is deliberate: once one check has failed the task cannot
// succeed, and continuing would spend time on commands whose result cannot change
// the outcome. The remaining steps are still recorded, as SKIPPED, so that a
// result never has silent gaps that could be mistaken for passes.
func (r *Runner) Run(ctx context.Context, req Request) (Report, error) {
	report := Report{}
	start := time.Now()

	if req.WorkingDir == "" {
		return report, fmt.Errorf("verification: working directory is required")
	}
	if len(req.Steps) == 0 {
		// The domain refuses to create such a task and the database refuses to
		// store one, so reaching here means a bug. Failing loudly is better than
		// returning a pass for zero checks.
		return report, fmt.Errorf("verification: no steps defined, so nothing could be verified")
	}

	report.Runs = make([]task.VerificationRun, 0, len(req.Steps))
	stopped := false

	for i, step := range req.Steps {
		if stopped {
			report.Runs = append(report.Runs, r.skipped(req.AttemptID, i, step))
			continue
		}

		// A cancellation between steps must not look like a skip: the step was
		// never attempted because the caller stopped, not because an earlier
		// step failed.
		if err := ctx.Err(); err != nil {
			report.Runs = append(report.Runs, r.cancelled(req.AttemptID, i, step))
			stopped = true
			report.FailureKind = task.FailureCancelled
			continue
		}

		run, outcome := r.runStep(ctx, req, i, step)
		report.Runs = append(report.Runs, run)

		if run.Status != task.VerificationPassed {
			stopped = true
			report.FailureKind = failureKindFor(outcome)
		}
	}

	report.Duration = time.Since(start)
	report.Passed = report.FailureKind == task.FailureNone
	for _, run := range report.Runs {
		if run.Status != task.VerificationPassed {
			report.Passed = false
		}
	}
	if report.Passed {
		report.FailureKind = task.FailureNone
	} else if report.FailureKind == task.FailureNone {
		report.FailureKind = task.FailureVerification
	}
	return report, nil
}

func (r *Runner) runStep(ctx context.Context, req Request, index int, step task.VerificationStep) (task.VerificationRun, procexec.Outcome) {
	timeout := r.DefaultTimeout
	if step.TimeoutSeconds > 0 {
		timeout = time.Duration(step.TimeoutSeconds) * time.Second
	}

	proc, err := procexec.Run(ctx, procexec.Spec{
		Command:        step.Command,
		Args:           step.Args,
		Dir:            req.WorkingDir,
		Timeout:        timeout,
		MaxOutputBytes: r.MaxOutputBytes,
	})

	run := task.VerificationRun{
		ID:              uuid.Must(uuid.NewV7()),
		AttemptID:       req.AttemptID,
		StepIndex:       index,
		Command:         step.String(),
		ExitCode:        proc.ExitCode,
		Stdout:          proc.Stdout,
		StdoutTruncated: proc.StdoutTruncated,
		Stderr:          proc.Stderr,
		StderrTruncated: proc.StderrTruncated,
		StartedAt:       proc.StartedAt,
		Duration:        proc.Duration,
	}
	if !proc.FinishedAt.IsZero() {
		finished := proc.FinishedAt
		run.FinishedAt = &finished
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}

	switch proc.Outcome {
	case procexec.OutcomeSucceeded:
		run.Status = task.VerificationPassed
	case procexec.OutcomeTimedOut:
		run.Status = task.VerificationTimedOut
	case procexec.OutcomeCancelled:
		run.Status = task.VerificationCancelled
	case procexec.OutcomeStartFailed:
		// A command that cannot be started is a failed check, not an aidev
		// error: the task named a command that is not runnable here, and that is
		// a real verification failure the user needs to see.
		run.Status = task.VerificationFailed
		if run.Stderr == "" && err != nil {
			run.Stderr = err.Error()
		}
	default:
		run.Status = task.VerificationFailed
	}
	return run, proc.Outcome
}

func (r *Runner) skipped(attemptID uuid.UUID, index int, step task.VerificationStep) task.VerificationRun {
	now := time.Now().UTC()
	return task.VerificationRun{
		ID:        uuid.Must(uuid.NewV7()),
		AttemptID: attemptID,
		StepIndex: index,
		Command:   step.String(),
		Status:    task.VerificationSkipped,
		StartedAt: now,
	}
}

func (r *Runner) cancelled(attemptID uuid.UUID, index int, step task.VerificationStep) task.VerificationRun {
	run := r.skipped(attemptID, index, step)
	run.Status = task.VerificationCancelled
	return run
}

func failureKindFor(outcome procexec.Outcome) task.FailureKind {
	switch outcome {
	case procexec.OutcomeTimedOut:
		return task.FailureTimeout
	case procexec.OutcomeCancelled:
		return task.FailureCancelled
	default:
		return task.FailureVerification
	}
}
