package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"aidev/internal/event"
	"aidev/internal/store"
	"aidev/internal/task"
)

func seedProject(t *testing.T, ctx context.Context, db *store.Store) task.Project {
	t.Helper()
	p, err := db.EnsureProject(ctx, "probe", "/tmp/aidev-probe-"+uuid.NewString(), "main")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return p
}

func seedTask(t *testing.T, ctx context.Context, db *store.Store, p task.Project, mutate func(*task.NewTaskInput)) task.Task {
	t.Helper()
	in := task.NewTaskInput{
		ProjectID:    p.ID,
		Title:        "Add a Greet function",
		Description:  "Create greet.go with Greet(name string) string",
		Verification: []task.VerificationStep{{Command: "go", Args: []string{"test", "./..."}}},
	}
	if mutate != nil {
		mutate(&in)
	}
	built, err := task.New(in, "build")
	if err != nil {
		t.Fatalf("task.New: %v", err)
	}
	created, err := db.CreateTask(ctx, built)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return created
}

func TestEnsureProjectIsIdempotent(t *testing.T) {
	db, ctx := openStore(t)

	path := "/tmp/aidev-project-" + uuid.NewString()
	first, err := db.EnsureProject(ctx, "aidev", path, "main")
	if err != nil {
		t.Fatalf("first EnsureProject: %v", err)
	}
	second, err := db.EnsureProject(ctx, "aidev", path, "main")
	if err != nil {
		t.Fatalf("second EnsureProject: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("registering the same repository twice created two projects: %s and %s", first.ID, second.ID)
	}

	byPath, err := db.GetProjectByPath(ctx, path)
	if err != nil {
		t.Fatalf("GetProjectByPath: %v", err)
	}
	if byPath.ID != first.ID {
		t.Errorf("GetProjectByPath returned %s, want %s", byPath.ID, first.ID)
	}

	if _, err := db.GetProject(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetProject for an unknown id = %v, want ErrNotFound", err)
	}
}

// A project defaults to leaving submodules alone, and the setting survives the
// EnsureProject that runs on every task creation — otherwise creating a task
// would silently undo an operator's choice.
func TestProjectSubmoduleModeIsStickyAcrossEnsureProject(t *testing.T) {
	db, ctx := openStore(t)

	path := "/tmp/aidev-submodules-" + uuid.NewString()
	created, err := db.EnsureProject(ctx, "aidev", path, "main")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if created.Submodules != task.SubmodulesNone {
		t.Errorf("a new project has submodules %q, want %q", created.Submodules, task.SubmodulesNone)
	}

	updated, err := db.SetProjectSubmodules(ctx, created.ID, task.SubmodulesReadOnly)
	if err != nil {
		t.Fatalf("SetProjectSubmodules: %v", err)
	}
	if updated.Submodules != task.SubmodulesReadOnly {
		t.Errorf("after setting, submodules = %q, want %q", updated.Submodules, task.SubmodulesReadOnly)
	}

	again, err := db.EnsureProject(ctx, "aidev", path, "main")
	if err != nil {
		t.Fatalf("second EnsureProject: %v", err)
	}
	if again.Submodules != task.SubmodulesReadOnly {
		t.Errorf("EnsureProject reset submodules to %q; an operator's setting must survive it",
			again.Submodules)
	}

	if _, err := db.SetProjectSubmodules(ctx, uuid.Must(uuid.NewV7()), task.SubmodulesReadOnly); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetProjectSubmodules for an unknown project = %v, want ErrNotFound", err)
	}
}

// The approval policy is the operator's (CLI only, migration 0008). It must
// default off, survive the EnsureProject every task creation runs, and report
// an unknown project rather than silently doing nothing.
func TestProjectApprovalPolicyIsStickyAcrossEnsureProject(t *testing.T) {
	db, ctx := openStore(t)

	path := "/tmp/aidev-approval-" + uuid.NewString()
	created, err := db.EnsureProject(ctx, "aidev", path, "main")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if created.RequiresApproval {
		t.Error("a new project requires approval, want the policy off by default")
	}

	updated, err := db.SetProjectRequiresApproval(ctx, created.ID, true)
	if err != nil {
		t.Fatalf("SetProjectRequiresApproval: %v", err)
	}
	if !updated.RequiresApproval {
		t.Error("after setting, requires_approval = false, want true")
	}

	again, err := db.EnsureProject(ctx, "aidev", path, "main")
	if err != nil {
		t.Fatalf("second EnsureProject: %v", err)
	}
	if !again.RequiresApproval {
		t.Error("EnsureProject reset the approval policy; an operator's choice must survive it")
	}

	if _, err := db.SetProjectRequiresApproval(ctx, uuid.Must(uuid.NewV7()), true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetProjectRequiresApproval for an unknown project = %v, want ErrNotFound", err)
	}
}

