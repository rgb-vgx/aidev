package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
)

// Review findings on the delegated TASK-000086 and TASK-000087 that their
// specifications did not exercise.

// A task that failed after a retry has a head commit past its base — the
// earlier attempt's unverified partial commit — but it delivered nothing:
// task diff must not present that as the delivered change.
func TestTaskDiffOfARetriedFailureIsNotDelivered(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		return os.WriteFile(filepath.Join(req.WorkingDir, "partial.txt"), []byte("half done\n"), 0o600)
	}
	created := h.createTask(withRetries(1))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || outcome.Task.Status != task.StatusFailed {
		t.Fatalf("RunTask = %v, %v; want FAILED after the retry", outcome.Task.Status, err)
	}
	stdout, stderr, err := h.runCLI(t, "task", "diff", created.Ref)
	if err != nil {
		t.Fatalf("task diff: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "not delivered") {
		t.Errorf("task diff of a task that failed after a retry presents the change as delivered:\n%s", stdout)
	}
}

// A diff larger than aidev captures must say it was cut, and how to see all
// of it, instead of ending silently half-way.
func TestTaskDiffSaysWhenItIsTruncated(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		big := strings.Repeat("a line of a large generated file\n", 45000) // ~1.5 MiB, over the CLI's 1 MiB default cap
		if err := os.WriteFile(filepath.Join(req.WorkingDir, "big.txt"), []byte(big), 0o600); err != nil {
			return err
		}
		return doTheWork(ctx, req)
	}
	created := h.createTask(nil)
	if out, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v", out.Task.Status, err)
	}
	_, stderr, err := h.runCLI(t, "task", "diff", created.Ref)
	if err != nil {
		t.Fatalf("task diff: %v", err)
	}
	if !strings.Contains(stderr, "truncated") || !strings.Contains(stderr, "git -C "+h.repoPath+" diff ") {
		t.Errorf("stderr = %q, want a note that the diff was truncated and the git command for all of it", stderr)
	}
}

// The MCP result carries earlier_attempts too: a planner reading a success
// on attempt 2 through Claude needs the same history as a person at the CLI.
func TestMCPResultListsEarlierAttempts(t *testing.T) {
	m := newMCPHarness(t)
	created := retriedTask(t, m.harness)

	var result struct {
		Result struct {
			EarlierAttempts []struct {
				Number      int    `json:"number"`
				FailureKind string `json:"failure_kind"`
			} `json:"earlier_attempts"`
		} `json:"result"`
	}
	m.call(t, "aidev_get_task_result", map[string]any{"task": created.Ref}, &result)
	got := result.Result.EarlierAttempts
	if len(got) != 1 || got[0].Number != 1 || got[0].FailureKind != "VERIFICATION" {
		t.Errorf("earlier_attempts = %+v, want attempt 1 with VERIFICATION", got)
	}
}
