package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aidev/internal/agent"
	"aidev/internal/cli"
	"aidev/internal/store"
	"aidev/internal/task"
)

// runCLI drives the real command surface, with the environment the commands read.
// It exists because the worktree commands are the operator's only way to act on the
// cleanup policy, and testing them below the CLI would leave the part an operator
// actually touches unexercised.
func (h *harness) runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	t.Setenv("DATABASE_URL", os.Getenv(envDatabaseURL))
	t.Setenv("WORKSPACE_ROOT", h.workspace)
	t.Setenv("LOG_LEVEL", "error")

	var out, errOut bytes.Buffer
	err = cli.Run(context.Background(), "test", args, &out, &errOut)
	return out.String(), errOut.String(), err
}

// A failed attempt's worktree must be findable, or "the work is kept for
// inspection" is a promise the tool does not help anyone act on.
func TestWorktreeListShowsRetainedWork(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(_ context.Context, req agent.Request) error {
		return os.WriteFile(filepath.Join(req.WorkingDir, "half-done.txt"), []byte("wip\n"), 0o600)
	}

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED", outcome.Task.Status)
	}

	stdout, _, err := h.runCLI(t, "worktree", "list")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if !strings.Contains(stdout, created.Ref) {
		t.Errorf("listing does not mention the task:\n%s", stdout)
	}
	if !strings.Contains(stdout, "RETAINED") {
		t.Errorf("listing does not show the worktree as retained:\n%s", stdout)
	}
	if !strings.Contains(stdout, "on disk") {
		t.Errorf("listing does not report disk usage, which is why an operator looks:\n%s", stdout)
	}

	// The JSON form is what a script would use to find reclaimable space.
	stdout, _, err = h.runCLI(t, "worktree", "list", "--json")
	if err != nil {
		t.Fatalf("worktree list --json: %v", err)
	}
	if !strings.Contains(stdout, `"on_disk": true`) {
		t.Errorf("JSON does not report the worktree as present:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"status": "RETAINED"`) {
		t.Errorf("JSON status is wrong:\n%s", stdout)
	}
}