func TestTaskRoundTrip(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)

	created := seedTask(t, ctx, db, p, func(in *task.NewTaskInput) {
		in.AcceptanceCriteria = "Greet returns a greeting"
		in.Priority = 50
		in.MaxRetries = 2
		in.RequiresApproval = true
		in.ExpectFailOnBase = true
		in.BaseRef = "main"
		in.Timeout = 15 * time.Minute
		in.Verification = []task.VerificationStep{
			{Command: "go", Args: []string{"test", "./..."}},
			{Command: "go", Args: []string{"vet", "./..."}, TimeoutSeconds: 60},
		}
	})

	if created.Ref == "" {
		t.Fatal("the database did not assign a task reference")
	}
	if !strings.HasPrefix(created.Ref, "TASK-") {
		t.Errorf("ref = %q, want a TASK- prefix", created.Ref)
	}
	if created.Status != task.StatusPending {
		t.Errorf("status = %s, want PENDING", created.Status)
	}

	got, err := db.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Title != created.Title || got.AcceptanceCriteria != created.AcceptanceCriteria {
		t.Errorf("text fields did not survive the round trip: %+v", got)
	}
	if got.Priority != 50 || got.MaxRetries != 2 || !got.RequiresApproval || !got.ExpectFailOnBase || got.BaseRef != "main" {
		t.Errorf("scalar fields did not survive: %+v", got)
	}
	if got.Timeout != 15*time.Minute {
		t.Errorf("timeout = %s, want 15m", got.Timeout)
	}
	if len(got.Verification) != 2 {
		t.Fatalf("got %d verification steps, want 2", len(got.Verification))
	}
	if got.Verification[0].String() != "go test ./..." {
		t.Errorf("step 0 = %q", got.Verification[0].String())
	}
	if got.Verification[1].TimeoutSeconds != 60 {
		t.Errorf("step 1 timeout = %d, want 60", got.Verification[1].TimeoutSeconds)
	}
}

// A person types TASK-000001, a program passes a UUID. Both must resolve, and
// the reference must be case-insensitive so that shell history is forgiving.
func TestResolveTaskAcceptsRefAndUUID(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	created := seedTask(t, ctx, db, p, nil)

	for _, identifier := range []string{
		created.ID.String(),
		created.Ref,
		strings.ToLower(created.Ref),
		"  " + created.Ref + "  ",
	} {
		got, err := db.ResolveTask(ctx, identifier)
		if err != nil {
			t.Errorf("ResolveTask(%q): %v", identifier, err)
			continue
		}
		if got.ID != created.ID {
			t.Errorf("ResolveTask(%q) = %s, want %s", identifier, got.ID, created.ID)
		}
	}

	if _, err := db.ResolveTask(ctx, "TASK-999999"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown ref = %v, want ErrNotFound", err)
	}
	if _, err := db.ResolveTask(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("empty identifier = %v, want ErrNotFound", err)
	}
}

func TestTaskRefsAreSequential(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)

	first := seedTask(t, ctx, db, p, nil)
	second := seedTask(t, ctx, db, p, nil)
	if first.Ref == second.Ref {
		t.Fatalf("two tasks share the reference %s", first.Ref)
	}
	if first.Ref >= second.Ref {
		t.Errorf("refs are not increasing: %s then %s", first.Ref, second.Ref)
	}
}

