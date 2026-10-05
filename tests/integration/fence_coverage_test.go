package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aidev/internal/agent"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Every write a run makes while its attempt is open must pass the lease fence
// (store.HoldLease), not only the ones on the success path. The tests below
// take the attempt away from a run at each of the other points where it writes
// — while it is creating the worktree, while it hands the worktree to a retry,
// and while it records a failure — and require that nothing of the run's
// reaches the database or git afterwards.
//
// The runs here are the "zombie" every time: another process ends the attempt
// while the run is busy, and the run's own watcher polls too rarely to hear
// about it (CancelPoll is an hour). A run that writes without fencing leaves
// evidence after task.cancelled, which is what these tests look for.

// gitStub writes a git that logs every invocation and, for the subcommand a
// test names, stops before running the real git until the test opens the
// gate. Stopping on a file rather than a timer is what makes these tests
// deterministic: the test cancels the attempt while the run is provably held
// at that point, then lets the run continue.
type gitStub struct {
	path   string
	marker string
	gate   string
}

func newGitStub(t *testing.T, heldSubcommand string) gitStub {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	stub := gitStub{
		path:   filepath.Join(dir, "git"),
		marker: filepath.Join(dir, "calls.log"),
		gate:   filepath.Join(dir, "gate"),
	}
	// The loop skips a flag and the value that belongs to it — aidev runs
	// every git command as `git -c k=v ... <subcommand> ...`, and without
	// skipping the value the scan stops on it and holds nothing.
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
skip=0
for a in "$@"; do
  if [ "$skip" = 1 ]; then skip=0; continue; fi
  case "$a" in
    -c|-C|--git-dir|--work-tree) skip=1 ;;
    %s) echo held >> %q; while [ ! -f %q ]; do sleep 0.02; done; break ;;
    -*) ;;
    *) break ;;
  esac
