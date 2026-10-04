package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"aidev/internal/store"
	"aidev/internal/task"
)

// A lease is what tells a live run apart from a dead one, so the run itself has
// to keep one: an attempt with no owner and no expiry could not be renewed, and
// recovering it would be a guess (research C1).
func TestRunLeasesItsAttempt(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED", outcome.Task.Status)
	}

	attempt, err := h.store.LatestAttempt(h.ctx, created.ID)
	if err != nil {
		t.Fatalf("LatestAttempt: %v", err)
	}
	if attempt.LeaseOwner == "" {
		t.Fatal("the attempt has no lease owner, so nobody can renew it")
	}
	// The owner names the process holding the lease: hostname:pid:uuid, so a
	// recovery message says who to look at.
	if want := fmt.Sprintf(":%d:", os.Getpid()); !strings.Contains(attempt.LeaseOwner, want) {
		t.Errorf("lease owner %q does not name this process (%q in it)", attempt.LeaseOwner, want)
	}
	if attempt.LeaseExpiresAt == nil {
		t.Fatal("the attempt has no lease expiry, so it could never be found expired")
	}
	if !attempt.LeaseExpiresAt.After(attempt.StartedAt) {
		t.Errorf("lease expires at %s, not after the attempt started at %s", attempt.LeaseExpiresAt, attempt.StartedAt)
	}
}

// Renewing is the whole protocol: the holder's tick extends the lease, and the
// answer carries the task's current status so the same tick also learns that
// the run was cancelled or finished elsewhere.
func TestRenewLeaseExtendsTheLeaseAndReportsStatus(t *testing.T) {
	h := newHarness(t, nil)
	const owner = "testhost:1234:01900000-0000-7000-8000-000000000000"

	expired := time.Now().UTC().Add(-time.Minute)
	stuck, attempt, _ := crashDuringRun(t, h, func(a *task.TaskAttempt) {
		a.LeaseOwner = owner
		a.LeaseExpiresAt = &expired
	})

	// The holder's own tick extends the lease and reports the task still runs.
	status, err := h.store.RenewLease(h.ctx, attempt.ID, owner, 30*time.Second)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if status != task.StatusRunning {
		t.Errorf("status = %s, want RUNNING", status)
	}
	renewed, err := h.store.GetAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.LeaseExpiresAt == nil || !renewed.LeaseExpiresAt.After(time.Now().UTC()) {
		t.Errorf("lease expires at %v, want it pushed into the future", renewed.LeaseExpiresAt)
	}
	if renewed.LeaseOwner != owner {
		t.Errorf("lease owner = %q, want %q", renewed.LeaseOwner, owner)
	}

	// A stranger's tick must not extend someone else's lease — otherwise two
	// processes both believe they hold the run. It still gets the status back,
	// so the answer stays useful even when nothing was renewed.
	if _, err := h.store.RenewLease(h.ctx, attempt.ID, "someone-else", 30*time.Second); err != nil {
		t.Fatalf("RenewLease with a foreign owner: %v", err)
	}
	unchanged, err := h.store.GetAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.LeaseExpiresAt.Equal(*renewed.LeaseExpiresAt) {
		t.Errorf("a foreign owner moved the lease to %s", unchanged.LeaseExpiresAt)
	}

	// After the task ends, the tick reports that instead of renewing: this is
	// how a watcher whose task was cancelled elsewhere learns to stop.
	if err := h.store.TransitionTask(h.ctx, stuck.ID, task.StatusRunning, task.StatusCancelled); err != nil {
		t.Fatal(err)
	}
	status, err = h.store.RenewLease(h.ctx, attempt.ID, owner, 30*time.Second)
	if err != nil {
		t.Fatalf("RenewLease after the task ended: %v", err)
	}
	if status != task.StatusCancelled {
		t.Errorf("status after the task ended = %s, want CANCELLED", status)
	}
}

// The command the manual says to run when a run was killed: it finds the tasks
// no process is checking in on, cancels them through the normal path, and
// leaves the work for inspection.
func TestRecoverCancelsTasksWithAnExpiredLease(t *testing.T) {
	h := newHarness(t, nil)
	// No prepare: an attempt with no lease at all is one nobody ever
	// heartbeated, which is exactly what a kill before the first tick leaves.
	stuck, attempt, record := crashDuringRun(t, h)

	stdout, _, err := h.runCLI(t, "task", "recover")
	if err != nil {
		t.Fatalf("task recover: %v", err)
	}
	if !strings.Contains(stdout, stuck.Ref) {
		t.Errorf("recover does not report the stuck task:\n%s", stdout)
	}
	if !strings.Contains(stdout, "lease expired") {
		t.Errorf("recover does not say why it cancelled:\n%s", stdout)
	}

	reloaded, err := h.store.GetTask(h.ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED", reloaded.Status)
	}
	reloadedAttempt, err := h.store.GetAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedAttempt.Status != task.AttemptCancelled || reloadedAttempt.FinishedAt == nil {
		t.Errorf("attempt = %s (finished %v), want closed as CANCELLED", reloadedAttempt.Status, reloadedAttempt.FinishedAt)
	}
	reloadedWorktree, err := h.store.GetWorktreeByAttempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedWorktree.Status != task.WorktreeRetained {
		t.Errorf("worktree = %s, want RETAINED", reloadedWorktree.Status)
	}
	if _, err := os.Stat(record.Path); err != nil {
		t.Errorf("the interrupted work was destroyed: %v", err)
	}

	// The history records the evidence, so `task events` answers "why was this
	// cancelled" without anyone reconstructing the lease columns.
	events, err := h.store.ListEvents(h.ctx, store.EventFilter{TaskID: stuck.ID})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Type.String() != "task.cancelled" {
			continue
		}
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decode event payload: %v (%s)", err, e.Payload)
		}
		if strings.Contains(payload.Reason, "lease expired: owner") {
			found = true
		}
	}
	if !found {
		t.Error("no task.cancelled event explains the lease that expired")
	}
}

