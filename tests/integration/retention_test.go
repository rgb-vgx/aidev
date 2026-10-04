package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aidev/internal/agent"
	"aidev/internal/store"
	"aidev/internal/task"
)

// Research C6: captured output had no way out of the database. `aidev prune`
// clears old output on finished tasks and keeps the record; `aidev task
// delete` removes a finished task with its history.

// seedRunsAt gives a task an attempt with one agent run and one verification
// step that finished at the given time, each holding output to prune.
func seedRunsAt(t *testing.T, ctx context.Context, db *store.Store, tk task.Task, finished time.Time) (task.WorkerRun, task.VerificationRun) {
	t.Helper()
	attempt, err := db.CreateAttempt(ctx, task.NewAttempt(tk.ID, 1))
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	exit := 0
	run, err := db.CreateWorkerRun(ctx, task.WorkerRun{
		ID: uuid.Must(uuid.NewV7()), AttemptID: attempt.ID, Backend: "opencode",
		Status: task.WorkerSucceeded, Command: "opencode run", WorkingDir: "/tmp/wt", ExitCode: &exit,
		Stdout: strings.Repeat("o", 1000), Stderr: strings.Repeat("e", 100), Diff: strings.Repeat("d", 500),
		Summary: "Created marker.txt", ChangedFiles: 1,
		StartedAt: finished.Add(-time.Minute), FinishedAt: &finished,
	})
	if err != nil {
		t.Fatalf("CreateWorkerRun: %v", err)
	}
	step, err := db.CreateVerificationRun(ctx, task.VerificationRun{
		ID: uuid.Must(uuid.NewV7()), AttemptID: attempt.ID, StepIndex: 0, Phase: task.PhaseVerify,
		Command: "go test ./...", Status: task.VerificationPassed, ExitCode: &exit,
		Stdout: strings.Repeat("v", 200), Duration: time.Second,
		StartedAt: finished.Add(-time.Second), FinishedAt: &finished,
	})
	if err != nil {
		t.Fatalf("CreateVerificationRun: %v", err)
	}
	return run, step
}

// finish moves a seeded task through the state machine to FAILED.
func finish(t *testing.T, ctx context.Context, db *store.Store, tk task.Task) {
	t.Helper()
	for _, step := range [][2]task.Status{
		{task.StatusPending, task.StatusReady},
		{task.StatusReady, task.StatusRunning},
		{task.StatusRunning, task.StatusFailed},
	} {
		if err := db.TransitionTask(ctx, tk.ID, step[0], step[1]); err != nil {
			t.Fatalf("TransitionTask %s -> %s: %v", step[0], step[1], err)
		}
	}
}

func TestPruneClearsOldOutputOfFinishedTasksOnly(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	done := seedTask(t, ctx, db, p, nil)
	doneRun, doneStep := seedRunsAt(t, ctx, db, done, old)
	finish(t, ctx, db, done)

	// Same age, but the task has not finished: its output may be what
	// someone is about to read, so it is never touched.
	open := seedTask(t, ctx, db, p, nil)
	openRun, _ := seedRunsAt(t, ctx, db, open, old)

	// Finished, but recent: inside the retention window.
	recent := seedTask(t, ctx, db, p, nil)
	recentRun, _ := seedRunsAt(t, ctx, db, recent, time.Now().UTC())
	finish(t, ctx, db, recent)

	eventsBefore := countEvents(t, ctx, db, done.ID)
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	dry, err := db.PruneLogs(ctx, cutoff, true)
	if err != nil {
		t.Fatalf("PruneLogs dry run: %v", err)
	}
	if dry.WorkerRuns != 1 || dry.VerificationRuns != 1 || dry.Bytes != 1000+100+500+200 {
		t.Errorf("dry run = %+v, want 1 agent run, 1 step, 1800 bytes", dry)
	}
	if got := workerRun(t, ctx, db, doneRun.AttemptID); got.Stdout == "" || got.LogsPruned {
		t.Fatal("a dry run cleared output")
	}

	report, err := db.PruneLogs(ctx, cutoff, false)
	if err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}
	if report.WorkerRuns != 1 || report.VerificationRuns != 1 || report.Bytes != 1800 {
		t.Errorf("report = %+v, want what the dry run predicted", report)
	}

	got := workerRun(t, ctx, db, doneRun.AttemptID)
	if got.Stdout != "" || got.Stderr != "" || got.Diff != "" || !got.LogsPruned {
		t.Errorf("finished old run after prune = stdout %d, stderr %d, diff %d bytes, pruned %v; want all cleared and flagged",
			len(got.Stdout), len(got.Stderr), len(got.Diff), got.LogsPruned)
	}
	// The record itself stays: outcome, summary, counts.
	if got.Status != task.WorkerSucceeded || got.Summary != "Created marker.txt" || got.ChangedFiles != 1 {
		t.Errorf("prune changed more than the output: %+v", got)
	}
	steps, err := db.ListVerificationRuns(ctx, doneStep.AttemptID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("ListVerificationRuns = %v, %v", steps, err)
	}
	if steps[0].Stdout != "" || !steps[0].LogsPruned || steps[0].Status != task.VerificationPassed {
		t.Errorf("finished old step after prune = %+v, want output cleared, flagged, status kept", steps[0])
	}

	if r := workerRun(t, ctx, db, openRun.AttemptID); r.Stdout == "" || r.LogsPruned {
		t.Error("prune cleared the output of a task that has not finished")
	}
	if r := workerRun(t, ctx, db, recentRun.AttemptID); r.Stdout == "" || r.LogsPruned {
		t.Error("prune cleared output newer than the cutoff")
	}

	// Invariant 4: the event log is untouched.
	if after := countEvents(t, ctx, db, done.ID); after != eventsBefore {
		t.Errorf("events for the pruned task went from %d to %d", eventsBefore, after)
	}

	// Nothing left to clear: a second pass reports nothing.
	again, err := db.PruneLogs(ctx, cutoff, false)
	if err != nil || again.WorkerRuns != 0 || again.VerificationRuns != 0 || again.Bytes != 0 {
		t.Errorf("second prune = %+v, %v; want nothing", again, err)
	}
}

