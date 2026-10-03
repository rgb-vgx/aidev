package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aidev/internal/agent"
	"aidev/internal/logging"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Cancel used to change only the database: the agent kept running, and spending,
// until its own timeout. `aidev task cancel` and the MCP server are usually other
// processes than the one running the task, so the run has to notice by reading the
// task's status. A second Orchestrator over the same database stands in for that
// other process: it shares nothing with the first but PostgreSQL.

// otherProcess returns an orchestrator that shares only the database with h.
func otherProcess(h *harness) *worker.Orchestrator {
	return worker.New(h.store, h.git, &agent.Fake{}, h.orchestrator.Config, logging.Discard())
}

// waitUntilStatus polls until the task reaches want, or fails the test.
func waitUntilStatus(t *testing.T, h *harness, ref string, want task.Status) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		current, err := h.store.ResolveTask(h.ctx, ref)
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}
		if current.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s", ref, want)
}

// runInBackground starts RunTask and returns a channel with its result.
type runResult struct {
	outcome worker.Outcome
	err     error
	took    time.Duration
}

func runInBackground(h *harness, ref string) <-chan runResult {
	done := make(chan runResult, 1)
	go func() {
		started := time.Now()
		outcome, err := h.orchestrator.RunTask(h.ctx, ref)
		done <- runResult{outcome, err, time.Since(started)}
	}()
	return done
}

// assertCancelledAndKept checks the record a cancelled run must leave.
func assertCancelledAndKept(t *testing.T, h *harness, created task.Task, res runResult) {
	t.Helper()
	// The task ended the way someone asked it to: an outcome, not an error.
	if res.err != nil {
		t.Errorf("RunTask: %v; a cancelled task is an outcome, not an error", res.err)
	}
	if res.outcome.Task.Status != task.StatusCancelled {
		t.Errorf("outcome status = %s, want CANCELLED: %s", res.outcome.Task.Status, res.outcome.Message)
	}
	reloaded, err := h.store.GetTask(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusCancelled {
		t.Errorf("persisted status = %s, want CANCELLED", reloaded.Status)
	}
	attempts, err := h.store.ListAttempts(h.ctx, created.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %v, %v; want exactly one", attempts, err)
	}
	if attempts[0].Status != task.AttemptCancelled || attempts[0].FinishedAt == nil {
		t.Errorf("attempt = %s (finished %v), want CANCELLED and finished", attempts[0].Status, attempts[0].FinishedAt)
	}
	wt, err := h.store.GetWorktreeByAttempt(h.ctx, attempts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED", wt.Status)
	}
	history := h.eventTypes(created.ID)
	if contains(history, "task.succeeded") || contains(history, "task.failed") {
		t.Errorf("a cancelled task's history records another ending: %v", history)
	}
}

func TestCancelFromAnotherProcessStopsTheAgent(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = 100 * time.Millisecond
	h.backend.Delay = time.Hour

	created := h.createTask(nil)
	done := runInBackground(h, created.Ref)
	waitUntilStatus(t, h, created.Ref, task.StatusRunning)
	// Let the agent actually start before cancelling.
	time.Sleep(200 * time.Millisecond)

	if _, err := otherProcess(h).Cancel(h.ctx, created.Ref, "stop it"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case res := <-done:
		if res.took > 10*time.Second {
			t.Errorf("the run took %s to stop", res.took)
		}
		assertCancelledAndKept(t, h, created, res)
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was still running 10s after another process cancelled the task")
	}
}

// When Cancel runs in the process that is running the task — one MCP server does
// both — there is no reason to wait for a poll.
func TestCancelInTheSameProcessStopsTheAgentWithoutPolling(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = time.Hour
	h.backend.Delay = time.Hour

	created := h.createTask(nil)
	done := runInBackground(h, created.Ref)
	waitUntilStatus(t, h, created.Ref, task.StatusRunning)
	time.Sleep(200 * time.Millisecond)

	if _, err := h.orchestrator.Cancel(h.ctx, created.Ref, "stop it"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case res := <-done:
		assertCancelledAndKept(t, h, created, res)
	case <-time.After(5 * time.Second):
		t.Fatal("the agent was still running 5s after the same orchestrator cancelled the task")
	}
}

// A cancelled run's diff is the record of what the agent managed to do — the
// audit record is most valuable precisely then — but persistWorkerRun used to
// collect it on the run's own context, which a cancel has already closed, so
// worker_runs.diff came out empty for exactly the runs that needed it most.
func TestCancelledRunStillRecordsTheDiff(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = 100 * time.Millisecond
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		// Block until the cancel reaches us: the file above is the work whose
		// diff must survive the cancellation.
		<-ctx.Done()
		return nil
	}

	created := h.createTask(nil)
	done := runInBackground(h, created.Ref)
	waitUntilStatus(t, h, created.Ref, task.StatusRunning)
	time.Sleep(200 * time.Millisecond)

	if _, err := otherProcess(h).Cancel(h.ctx, created.Ref, "stop it"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case res := <-done:
		assertCancelledAndKept(t, h, created, res)
		if res.outcome.Attempt == nil {
			t.Fatal("the outcome carries no attempt")
		}
		runs, err := h.store.ListWorkerRuns(h.ctx, res.outcome.Attempt.ID)
		if err != nil {
			t.Fatalf("ListWorkerRuns: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("worker runs = %d, want exactly 1", len(runs))
		}
		if !strings.Contains(runs[0].Diff, "marker.txt") {
			t.Errorf("diff = %q, want it to contain the file the agent wrote", runs[0].Diff)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not stop within 10s of the cancel")
	}
}

// Two processes cancelling one task at the same instant is normal use — the
// CLI and the MCP server are different processes. The loser of the
// compare-and-set must not be told "conflict": Cancel re-reads the task and
// reports it as already cancelled (ErrNotRunnable) instead, up to its retry
// budget, and exactly one cancel wins per task.
func TestConcurrentCancelNeverSurfacesAConflict(t *testing.T) {
	h := newHarness(t, nil)

	for round := 0; round < 10; round++ {
		created := h.createTask(nil)

		start := make(chan struct{})
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				_, err := otherProcess(h).Cancel(h.ctx, created.Ref, "stop it")
				errs <- err
			}()
		}
		close(start)

		var successes int
		for i := 0; i < 2; i++ {
			err := <-errs
			switch {
			case err == nil:
				successes++
			case errors.Is(err, worker.ErrNotRunnable):
				// The loser's verdict: the task is already cancelled.
			default:
				t.Errorf("round %d: Cancel = %v; a concurrent cancel must resolve to success or ErrNotRunnable, never a raw conflict", round, err)
			}
		}
		if successes != 1 {
			t.Errorf("round %d: successes = %d, want exactly 1", round, successes)
		}
	}
}

