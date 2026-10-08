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

// The agent does not commit. The prompt tells it not to ("The work is
// committed for you once verification passes"), and aidev commits the work
// itself: on success it snapshots the worktree — tracked changes and every new
// file git does not ignore — and commits that tree to the task's branch. In
// clean mode the checks run on a checkout of that same snapshot.
//
// TASK-000093 (2026-10-08) looked like a counter-example: the worker run said
// SUCCEEDED with "5 file(s) changed", the worktree's head was still the base
// commit, and every new file was untracked. That is the normal state of an
// attempt aidev has not yet committed; the attempt failed for another reason
// (ignored files under a protected path, fixed separately). This test pins
// the shape so it stays the normal state: new untracked files only, no
// commit, clean mode, checks that need the new files.
func TestUncommittedNewFilesAreVerifiedAndCommittedInCleanMode(t *testing.T) {
	h := newHarness(t, nil)
	commitIgnoreRules(h)
	check := "#!/bin/sh\ntest -f src/addon/main.js && test -f scripts/install.py && test ! -e tests/out\n"
	if err := os.WriteFile(filepath.Join(h.repoPath, "check.sh"), []byte(check), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, h.repoPath, "add", "check.sh")
	gitRun(t, h.repoPath, "commit", "-qm", "a check that needs the new files")

	h.backend.Work = func(_ context.Context, req agent.Request) error {
		writeUnder(t, req.WorkingDir, "src/addon/main.js", "console.log('loaded')\n")
		writeUnder(t, req.WorkingDir, "src/addon/ribbon.xml", "<customUI/>\n")
		writeUnder(t, req.WorkingDir, "scripts/install.py", "print('install')\n")
		// What running the tests leaves behind: ignored, never in the commit.
		writeUnder(t, req.WorkingDir, "tests/out/report.json", "{}")
		return nil // and no commit
	}

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.VerificationMode = string(task.VerificationClean)
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s (%s), want SUCCEEDED: the clean checkout carries untracked new files", outcome.Task.Status, outcome.Message)
	}

	// The success commit holds the agent's files, and not the ignored output.
	branch := "aidev/" + created.Ref
	files := gitRun(t, h.repoPath, "ls-tree", "-r", "--name-only", branch)
	for _, want := range []string{"src/addon/main.js", "src/addon/ribbon.xml", "scripts/install.py"} {
		if !strings.Contains(files, want) {
			t.Errorf("the commit on %s lacks %s:\n%s", branch, want, files)
		}
	}
	if strings.Contains(files, "tests/out") {
		t.Errorf("the commit carries ignored test output:\n%s", files)
	}
	if outcome.Worktree == nil || outcome.Worktree.HeadCommit == outcome.Worktree.BaseCommit {
		t.Errorf("worktree = %+v, want a head commit past the base", outcome.Worktree)
	}
}

// When such an attempt fails, the result must not read as "nothing was
// saved": the branch still points at the base, and the reader is told why —
// the work is in the worktree, uncommitted, because aidev commits only what
// passed verification.
func TestAFailedResultSaysTheWorkIsUncommittedInTheWorktree(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(_ context.Context, req agent.Request) error {
		writeUnder(t, req.WorkingDir, "src/new.go", "package src\n")
		return nil // the default check wants marker.txt, so this fails
	}
	created := h.createTask(nil)
	if out, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil || out.Task.Status != task.StatusFailed {
		t.Fatalf("RunTask = %v, %v; want FAILED", out.Task.Status, err)
	}

	text, stderr, err := h.runCLI(t, "task", "result", created.Ref)
	if err != nil {
		t.Fatalf("task result: %v\n%s", err, stderr)
	}
	for _, want := range []string{"uncommitted", "commits", "verification"} {
		if !strings.Contains(text, want) {
			t.Errorf("task result does not explain where the work is (%q missing):\n%s", want, text)
		}
	}
}