// Removing a worktree that still holds uncommitted work must be refused, and the
// refusal must say how to proceed deliberately.
func TestWorktreeRemoveProtectsUncommittedWork(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(_ context.Context, req agent.Request) error {
		return os.WriteFile(filepath.Join(req.WorkingDir, "half-done.txt"), []byte("wip\n"), 0o600)
	}

	created := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	attempt, err := h.store.LatestAttempt(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := h.store.GetWorktreeByAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(record.Path, "half-done.txt")

	_, _, err = h.runCLI(t, "worktree", "remove", created.Ref)
	if err == nil {
		t.Fatal("removing a worktree with uncommitted work was accepted")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %v, want it to explain how to proceed deliberately", err)
	}
	if _, statErr := os.Stat(partial); statErr != nil {
		t.Errorf("the refused removal destroyed the work anyway: %v", statErr)
	}

	// The record must not have been changed by a removal that did not happen.
	reloaded, err := h.store.GetWorktreeByAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.WorktreeRetained {
		t.Errorf("status = %s after a refused removal, want RETAINED", reloaded.Status)
	}

	// --force is the deliberate escape hatch, and it is recorded as such.
	stdout, _, err := h.runCLI(t, "worktree", "remove", created.Ref, "--force")
	if err != nil {
		t.Fatalf("worktree remove --force: %v", err)
	}
	if !strings.Contains(stdout, "removed") {
		t.Errorf("output = %q", stdout)
	}
	if !strings.Contains(stdout, "branch") {
		t.Errorf("output should say the branch is untouched: %q", stdout)
	}
	if _, statErr := os.Stat(record.Path); !os.IsNotExist(statErr) {
		t.Error("the worktree directory still exists after --force")
	}

	reloaded, err = h.store.GetWorktreeByAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.WorktreeRemoved || reloaded.RemovedAt == nil {
		t.Errorf("record = %+v, want REMOVED with a removal time", reloaded)
	}

	// The history says a human did it, not aidev's cleanup policy.
	events, err := h.store.ListEvents(h.ctx, store.EventFilter{TaskID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	// The payload is decoded rather than string-matched: PostgreSQL normalises
	// jsonb, so its spacing is not aidev's to predict.
	var found bool
	for _, e := range events {
		if e.Type.String() != "task.worktree_removed" {
			continue
		}
		var payload struct {
			By     string `json:"by"`
			Forced bool   `json:"forced"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decode event payload: %v (%s)", err, e.Payload)
		}
		if payload.By != "operator" {
			continue
		}
		found = true
		if !payload.Forced {
			t.Errorf("the forced removal was not recorded as forced: %s", e.Payload)
		}
	}
	if !found {
		t.Error("the operator's removal was not recorded in the task's history")
	}
}

func TestWorktreeRemoveRefusesWhileTheTaskIsActive(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Delay = 30 * time.Second
	created := h.createTask(nil)

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	go func() {
		defer close(done)
		_, _ = h.orchestrator.RunTask(ctx, created.Ref)
	}()

	// Wait for the task to actually be running before asserting on it.
	waitForStatus(t, h, created.ID, task.StatusRunning)

	_, _, err := h.runCLI(t, "worktree", "remove", created.Ref)
	if err == nil {
		t.Fatal("removing the worktree of a running task was accepted")
	}
	if !strings.Contains(err.Error(), "cancel it") {
		t.Errorf("error = %v, want it to say to cancel the task first", err)
	}

	cancel()
	<-done
}

func TestWorktreeRemoveOnATaskThatNeverRan(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)

	_, _, err := h.runCLI(t, "worktree", "remove", created.Ref)
	if err == nil {
		t.Fatal("expected an error for a task with no worktree")
	}
	if !strings.Contains(err.Error(), "never run") {
		t.Errorf("error = %v, want it to say the task has never run", err)
	}
}

// A worktree deleted outside aidev must be reconciled rather than blocking the
// operator: the record is aidev's belief, and the disk is the truth.
func TestWorktreeRemoveReconcilesAMissingDirectory(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(_ context.Context, req agent.Request) error {
		return os.WriteFile(filepath.Join(req.WorkingDir, "half-done.txt"), []byte("wip\n"), 0o600)
	}

	created := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	attempt, err := h.store.LatestAttempt(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := h.store.GetWorktreeByAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Someone deletes it by hand.
	if err := os.RemoveAll(record.Path); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := h.runCLI(t, "worktree", "list")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if !strings.Contains(stdout, "missing from disk") {
		t.Errorf("listing should flag a record whose directory is gone:\n%s", stdout)
	}

	stdout, _, err = h.runCLI(t, "worktree", "remove", created.Ref)
	if err != nil {
		t.Fatalf("worktree remove on a missing directory: %v", err)
	}
	if !strings.Contains(stdout, "already gone") {
		t.Errorf("output = %q, want it to say the directory was already gone", stdout)
	}

	reloaded, err := h.store.GetWorktreeByAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.WorktreeRemoved {
		t.Errorf("status = %s, want the record reconciled to REMOVED", reloaded.Status)
	}
}

func TestListWorktreesFiltersByStatus(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	succeeded := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, succeeded.Ref); err != nil {
		t.Fatal(err)
	}

	h.backend.Work = func(_ context.Context, req agent.Request) error {
		return os.WriteFile(filepath.Join(req.WorkingDir, "wrong.txt"), []byte("x"), 0o600)
	}
	failed := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, failed.Ref); err != nil {
		t.Fatal(err)
	}

	retained, err := h.store.ListWorktrees(h.ctx, []task.WorktreeStatus{task.WorktreeRetained})
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(retained) != 1 || retained[0].TaskRef != failed.Ref {
		t.Errorf("retained = %+v, want only the failed task's worktree", retained)
	}
	if retained[0].TaskStatus != task.StatusFailed {
		t.Errorf("task status = %s, want FAILED", retained[0].TaskStatus)
	}
	if retained[0].AttemptNumber != 1 {
		t.Errorf("attempt = %d, want 1", retained[0].AttemptNumber)
	}
	if retained[0].TaskTitle == "" {
		t.Error("the task title is missing, so an operator could not tell what the directory is")
	}

	all, err := h.store.ListWorktrees(h.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("got %d worktrees with no filter, want 2", len(all))
	}
}

// waitForStatus polls until a task reaches the wanted status, so a test asserting
// on a running task does not race the goroutine that starts it.
func waitForStatus(t *testing.T, h *harness, taskID uuid.UUID, want task.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		reloaded, err := h.store.GetTask(h.ctx, taskID)
		if err == nil && reloaded.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task did not reach %s within 10s", want)
}