done
exec %q "$@"
`, stub.marker, heldSubcommand, stub.marker, stub.gate, real)
	if err := os.WriteFile(stub.path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// saw reports whether the stub has run a call whose arguments contain want.
func (s gitStub) saw(t *testing.T, want string) bool {
	t.Helper()
	raw, err := os.ReadFile(s.marker)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func (s gitStub) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s.saw(t, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(s.marker)
	t.Fatalf("the run never reached %q; git calls so far:\n%s", want, raw)
}

// release lets the held git call run.
func (s gitStub) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(s.gate, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// eventsAfterCancelled returns the event types recorded after the task was
// cancelled, which is the evidence a fence-less write leaves behind.
func (h *harness) eventsAfterCancelled(t *testing.T, taskID uuid.UUID, ref string) []string {
	t.Helper()
	history := h.eventTypes(taskID)
	i := indexOf(history, "task.cancelled")
	if i < 0 {
		t.Fatalf("%s was never cancelled: %v", ref, history)
	}
	return history[i+1:]
}

// Creating the worktree is the run's first write after its attempt opens.
func TestCancelledRunDoesNotCreateItsWorktree(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = time.Hour // the run never hears about the cancel
	stub := newGitStub(t, "worktree")
	h.git.Command = stub.path
	h.backend.Work = doTheWork

	created := h.createTask(nil)
	done := make(chan worker.Outcome, 1)
	go func() {
		out, _ := h.orchestrator.RunTask(h.ctx, created.Ref)
		done <- out
	}()

	// The run is held inside the git call that creates the checkout; the
	// attempt is ended before it can record anything about it.
	stub.waitFor(t, "worktree add")
	if _, err := otherProcess(h).Cancel(context.Background(), created.Ref, "ended while the worktree was being made"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// What the cancel left behind, recorded before the run continues: a
	// failure below is unreadable without it.
	cancelledTask, _ := h.store.GetTask(context.Background(), created.ID)
	cancelledAttempts := attemptsInOrder(t, h, created.ID)
	stub.release(t)
	out := <-done

	if out.Task.Status != task.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED as the other process recorded", out.Task.Status)
	}
	for _, a := range cancelledAttempts {
		if a.Status == task.AttemptRunning {
			t.Errorf("after the cancel, attempt %d is still %s (task %s): the cancel did not close it",
				a.AttemptNumber, a.Status, cancelledTask.Status)
		}
	}
	if after := h.eventsAfterCancelled(t, created.ID, created.Ref); len(after) > 0 {
		t.Errorf("the run wrote events after the attempt was taken from it: %v", after)
	}
	attempt, err := h.store.LatestAttempt(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.GetWorktreeByAttempt(h.ctx, attempt.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("worktree record = %v, want none: the run recorded one after losing its attempt", err)
	}
	// And the directory git made is not left behind unrecorded.
	entries, err := os.ReadDir(h.workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), created.Ref) {
			t.Errorf("unrecorded worktree directory left on disk: %s", e.Name())
		}
	}
}

// Handing the worktree to a retry creates a branch and a new worktree record.
// The run is held in the git call before that write; the test then ends the
// attempt the way another process would and only then lets the run continue.
// What the run does next is a fact, not a race.
func TestEndedAttemptIsNotHandedToARetry(t *testing.T) {
	h := newHarness(t, nil)
	stub := newGitStub(t, "branch")
	h.git.Command = stub.path
	h.backend.Work = func(context.Context, agent.Request) error { return nil } // fails the checks

	created := h.createTask(withRetries(1))

	done := make(chan worker.Outcome, 1)
	go func() {
		out, _ := h.orchestrator.RunTask(h.ctx, created.Ref)
		done <- out
	}()
	stub.waitFor(t, "-a2") // the run is at the retry's branch, the next write is fenced

	// End the attempt the way Cancel or a recovery would: the attempt is
	// finished and the task is no longer RUNNING.
	attempts := attemptsInOrder(t, h, created.ID)
	latest := attempts[len(attempts)-1]
	if latest.Status != task.AttemptRunning {
		t.Fatalf("attempt %d is %s before the hand-over, want RUNNING", latest.AttemptNumber, latest.Status)
	}
	execSQL(t, `UPDATE task_attempts SET status = 'CANCELLED', failure_kind = 'CANCELLED', error = 'ended by another process', finished_at = now() WHERE id = $1`, latest.ID)
	execSQL(t, `UPDATE tasks SET status = 'CANCELLED' WHERE id = $1`, created.ID)
	// Only now does the run continue: its next write is the fenced one, and
	// the attempt it would write for is already gone.
	stub.release(t)

	out := <-done
	if out.Task.Status != task.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED", out.Task.Status)
	}

	// Nothing of the abandoned hand-over may be recorded: no worktree row for
	// the attempt that was ended, no branch left behind for it.
	if wt, err := h.store.GetWorktreeByAttempt(h.ctx, latest.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("attempt %d has a worktree record %+v, want none: the run recorded it after losing the attempt",
			latest.AttemptNumber, wt)
	}
	if h.branchExists("aidev/" + created.Ref + "-a2") {
		t.Errorf("the branch of the abandoned retry is left in the repository")
	}
	if after := h.eventTypes(created.ID); contains(after, "task.worktree_created") && countOf(after, "task.worktree_created") > 1 {
		t.Errorf("a second worktree_created event was recorded: %v", after)
	}
}

// Recording a failure marks the worktree retained (fail, before its own
// transaction), which is another write that must not happen after the attempt
// was taken from the run.
func TestCancelledRunDoesNotRecordItsOwnFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = time.Hour
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		// The checks fail, so the run takes the failure path after the cancel.
		in.Verification = []task.VerificationStep{shell("sleep 1 && false")}
	})

	done := make(chan worker.Outcome, 1)
	go func() {
		out, _ := h.orchestrator.RunTask(h.ctx, created.Ref)
		done <- out
	}()

	// Cancel while the checks are running.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if current, err := h.store.GetTask(context.Background(), created.ID); err == nil && current.Status == task.StatusVerifying {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := otherProcess(h).Cancel(context.Background(), created.Ref, "ended while the checks ran"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	out := <-done

	if out.Task.Status != task.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED", out.Task.Status)
	}
	if after := h.eventsAfterCancelled(t, created.ID, created.Ref); len(after) > 0 {
		t.Errorf("the run wrote events after the attempt was taken from it: %v", after)
	}
	// Cancel marks the worktree RETAINED without an event of its own, and the
	// run's retention is refused by the fence, so the history holds none.
	if n := countOf(h.eventTypes(created.ID), "task.worktree_retained"); n != 0 {
		t.Errorf("%d worktree_retained events, want none: the run did not retain anything itself", n)
	}
	attempts := attemptsInOrder(t, h, created.ID)
	if len(attempts) != 1 || attempts[0].Status != task.AttemptCancelled {
		t.Errorf("attempts = %+v, want one CANCELLED attempt", attempts)
	}
	if wt, err := h.store.GetWorktreeByAttempt(h.ctx, attempts[0].ID); err != nil || wt.Status != task.WorktreeRetained {
		t.Errorf("worktree = %+v, %v; want RETAINED as the cancel left it", wt, err)
	}
	if n := countOf(h.eventTypes(created.ID), "task.failed"); n != 0 {
		t.Errorf("%d task.failed events, want none: the cancel ended the task", n)
	}
}

// execSQL runs one statement against the test database, for the states a
// second process would leave behind.
func execSQL(t *testing.T, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv(envDatabaseURL))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
