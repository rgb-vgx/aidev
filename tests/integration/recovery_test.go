package integration

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"aidev/internal/git"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// crashDuringRun leaves the database in the state a `kill -9` would: the task
// RUNNING, an attempt open, and a worktree on disk recorded as ACTIVE. No
// orchestrator is involved, because the point is that nothing ran to clean up.
func crashDuringRun(t *testing.T, h *harness) (task.Task, task.TaskAttempt, task.Worktree) {
	t.Helper()

	created := h.createTask(nil)

	if err := h.store.TransitionTask(h.ctx, created.ID, task.StatusPending, task.StatusReady); err != nil {
		t.Fatal(err)
	}
	if err := h.store.TransitionTask(h.ctx, created.ID, task.StatusReady, task.StatusRunning); err != nil {
		t.Fatal(err)
	}
	attempt, err := h.store.CreateAttempt(h.ctx, task.NewAttempt(created.ID, 1))
	if err != nil {
		t.Fatal(err)
	}

	repo, err := h.git.OpenRepository(h.ctx, h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := h.git.Create(h.ctx, git.CreateRequest{
		Repository: repo,
		Name:       created.Ref + "-a1",
		Branch:     "aidev/" + created.Ref,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Work in progress, as an agent would have left it.
	if err := os.WriteFile(filepath.Join(wt.Path, "in-progress.txt"), []byte("half\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	record, err := h.store.CreateWorktree(h.ctx, task.Worktree{
		ID:         uuid.Must(uuid.NewV7()),
		AttemptID:  attempt.ID,
		Path:       wt.Path,
		Branch:     wt.Branch,
		BaseCommit: wt.BaseCommit,
		Status:     task.WorktreeActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	created.Status = task.StatusRunning
	return created, attempt, record
}

// A process killed mid-run leaves a task RUNNING, which no further run can move.
// There has to be a way back, or the task is stuck forever and its worktree is
// unaccounted for. This test is the documented recovery procedure, executed.
func TestRecoveryFromAnInterruptedRun(t *testing.T) {
	h := newHarness(t, nil)
	stuck, attempt, record := crashDuringRun(t, h)

	// The state an operator would find.
	stdout, _, err := h.runCLI(t, "task", "list", "--status", "RUNNING")
	if err != nil {
		t.Fatalf("task list: %v", err)
	}
	if !strings.Contains(stdout, stuck.Ref) {
		t.Fatalf("a stuck task is not discoverable by status:\n%s", stdout)
	}

	// Running it again must not be allowed: aidev cannot know whether another
	// process is still working on it.
	_, err = h.orchestrator.RunTask(h.ctx, stuck.Ref)
	if !errors.Is(err, worker.ErrNotRunnable) {
		t.Errorf("re-running a stuck task = %v, want ErrNotRunnable", err)
	}

	// Cancelling is the way back, and it must work with no process to signal.
	stdout, _, err = h.runCLI(t, "task", "cancel", stuck.Ref, "--reason", "aidev was killed mid-run")
	if err != nil {
		t.Fatalf("task cancel on a stuck task: %v", err)
	}
	if !strings.Contains(stdout, "cancelled") {
		t.Errorf("output = %q", stdout)
	}

	reloaded, err := h.store.GetTask(h.ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusCancelled {
		t.Fatalf("status = %s, want CANCELLED", reloaded.Status)
	}

	// The open attempt must be closed too, or it stays RUNNING forever and the
	// task's history never makes sense.
	reloadedAttempt, err := h.store.GetAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedAttempt.Status != task.AttemptCancelled {
		t.Errorf("attempt status = %s, want CANCELLED", reloadedAttempt.Status)
	}
	if reloadedAttempt.FinishedAt == nil {
		t.Error("the attempt has no finish time, so it still looks open")
	}

	// The interrupted work is kept, not discarded.
	reloadedWorktree, err := h.store.GetWorktreeByAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedWorktree.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED", reloadedWorktree.Status)
	}
	if _, err := os.Stat(filepath.Join(record.Path, "in-progress.txt")); err != nil {
		t.Errorf("the interrupted work was destroyed: %v", err)
	}

	// And it is then reclaimable, which closes the loop.
	stdout, _, err = h.runCLI(t, "worktree", "list")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if !strings.Contains(stdout, stuck.Ref) || !strings.Contains(stdout, "RETAINED") {
		t.Errorf("the recovered worktree is not listed:\n%s", stdout)
	}
	if _, _, err := h.runCLI(t, "worktree", "remove", stuck.Ref, "--force"); err != nil {
		t.Fatalf("worktree remove after recovery: %v", err)
	}
	if _, err := os.Stat(record.Path); !os.IsNotExist(err) {
		t.Error("the worktree was not removed")
	}
}

// The same recovery must be available through MCP, since a planner may be the only
// thing watching.
func TestRecoveryThroughMCP(t *testing.T) {
	m := newMCPHarness(t)
	stuck, _, _ := crashDuringRun(t, m.harness)

	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	m.call(t, "aidev_list_tasks", map[string]any{"statuses": []string{"RUNNING"}}, &list)
	var found bool
	for _, entry := range list.Tasks {
		if entry["ref"] == stuck.Ref {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stuck task is not listable through MCP: %+v", list.Tasks)
	}

	var cancelled struct {
		Task    map[string]any `json:"task"`
		Message string         `json:"message"`
	}
	m.call(t, "aidev_cancel_task", map[string]any{
		"task": stuck.Ref, "reason": "aidev was killed mid-run",
	}, &cancelled)
	if cancelled.Task["status"] != "CANCELLED" {
		t.Errorf("status = %v, want CANCELLED", cancelled.Task["status"])
	}
	if !strings.Contains(cancelled.Message, "kept") {
		t.Errorf("message = %q, want it to say the worktree was kept", cancelled.Message)
	}
}

// Cancelling must also close an attempt that a crash left open even when the task
// itself was already moved on by something else, so the two cannot disagree.
func TestCancelClosesEveryOpenAttempt(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)

	if err := h.store.TransitionTask(h.ctx, created.ID, task.StatusPending, task.StatusReady); err != nil {
		t.Fatal(err)
	}
	first, err := h.store.CreateAttempt(h.ctx, task.NewAttempt(created.ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.store.CreateAttempt(h.ctx, task.NewAttempt(created.ID, 2))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.orchestrator.Cancel(h.ctx, created.Ref, "cleanup"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	for _, id := range []uuid.UUID{first.ID, second.ID} {
		attempt, err := h.store.GetAttempt(h.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if attempt.Status != task.AttemptCancelled {
			t.Errorf("attempt %d status = %s, want CANCELLED", attempt.AttemptNumber, attempt.Status)
		}
	}
}