func TestListTasksFilters(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	other := seedProject(t, ctx, db)

	a := seedTask(t, ctx, db, p, nil)
	seedTask(t, ctx, db, p, nil)
	seedTask(t, ctx, db, other, nil)

	all, err := db.ListTasks(ctx, store.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered list returned %d tasks, want 3", len(all))
	}

	byProject, err := db.ListTasks(ctx, store.TaskFilter{ProjectID: p.ID})
	if err != nil {
		t.Fatalf("ListTasks by project: %v", err)
	}
	if len(byProject) != 2 {
		t.Errorf("project filter returned %d tasks, want 2", len(byProject))
	}

	if err := db.TransitionTask(ctx, a.ID, task.StatusPending, task.StatusReady); err != nil {
		t.Fatalf("TransitionTask: %v", err)
	}
	ready, err := db.ListTasks(ctx, store.TaskFilter{Statuses: []task.Status{task.StatusReady}})
	if err != nil {
		t.Fatalf("ListTasks by status: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != a.ID {
		t.Errorf("status filter returned %d tasks, want only %s", len(ready), a.Ref)
	}

	if _, err := db.ListTasks(ctx, store.TaskFilter{Statuses: []task.Status{task.Status("BOGUS")}}); err == nil {
		t.Error("an invalid status filter was accepted")
	}

	limited, err := db.ListTasks(ctx, store.TaskFilter{Limit: 1})
	if err != nil {
		t.Fatalf("ListTasks with limit: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit 1 returned %d tasks", len(limited))
	}
}

// The compare-and-set is what lets concurrent workers be added later without
// two of them both believing they moved the same task.
func TestTransitionTaskIsCompareAndSet(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	created := seedTask(t, ctx, db, p, nil)

	if err := db.TransitionTask(ctx, created.ID, task.StatusPending, task.StatusReady); err != nil {
		t.Fatalf("first transition: %v", err)
	}

	// Replaying the same transition must lose, because the status has moved on.
	err := db.TransitionTask(ctx, created.ID, task.StatusPending, task.StatusReady)
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("replayed transition = %v, want ErrConflict", err)
	}
	if err != nil && !strings.Contains(err.Error(), "found READY") {
		t.Errorf("conflict error should name the status it found, got: %v", err)
	}

	// An illegal transition is rejected by the domain before touching the row.
	var te *task.TransitionError
	if err := db.TransitionTask(ctx, created.ID, task.StatusReady, task.StatusSucceeded); !errors.As(err, &te) {
		t.Errorf("READY -> SUCCEEDED = %v, want *task.TransitionError", err)
	}

	after, err := db.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if after.Status != task.StatusReady {
		t.Errorf("status = %s, want READY unchanged by the rejected transitions", after.Status)
	}

	if err := db.TransitionTask(ctx, uuid.Must(uuid.NewV7()), task.StatusPending, task.StatusReady); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("transition of an unknown task = %v, want ErrNotFound", err)
	}
}

// TransitionTask checks the state machine in Go, but the agent inherits
// database credentials until the sandbox work closes that path, so the
// database must refuse an illegal transition on its own: a raw UPDATE that
// bypasses the Go check has to fail too, or the guard is advisory.
func TestDatabaseRejectsIllegalStatusTransitions(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	tk := seedTask(t, ctx, db, p, nil)
	if tk.Status != task.StatusPending {
		t.Fatalf("seeded task status = %s, want PENDING", tk.Status)
	}

	// An illegal jump straight to success.
	_, err := db.Pool().Exec(ctx, `UPDATE tasks SET status = 'SUCCEEDED' WHERE id = $1`, tk.ID)
	if err == nil {
		t.Fatal("PENDING -> SUCCEEDED was stored; the transition guard is not enforced")
	}
	if !strings.Contains(err.Error(), "illegal task transition") {
		t.Errorf("rejection = %v, want the transition-guard message", err)
	}

	// The worker's own first step, and a legal way to stop, still go through.
	if _, err := db.Pool().Exec(ctx, `UPDATE tasks SET status = 'READY' WHERE id = $1`, tk.ID); err != nil {
		t.Fatalf("PENDING -> READY rejected: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE tasks SET status = 'CANCELLED' WHERE id = $1`, tk.ID); err != nil {
		t.Fatalf("READY -> CANCELLED rejected: %v", err)
	}

	// Terminal is terminal, even via raw SQL.
	if _, err := db.Pool().Exec(ctx, `UPDATE tasks SET status = 'READY' WHERE id = $1`, tk.ID); err == nil {
		t.Fatal("CANCELLED -> READY was stored; a terminal state can be left")
	}
}

func TestUpdatedAtMaintainedByDatabase(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	created := seedTask(t, ctx, db, p, nil)

	time.Sleep(10 * time.Millisecond)
	if err := db.TransitionTask(ctx, created.ID, task.StatusPending, task.StatusReady); err != nil {
		t.Fatalf("TransitionTask: %v", err)
	}
	after, err := db.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !after.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated_at did not advance (%s -> %s); the trigger is not firing",
			created.UpdatedAt, after.UpdatedAt)
	}
}