// A task whose process is alive has a lease in the future. Recovering it would
// cancel a run that is working, which is the opposite of what it is for.
func TestRecoverLeavesALiveLeaseAlone(t *testing.T) {
	h := newHarness(t, nil)
	expires := time.Now().UTC().Add(time.Hour)
	stuck, _, _ := crashDuringRun(t, h, func(a *task.TaskAttempt) {
		a.LeaseOwner = "somehost:1:01900000-0000-7000-8000-000000000000"
		a.LeaseExpiresAt = &expires
	})

	stdout, _, err := h.runCLI(t, "task", "recover")
	if err != nil {
		t.Fatalf("task recover: %v", err)
	}
	if !strings.Contains(stdout, "no tasks with an expired lease") {
		t.Errorf("recover reported something for a live lease:\n%s", stdout)
	}
	reloaded, err := h.store.GetTask(h.ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusRunning {
		t.Errorf("status = %s, want the running task left alone", reloaded.Status)
	}

	if _, _, err := h.runCLI(t, "task", "cancel", stuck.Ref, "--reason", "test cleanup"); err != nil {
		t.Fatalf("task cancel: %v", err)
	}
}

// A command that cancels unattended must be inspectable before it acts:
// --dry-run lists without cancelling, --json is the machine form, and only the
// plain invocation cancels.
func TestRecoverDryRunListsWithoutCancelling(t *testing.T) {
	h := newHarness(t, nil)
	stuck, attempt, _ := crashDuringRun(t, h)

	// The flag is named in the help, or nobody learns it exists.
	_, stderr, _ := h.runCLI(t, "task", "recover", "-h")
	if !strings.Contains(stderr, "dry-run") {
		t.Errorf("task recover -h does not mention -dry-run:\n%s", stderr)
	}

	stdout, _, err := h.runCLI(t, "task", "recover", "--dry-run")
	if err != nil {
		t.Fatalf("task recover --dry-run: %v", err)
	}
	if !strings.Contains(stdout, stuck.Ref) || !strings.Contains(stdout, "would cancel") {
		t.Errorf("dry-run does not list what it would cancel:\n%s", stdout)
	}
	reloaded, err := h.store.GetTask(h.ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusRunning {
		t.Fatalf("dry-run changed the task to %s", reloaded.Status)
	}

	stdout, _, err = h.runCLI(t, "task", "recover", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("task recover --json: %v", err)
	}
	var listed struct {
		DryRun bool `json:"dry_run"`
		Tasks  []struct {
			Ref    string `json:"ref"`
			Action string `json:"action"`
			Reason string `json:"reason"`
		} `json:"tasks"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &listed); jsonErr != nil {
		t.Fatalf("--json output is not that shape: %v\n%s", jsonErr, stdout)
	}
	if !listed.DryRun || len(listed.Tasks) != 1 || listed.Tasks[0].Ref != stuck.Ref {
		t.Errorf("dry-run JSON = %+v, want dry_run with this task", listed)
	}
	if listed.Tasks[0].Action != "would_cancel" || !strings.Contains(listed.Tasks[0].Reason, "lease expired") {
		t.Errorf("task = %+v, want action would_cancel and the lease as the reason", listed.Tasks[0])
	}

	// The dry runs changed nothing, so the real one still finds the task.
	stdout, _, err = h.runCLI(t, "task", "recover")
	if err != nil {
		t.Fatalf("task recover: %v", err)
	}
	if !strings.Contains(stdout, stuck.Ref) || !strings.Contains(stdout, "cancelled") {
		t.Errorf("recover did not cancel:\n%s", stdout)
	}
	reloaded, err = h.store.GetTask(h.ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED", reloaded.Status)
	}
	if _, err := h.store.GetAttempt(h.ctx, attempt.ID); err != nil {
		t.Fatal(err)
	}
}

// Doctor must surface the stuck tasks the recovery command exists for — as a
// warning, because a stuck task does not stop aidev from working, and an
// operator has to be able to read the rest of the checks first.
func TestDoctorReportsStuckTasks(t *testing.T) {
	h := newHarness(t, nil)
	stuck, _, _ := crashDuringRun(t, h)

	stdout, stderr, err := h.runCLI(t, "doctor")
	if err != nil {
		t.Fatalf("doctor exited non-zero on a warning: %v\n%s\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "stuck tasks") {
		t.Errorf("doctor does not list the stuck-tasks check:\n%s", stdout)
	}
	if !strings.Contains(stdout, stuck.Ref) {
		t.Errorf("doctor does not name the stuck task:\n%s", stdout)
	}
	if !strings.Contains(stdout, "aidev task recover") {
		t.Errorf("doctor does not say how to recover:\n%s", stdout)
	}

	if _, _, err := h.runCLI(t, "task", "cancel", stuck.Ref, "--reason", "test cleanup"); err != nil {
		t.Fatalf("task cancel: %v", err)
	}
}
