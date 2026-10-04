package integration

import (
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/task"
	"aidev/internal/worker"
)

// Research D2: the base ref is resolved when the task runs, which can be long
// after it was created. The commit at creation is recorded, and a run that
// starts from a different one says so — in the event log, and in the result a
// reviewer reads — while still going ahead on the newer code.

func TestBaseRefMovedBetweenCreateAndRunIsReported(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	atCreate := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", "main"))
	created := h.createTask(nil)
	if created.BaseCommitAtCreate != atCreate {
		t.Fatalf("base_commit_at_create = %q, want main at creation %s", created.BaseCommitAtCreate, atCreate)
	}
	if got := h.eventPayload(created.ID, "task.created")["base_commit"]; got != atCreate {
		t.Errorf("task.created base_commit = %v, want %s", got, atCreate)
	}

	// Someone pushes to main before the task runs.
	writeFile(t, filepath.Join(h.repoPath, "later.txt"), "pushed after the task was created\n")
	gitRun(t, h.repoPath, "add", "later.txt")
	gitRun(t, h.repoPath, "commit", "-qm", "a later change")
	atRun := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", "main"))

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	// A warning, not a refusal: the run goes ahead on the current code.
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: a moved base is reported, not refused", outcome.Task.Status)
	}
	if outcome.Worktree == nil || outcome.Worktree.BaseCommit != atRun {
		t.Fatalf("worktree base = %+v, want the commit main pointed at when the run began (%s)", outcome.Worktree, atRun)
	}

	payload := h.eventPayload(created.ID, "task.base_moved")
	if payload == nil {
		t.Fatal("no task.base_moved event although main moved between create and run")
	}
	if payload["at_create"] != atCreate || payload["at_run"] != atRun || payload["base_ref"] != "main" {
		t.Errorf("task.base_moved payload = %v, want main %s -> %s", payload, atCreate, atRun)
	}

	// The stored result says so too, for a reader who never sees the run.
	stored, err := h.store.GetTask(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BaseCommitAtCreate != atCreate || wt.BaseCommit != atRun {
		t.Errorf("stored commits = %s / %s, want %s / %s", stored.BaseCommitAtCreate, wt.BaseCommit, atCreate, atRun)
	}
}

func TestNoBaseMovedEventWhenTheRefStayedPut(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED", outcome.Task.Status)
	}
	if contains(h.eventTypes(created.ID), "task.base_moved") {
		t.Error("task.base_moved was recorded although the base ref did not move")
	}
}

// An explicit base_ref is recorded the same way as the default branch.
func TestBaseCommitAtCreateFollowsAnExplicitRef(t *testing.T) {
	h := newHarness(t, nil)
	gitRun(t, h.repoPath, "branch", "release")
	writeFile(t, filepath.Join(h.repoPath, "on-main.txt"), "only on main\n")
	gitRun(t, h.repoPath, "add", "on-main.txt")
	gitRun(t, h.repoPath, "commit", "-qm", "main moves on")
	release := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", "release"))

	created := h.createTask(func(in *worker.CreateTaskInput) { in.BaseRef = "release" })
	if created.BaseCommitAtCreate != release {
		t.Errorf("base_commit_at_create = %q, want release's commit %s", created.BaseCommitAtCreate, release)
	}
}
