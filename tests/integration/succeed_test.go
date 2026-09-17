package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// A task succeeds by delivering its work on its branch. When the commit that puts
// it there fails, nothing was delivered: the task must not be recorded as
// SUCCEEDED, and the work must stay where it is.
//
// The failure is a leftover index.lock in the worktree's administrative directory,
// which is what a git process that crashed or was killed leaves behind. Hooks
// cannot be used: aidev commits with --no-verify.
func TestAFailedCommitIsNotASuccess(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		out, err := exec.Command("git", "-C", req.WorkingDir, "rev-parse", "--absolute-git-dir").Output()
		if err != nil {
			return err
		}
		lock := filepath.Join(strings.TrimSpace(string(out)), "index.lock")
		return os.WriteFile(lock, nil, 0o600)
	}

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status == task.StatusSucceeded {
		t.Fatalf("status = SUCCEEDED although the work was never committed: %s", outcome.Message)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: %s", outcome.Task.Status, outcome.Message)
	}
	if strings.Contains(outcome.Message, "committed on") {
		t.Errorf("message = %q, it claims a commit that does not exist", outcome.Message)
	}

	reloaded, err := h.store.GetTask(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusFailed {
		t.Errorf("persisted status = %s, want FAILED", reloaded.Status)
	}

	// Verification passed, so this is not the agent's fault or a verification
	// failure: it is the worktree that could not deliver.
	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != task.AttemptFailed || attempt.FailureKind != task.FailureWorktree {
		t.Errorf("attempt = %s/%s, want FAILED/WORKTREE", attempt.Status, attempt.FailureKind)
	}
	if !strings.Contains(attempt.Error, "commit") {
		t.Errorf("attempt error = %q, want it to say the commit failed", attempt.Error)
	}

	// The work is kept, and recorded as kept.
	call, _ := h.backend.LastCall()
	if _, err := os.Stat(filepath.Join(call.WorkingDir, "marker.txt")); err != nil {
		t.Errorf("the uncommitted work is gone: %v", err)
	}
	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED", wt.Status)
	}

	if strings.Contains(h.branchFiles("aidev/"+created.Ref), "marker.txt") {
		t.Error("the branch holds the work, so the commit did not fail and the test proves nothing")
	}

	history := h.eventTypes(created.ID)
	if contains(history, "task.succeeded") {
		t.Errorf("history records a success: %v", history)
	}
	if !contains(history, "task.failed") {
		t.Errorf("history is missing task.failed: %v", history)
	}
	if contains(history, "task.worktree_removed") {
		t.Errorf("history records a removal: %v", history)
	}
}

// A task cancelled while its verification runs is CANCELLED, and Cancel promises
// that the worktree is kept. Verification passing afterwards must not undo that:
// the run may not remove the worktree, and may not mark the record REMOVED, when
// the task is no longer its to finish.
//
// The run's own context is not cancelled here — Cancel only changes the database —
// so this is exactly the path where the move to SUCCEEDED loses its
// compare-and-swap after the success path has already acted.
func TestCancelDuringVerificationKeepsTheWorktree(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{
			{Command: "sh", Args: []string{"-c", "sleep 2 && test -f marker.txt"}},
		}
	})

	cancelled := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			current, err := h.store.GetTask(h.ctx, created.ID)
			if err != nil {
				cancelled <- err
				return
			}
			if current.Status == task.StatusVerifying {
				_, err := h.orchestrator.Cancel(h.ctx, created.Ref, "cancelled during verification")
				cancelled <- err
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancelled <- context.DeadlineExceeded
	}()

	outcome, runErr := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err := <-cancelled; err != nil {
		t.Fatalf("Cancel during verification: %v", err)
	}
	// The task ended, and its end is recorded: that is an outcome, as every other
	// recorded ending is, not an error from RunTask. `aidev task run` would
	// otherwise exit 1 for a cancellation someone asked for.
	if runErr != nil {
		t.Errorf("RunTask: %v; a task cancelled by someone else is an outcome, not an error", runErr)
	}
	if outcome.Task.Status != task.StatusCancelled {
		t.Errorf("the run reported %s for a task that was cancelled: %s", outcome.Task.Status, outcome.Message)
	}

	reloaded, err := h.store.GetTask(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusCancelled {
		t.Fatalf("persisted status = %s, want CANCELLED", reloaded.Status)
	}

	call, ok := h.backend.LastCall()
	if !ok {
		t.Fatal("the backend was never called")
	}
	if _, err := os.Stat(call.WorkingDir); err != nil {
		t.Errorf("the worktree of a cancelled task was removed: %v", err)
	}

	attempts, err := h.store.ListAttempts(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("got %d attempts, want 1", len(attempts))
	}
	wt, err := h.store.GetWorktreeByAttempt(h.ctx, attempts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED as Cancel promised", wt.Status)
	}

	history := h.eventTypes(created.ID)
	if contains(history, "task.succeeded") || contains(history, "task.worktree_removed") {
		t.Errorf("history records a success path for a cancelled task: %v", history)
	}
}
