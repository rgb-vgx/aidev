package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"aidev/internal/logging"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Fencing (production review, 2026-10-04): a lease only lets aidev notice a
// dead run; it does not stop a run that was merely paused — or that never got
// the news — from writing after someone else ended its attempt. Every write a
// run makes while its attempt is open now first checks, under the task's row
// lock, that the attempt is still RUNNING and held by this process. A run that
// lost its attempt publishes nothing: no commit on the branch, no events after
// the ending, no rows.
//
// The run here is the "zombie": another process cancels the task while its
// verification runs, and the run's own watcher polls too rarely to hear about
// it, exactly as a process paused past its lease would.
func TestARunThatLostItsAttemptPublishesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = time.Hour // this run never hears about the cancel
	h.backend.Work = doTheWork
	other := worker.New(h.store, h.git, h.backend, h.orchestrator.Config, logging.Discard())

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{shell("sleep 1 && test -f marker.txt")}
	})

	cancelled := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			current, err := h.store.GetTask(context.Background(), created.ID)
			if err == nil && current.Status == task.StatusVerifying {
				_, err := other.Cancel(context.Background(), created.Ref, "recovered by another process")
				cancelled <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancelled <- context.DeadlineExceeded
	}()

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if cerr := <-cancelled; cerr != nil {
		t.Fatalf("Cancel from the other process: %v", cerr)
	}
	if err != nil {
		t.Fatalf("RunTask: %v; losing the attempt is an outcome, not an error", err)
	}
	if outcome.Task.Status != task.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED as the other process recorded", outcome.Task.Status)
	}

	// The checks passed after the cancel, but the run no longer owned the
	// attempt: nothing may reach the branch.
	if files := h.branchFiles("aidev/" + created.Ref); strings.Contains(files, "marker.txt") {
		t.Errorf("the branch holds the work although the attempt was taken from the run:\n%s", files)
	}

	// The cancel is the last word in the history.
	history := h.eventTypes(created.ID)
	i := indexOf(history, "task.cancelled")
	if i < 0 {
		t.Fatalf("history has no task.cancelled: %v", history)
	}
	if after := history[i+1:]; len(after) > 0 {
		t.Errorf("events written after the attempt was taken from the run: %v", after)
	}

	stored, err := h.store.GetTask(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != task.StatusCancelled {
		t.Errorf("persisted status = %s, want CANCELLED", stored.Status)
	}
}
