package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// TASK-000026 in miniature. A real Codex run could not install pytest, wrote its
// own pytest.py, and `python3 -m pytest` — aidev's verification — then ran the
// agent's file. That run disclosed it, and the file happened to work. aidev must
// not depend on either: a judge the agent wrote is not independent evidence,
// however good the work is.

const honestCheck = "#!/bin/sh\ntest -f marker.txt\n"

// selfApproval exits 0 and leaves a trace, so a test can tell whether aidev ran it.
const selfApproval = "#!/bin/sh\ntouch ran-by-verification\nexit 0\n"

// commitCheckScript makes check.sh part of the base commit the agent starts from.
func commitCheckScript(h *harness) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.repoPath, "check.sh"), []byte(honestCheck), 0o755); err != nil {
		h.t.Fatal(err)
	}
	gitRun(h.t, h.repoPath, "add", "check.sh")
	gitRun(h.t, h.repoPath, "commit", "-qm", "add check.sh")
}

func verifyWith(command string) func(*worker.CreateTaskInput) {
	return func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: command}}
	}
}

// assertIntercepted checks the outcome every interception must have: failed as
// a verification failure, the reason naming what was replaced, the replaced
// runner never executed, and the worktree kept for a human to look at.
func assertIntercepted(t *testing.T, h *harness, created task.Task, outcome worker.Outcome, mentions ...string) {
	t.Helper()

	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: verification the agent could rewrite decided the outcome", outcome.Task.Status)
	}

	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.FailureKind != task.FailureVerification {
		t.Errorf("failure kind = %s, want VERIFICATION", attempt.FailureKind)
	}
	for _, m := range mentions {
		if !strings.Contains(attempt.Error, m) {
			t.Errorf("failure reason %q does not mention %q: a reviewer needs to know what was replaced", attempt.Error, m)
		}
	}

	call, _ := h.backend.LastCall()
	if _, err := os.Stat(filepath.Join(call.WorkingDir, "ran-by-verification")); err == nil {
		t.Error("aidev executed the runner the agent replaced")
	}

	runs, err := h.store.ListVerificationRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Status == task.VerificationPassed {
			t.Errorf("verification run %+v is recorded as PASSED", r)
		}
	}

	history := h.eventTypes(created.ID)
	if a, b := indexOf(history, "task.verification_intercepted"), indexOf(history, "task.failed"); a < 0 || b < 0 || a > b {
		t.Errorf("history = %v, want task.verification_intercepted recorded before task.failed", history)
	}
	if contains(history, "task.verification_step_completed") {
		t.Errorf("history = %v: a verification step ran after interception", history)
	}

	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED so the replacement can be inspected", wt.Status)
	}
}

// The check must not cost honest work anything: a script the agent left alone
// still decides the task.
func TestUntouchedVerificationScriptStillDecides(t *testing.T) {
	h := newHarness(t, nil)
	commitCheckScript(h)
	h.backend.Work = doTheWork

	created := h.createTask(verifyWith("./check.sh"))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: the agent did the work and did not touch check.sh", outcome.Task.Status)
	}
	if contains(h.eventTypes(created.ID), "task.verification_intercepted") {
		t.Error("an untouched verification script was reported as intercepted")
	}
}

// Doing the work does not redeem replacing the judge. Otherwise the check would
// only catch agents whose work is also wrong, which verification catches anyway.
func TestReplacingTheVerificationScriptFailsEvenWhenTheWorkIsDone(t *testing.T) {
	h := newHarness(t, nil)
	commitCheckScript(h)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.WorkingDir, "check.sh"), []byte(selfApproval), 0o755)
	}

	created := h.createTask(verifyWith("./check.sh"))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	assertIntercepted(t, h, created, outcome, "step 1 `./check.sh`", "check.sh")
}

// Committing inside the worktree makes `git status` clean and a diff against the
// index empty. The prompt asks agents not to commit; this is what happens when
// one does anyway.
func TestReplacementIsCaughtWhenTheAgentCommitsIt(t *testing.T) {
	h := newHarness(t, nil)
	commitCheckScript(h)
	h.backend.Work = func(_ context.Context, req agent.Request) error {
		if err := os.WriteFile(filepath.Join(req.WorkingDir, "check.sh"), []byte(selfApproval), 0o755); err != nil {
			return err
		}
		gitRun(t, req.WorkingDir, "add", "check.sh")
		gitRun(t, req.WorkingDir, "commit", "-qm", "make the check pass")
		return nil
	}

	created := h.createTask(verifyWith("./check.sh"))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	assertIntercepted(t, h, created, outcome, "check.sh")
}

// A runner placed under a .gitignore rule is invisible to `git add -N .` and to
// status, which is where it would be put to go unnoticed.
func TestReplacementIsCaughtWhenGitIgnoresIt(t *testing.T) {
	h := newHarness(t, nil)
	writeFile(t, filepath.Join(h.repoPath, ".gitignore"), "gen/\n")
	gitRun(t, h.repoPath, "add", ".gitignore")
	gitRun(t, h.repoPath, "commit", "-qm", "ignore generated files")
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		dir := filepath.Join(req.WorkingDir, "gen")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "check.sh"), []byte(selfApproval), 0o755)
	}

	created := h.createTask(verifyWith("./gen/check.sh"))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	assertIntercepted(t, h, created, outcome, "./gen/check.sh", "gen/")
}