func TestAttemptLifecyclePersisted(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	tk := seedTask(t, ctx, db, p, nil)

	n, err := db.NextAttemptNumber(ctx, tk.ID)
	if err != nil {
		t.Fatalf("NextAttemptNumber: %v", err)
	}
	if n != 1 {
		t.Errorf("first attempt number = %d, want 1", n)
	}

	first, err := db.CreateAttempt(ctx, task.NewAttempt(tk.ID, n))
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if first.Status != task.AttemptRunning || first.FinishedAt != nil {
		t.Errorf("new attempt = %+v, want RUNNING and unfinished", first)
	}

	if err := db.FinishAttempt(ctx, first.ID, task.AttemptFailed, task.FailureVerification, "go test failed"); err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}

	reloaded, err := db.GetAttempt(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	if reloaded.Status != task.AttemptFailed {
		t.Errorf("status = %s, want FAILED", reloaded.Status)
	}
	if reloaded.FailureKind != task.FailureVerification {
		t.Errorf("failure kind = %s, want VERIFICATION", reloaded.FailureKind)
	}
	if reloaded.FinishedAt == nil {
		t.Error("finished_at was not set")
	}

	// Finishing twice must not silently rewrite the first outcome.
	err = db.FinishAttempt(ctx, first.ID, task.AttemptSucceeded, task.FailureNone, "")
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("double finish = %v, want ErrConflict", err)
	}
	if err := db.FinishAttempt(ctx, first.ID, task.AttemptRunning, task.FailureNone, ""); err == nil {
		t.Error("finishing an attempt as RUNNING was accepted")
	}

	// A second attempt takes the next number, which is what makes retry a
	// matter of appending rather than overwriting.
	n2, err := db.NextAttemptNumber(ctx, tk.ID)
	if err != nil {
		t.Fatalf("NextAttemptNumber: %v", err)
	}
	if n2 != 2 {
		t.Errorf("second attempt number = %d, want 2", n2)
	}
	if _, err := db.CreateAttempt(ctx, task.NewAttempt(tk.ID, n2)); err != nil {
		t.Fatalf("CreateAttempt second: %v", err)
	}

	attempts, err := db.ListAttempts(ctx, tk.ID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("got %d attempts, want 2", len(attempts))
	}
	if attempts[0].AttemptNumber != 2 {
		t.Errorf("ListAttempts should be newest first, got %d first", attempts[0].AttemptNumber)
	}

	latest, err := db.LatestAttempt(ctx, tk.ID)
	if err != nil {
		t.Fatalf("LatestAttempt: %v", err)
	}
	if latest.AttemptNumber != 2 {
		t.Errorf("LatestAttempt = %d, want 2", latest.AttemptNumber)
	}
}

func TestDuplicateAttemptNumberRejected(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	tk := seedTask(t, ctx, db, p, nil)

	if _, err := db.CreateAttempt(ctx, task.NewAttempt(tk.ID, 1)); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	_, err := db.CreateAttempt(ctx, task.NewAttempt(tk.ID, 1))
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("duplicate attempt number = %v, want ErrAlreadyExists", err)
	}
}

