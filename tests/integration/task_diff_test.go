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

// `aidev task diff <task>` shows what a task changed, the first thing a
// reviewer reads: for a delivered task, the diff from its base commit to the
// commit on its branch; for an attempt that delivered nothing, the change
// its agent left uncommitted in the worktree, marked as not delivered.

func TestTaskDiffShowsTheDeliveredChange(t *testing.T) {
	h := newHarness(t, nil)
	created := succeededTask(t, h)

	stdout, stderr, err := h.runCLI(t, "task", "diff", created.Ref)
	if err != nil {
		t.Fatalf("task diff: %v\n%s", err, stderr)
	}
	for _, want := range []string{"diff --git a/marker.txt b/marker.txt", "+++ b/marker.txt", "+done"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("task diff output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "not delivered") {
		t.Errorf("a delivered change is labelled as not delivered:\n%s", stdout)
	}
}

func TestTaskDiffOfAFailedAttemptShowsItsUndeliveredChange(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		return os.WriteFile(filepath.Join(req.WorkingDir, "partial.txt"), []byte("half done\n"), 0o600)
	}
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "test", Args: []string{"-f", "marker.txt"}}}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || outcome.Task.Status != task.StatusFailed {
		t.Fatalf("RunTask = %v, %v; want FAILED", outcome.Task.Status, err)
	}

	stdout, stderr, err := h.runCLI(t, "task", "diff", created.Ref)
	if err != nil {
		t.Fatalf("task diff: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "not delivered") {
		t.Errorf("task diff of a failed attempt does not say the change was not delivered:\n%s", stdout)
	}
	if !strings.Contains(stdout, "partial.txt") || !strings.Contains(stdout, "+half done") {
		t.Errorf("task diff of a failed attempt lacks the agent's change:\n%s", stdout)
	}
}

func TestTaskDiffOfATaskThatNeverRan(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)

	_, _, err := h.runCLI(t, "task", "diff", created.Ref)
	if err == nil || !strings.Contains(err.Error(), "has not run") {
		t.Errorf("task diff of a task that never ran = %v, want an error saying it has not run", err)
	}
}