func TestPruneCLIRequiresAnAge(t *testing.T) {
	h := newHarness(t, nil)
	if _, stderr, err := h.runCLI(t, "prune"); err == nil || !strings.Contains(stderr+err.Error(), "--logs-older-than") {
		t.Errorf("prune without an age = %v (%s), want a usage error naming --logs-older-than", err, stderr)
	}
	stdout, stderr, err := h.runCLI(t, "prune", "--logs-older-than", "30d", "--dry-run")
	if err != nil {
		t.Fatalf("prune --dry-run: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "would clear") {
		t.Errorf("dry run output = %q", stdout)
	}
}

func TestTaskDeleteRemovesAFinishedTaskWithItsHistory(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	created := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	branch := "aidev/" + created.Ref

	stdout, stderr, err := h.runCLI(t, "task", "delete", created.Ref)
	if err != nil {
		t.Fatalf("task delete: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "deleted "+created.Ref) {
		t.Errorf("task delete output = %q", stdout)
	}
	if _, err := h.store.GetTask(h.ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTask after delete = %v, want ErrNotFound", err)
	}
	if n := countEvents(t, h.ctx, h.store, created.ID); n != 0 {
		t.Errorf("%d events survived the delete, want the cascade to remove them", n)
	}
	// The delivered branch is the repository's, not aidev's to remove.
	if !h.branchExists(branch) {
		t.Errorf("task delete removed the branch %s", branch)
	}
}

func TestTaskDeleteRefusesWhatCanStillChangeOrIsOnDisk(t *testing.T) {
	h := newHarness(t, nil)

	pending := h.createTask(nil)
	if _, stderr, err := h.runCLI(t, "task", "delete", pending.Ref); err == nil ||
		!strings.Contains(stderr+err.Error(), "cancel") {
		t.Errorf("deleting a PENDING task = %v (%s), want a refusal pointing at cancel", err, stderr)
	}

	// A failed attempt keeps its worktree; deleting the record would orphan it.
	h.backend.Work = func(context.Context, agent.Request) error { return nil } // marker never made
	failed := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, failed.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if _, stderr, err := h.runCLI(t, "task", "delete", failed.Ref); err == nil ||
		!strings.Contains(stderr+err.Error(), "aidev worktree remove") {
		t.Errorf("deleting a task with a retained worktree = %v (%s), want a refusal pointing at worktree remove", err, stderr)
	}
	if _, err := h.store.GetTask(h.ctx, failed.ID); err != nil {
		t.Errorf("the refused task is gone: %v", err)
	}

	// Once the worktree is removed, the task may go.
	if _, stderr, err := h.runCLI(t, "worktree", "remove", failed.Ref, "--force"); err != nil {
		t.Fatalf("worktree remove: %v\n%s", err, stderr)
	}
	if _, stderr, err := h.runCLI(t, "task", "delete", failed.Ref); err != nil {
		t.Errorf("task delete after removing the worktree: %v\n%s", err, stderr)
	}
}

func workerRun(t *testing.T, ctx context.Context, db *store.Store, attemptID uuid.UUID) task.WorkerRun {
	t.Helper()
	runs, err := db.ListWorkerRuns(ctx, attemptID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListWorkerRuns = %v, %v", runs, err)
	}
	return runs[0]
}

func countEvents(t *testing.T, ctx context.Context, db *store.Store, taskID uuid.UUID) int {
	t.Helper()
	events, err := db.ListEvents(ctx, store.EventFilter{TaskID: taskID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return len(events)
}