func TestWorktreeWorkerAndVerificationPersistence(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	tk := seedTask(t, ctx, db, p, nil)
	attempt, err := db.CreateAttempt(ctx, task.NewAttempt(tk.ID, 1))
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	wt, err := db.CreateWorktree(ctx, task.Worktree{
		ID:         uuid.Must(uuid.NewV7()),
		AttemptID:  attempt.ID,
		Path:       "/tmp/aidev-workspaces/" + tk.Ref,
		Branch:     "aidev/" + tk.Ref,
		BaseCommit: "b2d43534d0aedd28df35746ab0c64343220fb9e9",
		Status:     task.WorktreeActive,
	})
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if wt.RemovedAt != nil {
		t.Error("a new worktree must not have a removal time")
	}

	if err := db.SetWorktreeHead(ctx, wt.ID, "8c3b0b127c09133a3ce4b4d34e117c62490bebfd"); err != nil {
		t.Fatalf("SetWorktreeHead: %v", err)
	}

	// A retained worktree still exists on disk, so it must have no removal time.
	if err := db.SetWorktreeStatus(ctx, wt.ID, task.WorktreeRetained); err != nil {
		t.Fatalf("SetWorktreeStatus retained: %v", err)
	}
	got, err := db.GetWorktreeByAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("GetWorktreeByAttempt: %v", err)
	}
	if got.Status != task.WorktreeRetained || got.RemovedAt != nil {
		t.Errorf("retained worktree = %+v, want RETAINED with no removal time", got)
	}
	if got.HeadCommit == "" {
		t.Error("head commit was not persisted")
	}

	retained, err := db.ListRetainedWorktrees(ctx)
	if err != nil {
		t.Fatalf("ListRetainedWorktrees: %v", err)
	}
	if len(retained) != 1 {
		t.Errorf("got %d retained worktrees, want 1", len(retained))
	}

	if err := db.SetWorktreeStatus(ctx, wt.ID, task.WorktreeRemoved); err != nil {
		t.Fatalf("SetWorktreeStatus removed: %v", err)
	}
	got, err = db.GetWorktreeByAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("GetWorktreeByAttempt: %v", err)
	}
	if got.RemovedAt == nil {
		t.Error("a removed worktree must record when it was removed")
	}

	exit := 0
	cost := 0.0
	finished := time.Now().UTC()
	run, err := db.CreateWorkerRun(ctx, task.WorkerRun{
		ID:           uuid.Must(uuid.NewV7()),
		AttemptID:    attempt.ID,
		Backend:      "opencode",
		Status:       task.WorkerSucceeded,
		Command:      "opencode run --dir /tmp/wt --format json",
		WorkingDir:   "/tmp/wt",
		ExitCode:     &exit,
		Stdout:       `{"type":"text","part":{"text":"Done."}}`,
		Stderr:       "",
		SessionID:    "ses_f69583e28ffeaKEYp21iY6h9hp",
		Summary:      "Created greet.go",
		FinishReason: "stop",
		Tokens:       []byte(`{"input":8421,"output":76}`),
		Cost:         &cost,
		Diff:         "diff --git a/greet.go b/greet.go",
		ChangedFiles: 1,
		StartedAt:    time.Now().UTC().Add(-16 * time.Second),
		FinishedAt:   &finished,
	})
	if err != nil {
		t.Fatalf("CreateWorkerRun: %v", err)
	}
	if run.SessionID == "" {
		t.Error("the agent session id was not persisted; a future retry needs it")
	}
	if string(run.Tokens) == "" {
		t.Error("token usage was not persisted")
	}
	if run.ExitCode == nil || *run.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", run.ExitCode)
	}

	runs, err := db.ListWorkerRuns(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("ListWorkerRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d worker runs, want 1", len(runs))
	}

	failExit := 1
	for i, step := range []struct {
		status task.VerificationStatus
		exit   *int
	}{
		{task.VerificationPassed, &exit},
		{task.VerificationFailed, &failExit},
		{task.VerificationSkipped, nil},
	} {
		vFinished := time.Now().UTC()
		if _, err := db.CreateVerificationRun(ctx, task.VerificationRun{
			ID:         uuid.Must(uuid.NewV7()),
			AttemptID:  attempt.ID,
			StepIndex:  i,
			Phase:      task.PhaseVerify,
			Command:    fmt.Sprintf("go test ./step%d", i),
			Status:     step.status,
			ExitCode:   step.exit,
			Stdout:     "ok",
			Duration:   1500 * time.Millisecond,
			StartedAt:  time.Now().UTC(),
			FinishedAt: &vFinished,
		}); err != nil {
			t.Fatalf("CreateVerificationRun %d: %v", i, err)
		}
	}

	results, err := db.ListVerificationRuns(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("ListVerificationRuns: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d verification runs, want 3", len(results))
	}
	for i, r := range results {
		if r.StepIndex != i {
			t.Errorf("results are not in step order: position %d has step %d", i, r.StepIndex)
		}
	}
	if results[0].Duration != 1500*time.Millisecond {
		t.Errorf("duration = %s, want 1.5s", results[0].Duration)
	}
	// The phase column is new with setup_steps; a row that cannot name which
	// half of the sequence it belongs to would be unpageable.
	if results[0].Phase != task.PhaseVerify {
		t.Errorf("phase = %q, want %q", results[0].Phase, task.PhaseVerify)
	}
	if !results[0].Passed() || results[1].Passed() {
		t.Error("Passed() does not reflect the recorded statuses")
	}
	// A skipped step has no exit code, and must never look like a pass.
	if results[2].ExitCode != nil {
		t.Errorf("skipped step exit code = %v, want nil", results[2].ExitCode)
	}
}

