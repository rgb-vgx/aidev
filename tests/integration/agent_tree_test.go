package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Research A6: the success commit carries the tree snapshotted when the agent
// finished, not the working directory as verification left it. Before this,
// the commit ran `git add --all` after the checks, so anything they wrote that
// .gitignore did not cover — coverage output, build products, a regenerated
// fixture — was delivered on the branch as if the agent had written it.

// shell is a verification step run through sh, so a test can make the checks
// write files the way real test runners do.
func shell(script string) task.VerificationStep {
	return task.VerificationStep{Command: "sh", Args: []string{"-c", script}}
}

// passesAndWrites is a passing check that also leaves an output file behind,
// the report's own example: cover.out, not ignored by the repository.
func passesAndWrites(in *worker.CreateTaskInput) {
	in.Verification = []task.VerificationStep{shell("test -f marker.txt && echo 'mode: set' > cover.out")}
}

func TestCommitLeavesOutWhatVerificationWrote(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(passesAndWrites)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: %s", outcome.Task.Status, outcome.Message)
	}

	branch := "aidev/" + created.Ref
	files := h.branchFiles(branch)
	if !strings.Contains(files, "marker.txt") {
		t.Errorf("the branch lacks the agent's marker.txt:\n%s", files)
	}
	if strings.Contains(files, "cover.out") {
		t.Errorf("the branch carries cover.out, which only verification wrote:\n%s", files)
	}

	// The tree on the branch is the tree recorded for the worktree, so a
	// later reader can tell what was committed without trusting the branch.
	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.AgentTree == "" {
		t.Fatal("worktree.agent_tree was not recorded")
	}
	if got := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", branch+"^{tree}")); got != wt.AgentTree {
		t.Errorf("branch tree = %s, want the recorded agent tree %s", got, wt.AgentTree)
	}

	// The reviewer is told: what passed is not exactly what was committed.
	payload := h.eventPayload(created.ID, "task.verification_worktree_modified")
	if payload == nil {
		t.Fatal("no task.verification_worktree_modified event although verification wrote cover.out")
	}
	if !driftNames(payload, "A", "cover.out") {
		t.Errorf("event paths = %v, want cover.out added", payload["paths"])
	}
	if payload["agent_tree"] != wt.AgentTree {
		t.Errorf("event agent_tree = %v, want %s", payload["agent_tree"], wt.AgentTree)
	}

	// The left-out file makes the worktree dirty, and keep-on-dirty removal
	// keeps it rather than discarding something nobody committed.
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED: it holds a file the commit left out", wt.Status)
	}
}

// A check that rewrites a tracked file — a formatter, a code generator — is
// the case the warning exists for: the checks passed against content the
// branch does not receive.
func TestVerificationEditingATrackedFileIsReported(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{shell("test -f marker.txt && echo '// formatted' >> main.go")}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: the warning reports, it does not decide", outcome.Task.Status)
	}

	payload := h.eventPayload(created.ID, "task.verification_worktree_modified")
	if payload == nil || !driftNames(payload, "M", "main.go") {
		t.Fatalf("event = %v, want main.go reported modified", payload)
	}
	if total, _ := payload["total"].(float64); total != 1 {
		t.Errorf("event total = %v, want 1", payload["total"])
	}

	// The branch has main.go as the agent left it.
	branchMain := gitRun(t, h.repoPath, "show", "aidev/"+created.Ref+":main.go")
	if strings.Contains(branchMain, "formatted") {
		t.Errorf("the branch carries verification's edit to main.go:\n%s", branchMain)
	}

	// Reported inside the verification phase, before the verdict.
	history := h.eventTypes(created.ID)
	a, b := indexOf(history, "task.verification_worktree_modified"), indexOf(history, "task.verification_completed")
	if a < 0 || b < 0 || a > b {
		t.Errorf("history = %v, want the warning before task.verification_completed", history)
	}
}

// Silence is the common case and must stay silent: checks that write
// nothing produce no warning, and the delivered worktree is still clean
// enough to remove — the snapshot commit must not leave the index stale.
func TestNoDriftWarningWhenVerificationWritesNothing(t *testing.T) {
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
	if contains(h.eventTypes(created.ID), "task.verification_worktree_modified") {
		t.Error("a drift warning was recorded although verification wrote nothing")
	}
	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRemoved {
		t.Errorf("worktree status = %s, want REMOVED: a delivered worktree that matches its commit is clean", wt.Status)
	}
}

// In clean mode the checks run in a detached checkout of the snapshot, so
// whatever they write never touches the agent's worktree: no warning, and
// the commit is the snapshot that was checked.
func TestNoDriftWarningInCleanMode(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		passesAndWrites(in)
		in.VerificationMode = "clean"
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: %s", outcome.Task.Status, outcome.Message)
	}
	if contains(h.eventTypes(created.ID), "task.verification_worktree_modified") {
		t.Error("a drift warning was recorded although clean checks never touch the agent's worktree")
	}
	if strings.Contains(h.branchFiles("aidev/"+created.Ref), "cover.out") {
		t.Error("the branch carries cover.out from a clean verification")
	}
}

// An agent may commit its own work on the task branch. The snapshot is then
// taken on the agent's commit, and the success commit stacks the remaining
// uncommitted work on top of it — the agent's history is kept, not replaced.
func TestSnapshotBuildsOnTheAgentsOwnCommit(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		gitRun(t, req.WorkingDir, "add", "marker.txt")
		gitRun(t, req.WorkingDir, "-c", "user.name=Agent", "-c", "user.email=agent@example.com",
			"commit", "-qm", "agent: add the marker")
		writeFile(t, filepath.Join(req.WorkingDir, "notes.txt"), "left uncommitted\n")
		return nil
	}

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: %s", outcome.Task.Status, outcome.Message)
	}

	branch := "aidev/" + created.Ref
	files := h.branchFiles(branch)
	for _, want := range []string{"marker.txt", "notes.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("the branch lacks %s:\n%s", want, files)
		}
	}
	parent := strings.TrimSpace(gitRun(t, h.repoPath, "log", "-1", "--format=%s", branch+"^"))
	if parent != "agent: add the marker" {
		t.Errorf("the success commit's parent is %q, want the agent's own commit", parent)
	}
}

// An agent that changed nothing still succeeds once verification passes,
// with no commit — the snapshot equal to the branch's tree means there is
// nothing to deliver.
func TestNothingToCommitWhenTheSnapshotMatchesTheBranch(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(context.Context, agent.Request) error { return nil }

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "true"}}
	})
	before := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", "main"))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED", outcome.Task.Status)
	}
	if !strings.Contains(outcome.Message, "no changes to commit") {
		t.Errorf("message = %q, want it to say there was nothing to commit", outcome.Message)
	}
	if got := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", "aidev/"+created.Ref)); got != before {
		t.Errorf("branch = %s, want it still at the base %s", got, before)
	}
}

// driftNames reports whether a drift event lists path with status.
func driftNames(payload map[string]any, status, path string) bool {
	entries, _ := payload["paths"].([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if m["path"] == path && m["status"] == status {
			return true
		}
	}
	return false
}