// Verification commands can run for minutes too (go test), so a cancel has to
// reach them as well, and a step it stopped is not a pass.
func TestCancelFromAnotherProcessStopsVerification(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = 100 * time.Millisecond
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{
			{Command: "sleep", Args: []string{"60"}},
		}
	})
	done := runInBackground(h, created.Ref)
	waitUntilStatus(t, h, created.Ref, task.StatusVerifying)
	time.Sleep(200 * time.Millisecond)

	if _, err := otherProcess(h).Cancel(h.ctx, created.Ref, "stop it"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case res := <-done:
		assertCancelledAndKept(t, h, created, res)
		if res.outcome.Attempt != nil {
			runs, err := h.store.ListVerificationRuns(h.ctx, res.outcome.Attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, vr := range runs {
				if vr.Status == task.VerificationPassed {
					t.Errorf("a step stopped by the cancel was recorded as PASSED: %+v", vr)
				}
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("verification was still running 10s after another process cancelled the task")
	}
}

// A run that is never cancelled must not be disturbed by the watching: it ends as
// it would have, and the watcher does not outlive it.
func TestWatchingForCancelDoesNotDisturbANormalRun(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = 10 * time.Millisecond
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		time.Sleep(300 * time.Millisecond)
		return doTheWork(ctx, req)
	}

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: %s", outcome.Task.Status, outcome.Message)
	}
}