func TestEventLogIsAppendOnlyAndOrdered(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	tk := seedTask(t, ctx, db, p, nil)
	attempt, err := db.CreateAttempt(ctx, task.NewAttempt(tk.ID, 1))
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	types := []event.Type{
		event.TypeTaskCreated, event.TypeTaskReady, event.TypeTaskStarted,
		event.TypeWorktreeCreated, event.TypeWorkerStarted, event.TypeWorkerCompleted,
		event.TypeVerificationStarted, event.TypeVerificationCompleted, event.TypeTaskSucceeded,
	}
	var lastSeq int64
	for i, ty := range types {
		e, err := event.New(tk.ID, &attempt.ID, ty, map[string]any{"step": i})
		if err != nil {
			t.Fatalf("event.New(%s): %v", ty, err)
		}
		written, err := db.AppendEvent(ctx, e)
		if err != nil {
			t.Fatalf("AppendEvent(%s): %v", ty, err)
		}
		if written.Seq <= lastSeq {
			t.Errorf("sequence did not increase: %d after %d", written.Seq, lastSeq)
		}
		lastSeq = written.Seq
	}

	history, err := db.ListEvents(ctx, store.EventFilter{TaskID: tk.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(history) != len(types) {
		t.Fatalf("got %d events, want %d", len(history), len(types))
	}
	for i, e := range history {
		if e.Type != types[i] {
			t.Errorf("event %d = %s, want %s (history must be in order)", i, e.Type, types[i])
		}
		if e.AttemptID == nil || *e.AttemptID != attempt.ID {
			t.Errorf("event %d lost its attempt id", i)
		}
	}

	// Resuming from a sequence returns only what came after it.
	tail, err := db.ListEvents(ctx, store.EventFilter{TaskID: tk.ID, AfterSeq: history[5].Seq})
	if err != nil {
		t.Fatalf("ListEvents after seq: %v", err)
	}
	if len(tail) != len(types)-6 {
		t.Errorf("got %d events after seq %d, want %d", len(tail), history[5].Seq, len(types)-6)
	}

	filtered, err := db.ListEvents(ctx, store.EventFilter{
		TaskID: tk.ID,
		Types:  []event.Type{event.TypeTaskSucceeded},
	})
	if err != nil {
		t.Fatalf("ListEvents filtered: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Type != event.TypeTaskSucceeded {
		t.Errorf("type filter returned %d events", len(filtered))
	}

	// The database itself must refuse to rewrite history.
	_, err = db.Pool().Exec(ctx, `UPDATE events SET type = 'task.failed' WHERE task_id = $1`, tk.ID)
	if err == nil {
		t.Fatal("an UPDATE on events succeeded; the append-only guarantee is not enforced")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("rejection = %v, want the append-only message", err)
	}

	if _, err := db.ListEvents(ctx, store.EventFilter{}); err == nil {
		t.Error("listing events without a task id was accepted")
	}
}

// Deleting a task must take its history with it: an audit log that cannot be
// pruned would eventually be an operational problem, and the cascade is only
// honest if the append-only trigger does not block it.
func TestDeletingATaskCascadesItsHistory(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	tk := seedTask(t, ctx, db, p, nil)

	e, err := event.New(tk.ID, nil, event.TypeTaskCreated, nil)
	if err != nil {
		t.Fatalf("event.New: %v", err)
	}
	if _, err := db.AppendEvent(ctx, e); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, `DELETE FROM tasks WHERE id = $1`, tk.ID); err != nil {
		t.Fatalf("deleting a task was blocked: %v", err)
	}

	var remaining int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id = $1`, tk.ID).Scan(&remaining); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d events survived the task deletion", remaining)
	}
}

// A task and its creation event must appear together or not at all, which is
// what the transaction is for.
func TestTransactionRollsBackEverything(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)

	sentinel := errors.New("deliberate failure after the writes")
	var createdID uuid.UUID

	err := db.InTx(ctx, func(tx *store.Store) error {
		built, err := task.New(task.NewTaskInput{
			ProjectID:    p.ID,
			Title:        "Rolled back",
			Verification: []task.VerificationStep{{Command: "true"}},
		}, "build")
		if err != nil {
			return err
		}
		created, err := tx.CreateTask(ctx, built)
		if err != nil {
			return err
		}
		createdID = created.ID

		e, err := event.New(created.ID, nil, event.TypeTaskCreated, map[string]any{"title": created.Title})
		if err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, e); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx returned %v, want the sentinel", err)
	}

	if _, err := db.GetTask(ctx, createdID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the task survived the rollback: %v", err)
	}
	var events int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id = $1`, createdID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("%d events survived the rollback", events)
	}
}

func TestTransactionCommitsEverything(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)

	var createdID uuid.UUID
	err := db.InTx(ctx, func(tx *store.Store) error {
		built, err := task.New(task.NewTaskInput{
			ProjectID:    p.ID,
			Title:        "Committed",
			Verification: []task.VerificationStep{{Command: "true"}},
		}, "build")
		if err != nil {
			return err
		}
		created, err := tx.CreateTask(ctx, built)
		if err != nil {
			return err
		}
		createdID = created.ID
		e, err := event.New(created.ID, nil, event.TypeTaskCreated, nil)
		if err != nil {
			return err
		}
		_, err = tx.AppendEvent(ctx, e)
		return err
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	if _, err := db.GetTask(ctx, createdID); err != nil {
		t.Errorf("the committed task is missing: %v", err)
	}
	history, err := db.ListEvents(ctx, store.EventFilter{TaskID: createdID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("got %d events, want 1", len(history))
	}
}

func TestNestedTransactionIsRefused(t *testing.T) {
	db, ctx := openStore(t)

	err := db.InTx(ctx, func(tx *store.Store) error {
		return tx.InTx(ctx, func(*store.Store) error { return nil })
	})
	if err == nil {
		t.Fatal("a nested InTx was accepted; the caller would believe it had isolation it does not have")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Errorf("error = %v, want it to explain nesting", err)
	}
}

func TestApprovalPolicyPersistence(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	tk := seedTask(t, ctx, db, p, func(in *task.NewTaskInput) { in.RequiresApproval = true })

	pending, err := db.RequestApproval(ctx, tk.ID, "task touches protected files")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if pending.Status != task.ApprovalPending || pending.DecidedAt != nil {
		t.Errorf("new approval = %+v, want PENDING and undecided", pending)
	}

	// Only one open request per task, enforced by the database.
	if _, err := db.RequestApproval(ctx, tk.ID, "again"); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("second pending request = %v, want ErrAlreadyExists", err)
	}

	granted, err := db.DecideApproval(ctx, tk.ID, task.ApprovalGranted, "thuyetmt", "")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if granted.Status != task.ApprovalGranted {
		t.Errorf("status = %s, want GRANTED", granted.Status)
	}
	if granted.DecidedAt == nil || granted.DecidedBy != "thuyetmt" {
		t.Errorf("decision was not recorded: %+v", granted)
	}
	// An empty reason must not erase the original request's reason.
	if granted.Reason != "task touches protected files" {
		t.Errorf("reason = %q, want the original request reason preserved", granted.Reason)
	}

	if _, err := db.DecideApproval(ctx, tk.ID, task.ApprovalGranted, "x", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deciding twice = %v, want ErrNotFound (no pending request)", err)
	}
	if _, err := db.DecideApproval(ctx, tk.ID, task.ApprovalPending, "x", ""); err == nil {
		t.Error("deciding a request as PENDING was accepted")
	}

	latest, err := db.LatestApproval(ctx, tk.ID)
	if err != nil {
		t.Fatalf("LatestApproval: %v", err)
	}
	if latest.ID != granted.ID {
		t.Errorf("LatestApproval = %s, want %s", latest.ID, granted.ID)
	}

	// After a decision a new request may be opened, which is what makes a
	// re-review possible.
	if _, err := db.RequestApproval(ctx, tk.ID, "second review"); err != nil {
		t.Fatalf("RequestApproval after a decision: %v", err)
	}
	all, err := db.ListApprovals(ctx, tk.ID)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("got %d approvals, want 2", len(all))
	}
}

// A task's verification list is required by the domain; the database must reject
// an empty one too, so that a future write path cannot bypass the rule.
func TestDatabaseRejectsTaskWithoutVerification(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)

	built, err := task.New(task.NewTaskInput{
		ProjectID:    p.ID,
		Title:        "No verification",
		Verification: []task.VerificationStep{{Command: "true"}},
	}, "build")
	if err != nil {
		t.Fatalf("task.New: %v", err)
	}
	built.Verification = nil // bypass the domain check the way a bug would

	if _, err := db.CreateTask(ctx, built); err == nil {
		t.Fatal("the database accepted a task with no verification steps")
	} else if !strings.Contains(err.Error(), "tasks_verification_is_nonempty_array") {
		t.Errorf("error = %v, want the verification constraint to be named", err)
	}
}

