package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/task"
	"aidev/internal/worker"
)

// Tasks of one repository run side by side, and their checks shared one test
// database: on 2026-11-01 the agent of TASK-000166 applied a migration that
// was never merged to aidev_test, and the database stayed broken for every
// later run until it was recreated by hand. A check can keep its resources
// apart only if it knows which task it runs for, so aidev puts the task's
// reference in AIDEV_TASK_REF for the setup and verification commands (and
// for the agent, which runs the same commands while it works).

func TestSetupAndVerificationSeeTheTaskReference(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	out := t.TempDir()
	setupFile, verifyFile := filepath.Join(out, "setup.txt"), filepath.Join(out, "verify.txt")

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.SetupSteps = []task.VerificationStep{{Command: "sh", Args: []string{"-c", `printf %s "$AIDEV_TASK_REF" > ` + setupFile}}}
		in.Verification = []task.VerificationStep{
			{Command: "sh", Args: []string{"-c", `printf %s "$AIDEV_TASK_REF" > ` + verifyFile}},
			{Command: "test", Args: []string{"-f", "marker.txt"}},
		}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v: %s", outcome.Task.Status, err, outcome.Message)
	}
	for name, path := range map[string]string{"setup": setupFile, "verification": verifyFile} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s step left no output: %v", name, err)
		}
		if got := strings.TrimSpace(string(raw)); got != created.Ref {
			t.Errorf("%s step saw AIDEV_TASK_REF=%q, want %q", name, got, created.Ref)
		}
	}
}
