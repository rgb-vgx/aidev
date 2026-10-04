package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
)

// gitInWorktree runs git the way an agent would: plain git, inside the
// worktree, with none of aidev's own flags.
func gitInWorktree(req agent.Request, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = req.WorkingDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return &gitError{args: strings.Join(args, " "), err: err, out: string(out)}
	}
	return nil
}

type gitError struct {
	args string
	err  error
	out  string
}

func (e *gitError) Error() string {
	return "git " + e.args + ": " + e.err.Error() + "\n" + e.out
}

// workAndTamper does the work verification requires and then reaches outside
// the worktree, so that "the work would have passed" and "the task must fail
// anyway" are both true at once.
func workAndTamper(t *testing.T, tamper func(req agent.Request) error) func(context.Context, agent.Request) error {
	t.Helper()
	return func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		return tamper(req)
	}
}

// An agent that writes the shared config must fail the attempt as
// CONTAINMENT: the write is a delayed command-execution channel into every
// later git command in the repository (docs/research.md §7i), so it is a
// breach even when the task's own work is perfect.
func TestWritingSharedConfigFailsContainment(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = workAndTamper(t, func(req agent.Request) error {
		return gitInWorktree(req, "config", "aidev.probe", "shared")
	})

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: %s", outcome.Task.Status, outcome.Message)
	}

	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.FailureKind != task.FailureContainment {
		t.Errorf("failure kind = %s, want CONTAINMENT", attempt.FailureKind)
	}

	history := h.eventTypes(created.ID)
	if !contains(history, "task.containment_breach") {
		t.Errorf("history = %v, want a containment_breach detail event", history)
	}

	// The agent finished its work: the run is recorded as the success it was.
	workerRuns, err := h.store.ListWorkerRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workerRuns) != 1 || workerRuns[0].Status != task.WorkerSucceeded {
		t.Errorf("worker run = %+v, want the agent's own run recorded as SUCCEEDED", workerRuns)
	}

	// Containment is checked before verification: marker.txt exists, so every
	// verification command would pass, and none of them may run against a
	// tampered workspace.
	runs, err := h.store.ListVerificationRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("verification ran %d step(s) after a containment breach, want 0", len(runs))
	}

	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED: the breach must be inspectable", wt.Status)
	}
}

// A planted hook in the shared hooks directory is the same class of breach:
// it does not run for aidev's own commands (layer 1 pins hooksPath to
// /dev/null), but it is there for the operator's next plain git command.
func TestPlantingASharedHookFailsContainment(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = workAndTamper(t, func(req agent.Request) error {
		path := filepath.Join(h.repoPath, ".git", "hooks", "post-commit")
		return os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	})

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: %s", outcome.Task.Status, outcome.Message)
	}

	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.FailureKind != task.FailureContainment {
		t.Errorf("failure kind = %s, want CONTAINMENT", attempt.FailureKind)
	}

	runs, err := h.store.ListVerificationRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("verification ran %d step(s) after a planted hook, want 0", len(runs))
	}
}

// Detaching HEAD leaves the task's branch behind; the attempt must fail rather
// than verify against whatever HEAD now points at.
func TestDetachingHEADFailsContainment(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = workAndTamper(t, func(req agent.Request) error {
		return gitInWorktree(req, "checkout", "--detach")
	})

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: %s", outcome.Task.Status, outcome.Message)
	}

	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.FailureKind != task.FailureContainment {
		t.Errorf("failure kind = %s, want CONTAINMENT", attempt.FailureKind)
	}
	if history := h.eventTypes(created.ID); !contains(history, "task.containment_breach") {
		t.Errorf("history = %v, want a containment_breach detail event", history)
	}
}

// A ref outside the task's own branch moving is warned about but does not fail
// the attempt: another task in the same repository advancing its own branch is
// indistinguishable from the agent doing it, and the concurrent task is the
// common case. The work still has to pass verification.
func TestForeignRefChangeWarnsWithoutFailing(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = workAndTamper(t, func(req agent.Request) error {
		return gitInWorktree(req, "tag", "probe-foreign")
	})

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: a foreign ref is a warning, not a verdict: %s",
			outcome.Task.Status, outcome.Message)
	}

	history := h.eventTypes(created.ID)
	if !contains(history, "task.shared_refs_changed") {
		t.Errorf("history = %v, want a shared_refs_changed warning", history)
	}
	if contains(history, "task.containment_breach") {
		t.Errorf("history = %v, want no containment_breach for a foreign ref alone", history)
	}
}