// BIGSERIAL assigns seq at INSERT time, not at COMMIT time. Without a lock, a
// transaction that appends an event for a task can commit after another
// transaction already appended a higher seq for the same task, and a reader
// paging with seq > last_seen would never come back for the lower one.
// AppendEvent must therefore hold a per-task lock from the insert until the
// appending transaction commits: same task blocks, different task does not.
func TestAppendEventHoldsPerTaskLockUntilCommit(t *testing.T) {
	db, ctx := openStore(t)
	p := seedProject(t, ctx, db)
	tkA := seedTask(t, ctx, db, p, nil)
	tkB := seedTask(t, ctx, db, p, nil)

	newReady := func(id uuid.UUID) event.Event {
		e, err := event.New(id, nil, event.TypeTaskReady, map[string]any{"probe": "lock"})
		if err != nil {
			t.Fatalf("event.New: %v", err)
		}
		return e
	}

	txStarted := make(chan struct{})
	release := make(chan struct{})
	txDone := make(chan error, 1)
	// Releasing is idempotent so a Fatalf on any assertion still lets the
	// transaction finish instead of holding the lock until the test binary
	// times out.
	var releaseOnce sync.Once
	giveUp := func() { releaseOnce.Do(func() { close(release) }) }
	defer giveUp()

	go func() {
		txDone <- db.InTx(ctx, func(tx *store.Store) error {
			if _, err := tx.AppendEvent(ctx, newReady(tkA.ID)); err != nil {
				return err
			}
			close(txStarted)
			<-release
			return nil
		})
	}()
	select {
	case <-txStarted:
	case err := <-txDone:
		t.Fatalf("InTx failed before it could hold the lock: %v", err)
	}

	// Same task, transaction still open: the insert must wait for the commit.
	blockCtx, cancelBlock := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelBlock()
	_, err := db.AppendEvent(blockCtx, newReady(tkA.ID))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-task append err = %v, want context.DeadlineExceeded (blocked on the advisory lock)", err)
	}

	// A different task must not wait: the lock is keyed per task, so
	// concurrent tasks never serialise each other's history writes.
	waitCtx, cancelWait := context.WithTimeout(ctx, 5*time.Second)
	defer cancelWait()
	if _, err := db.AppendEvent(waitCtx, newReady(tkB.ID)); err != nil {
		t.Fatalf("cross-task append while the lock is held: %v", err)
	}

	giveUp()
	if err := <-txDone; err != nil {
		t.Fatalf("InTx: %v", err)
	}

	if _, err := db.AppendEvent(ctx, newReady(tkA.ID)); err != nil {
		t.Fatalf("append after the commit: %v", err)
	}

	// The committed event sorts before anything appended after the commit, so
	// a reader resuming from its seq sees exactly the later one.
	history, err := db.ListEvents(ctx, store.EventFilter{TaskID: tkA.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("got %d events for task A, want 2 (one from the transaction, one after)", len(history))
	}
	if history[0].Seq >= history[1].Seq {
		t.Errorf("seq order = %d then %d, want the committed event first", history[0].Seq, history[1].Seq)
	}
}
