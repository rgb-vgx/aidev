package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Automatic retry (research F; design settled with the user on 2026-09-17):
// an attempt that fails in a way another try can fix, on a task with
// max_retries, is recorded as failed and the next attempt starts in the same
// run — same worktree directory, a new branch, the agent's session continued
// with the failure in the prompt. The task itself never passes through
// FAILED on the way.

func withRetries(n int) func(*worker.CreateTaskInput) {
	return func(in *worker.CreateTaskInput) { in.MaxRetries = n }
}

func TestRetryContinuesAfterFailedChecks(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.SessionID = "ses_retry"
	var calls atomic.Int32
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if calls.Add(1) == 1 {
			// The first attempt stops halfway: some work, no marker.
			return os.WriteFile(filepath.Join(req.WorkingDir, "wip.txt"), []byte("half\n"), 0o600)
		}
		return doTheWork(ctx, req)
	}

	created := h.createTask(withRetries(1))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED on the retry: %s", outcome.Task.Status, outcome.Message)
	}
	if !strings.Contains(outcome.Message, "on attempt 2") {
		t.Errorf("message = %q, want it to say which attempt passed", outcome.Message)
	}

	// The second call continues the first: same directory, same session,
	// and a prompt that says what failed instead of repeating the task.
	reqs := h.backend.Calls()
	if len(reqs) != 2 {
		t.Fatalf("the agent was called %d times, want 2", len(reqs))
	}
	if reqs[1].WorkingDir != reqs[0].WorkingDir {
		t.Errorf("retry ran in %s, want the first attempt's directory %s", reqs[1].WorkingDir, reqs[0].WorkingDir)
	}
	if reqs[0].SessionID != "" || reqs[1].SessionID != "ses_retry" {
		t.Errorf("sessions = %q then %q, want a fresh one then ses_retry continued", reqs[0].SessionID, reqs[1].SessionID)
	}
	for _, want := range []string{"Attempt 2", "test -f marker.txt", "failed"} {
		if !strings.Contains(reqs[1].Prompt, want) {
			t.Errorf("retry prompt lacks %q:\n%s", want, reqs[1].Prompt)
		}
	}
	if strings.Contains(reqs[1].Prompt, "You are implementing one specific change") {
		t.Error("the retry prompt repeats the whole task although the session that has it was continued")
	}

	attempts := attemptsInOrder(t, h, created.ID)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts, want 2", len(attempts))
	}
	if attempts[0].Status != task.AttemptFailed || attempts[0].FailureKind != task.FailureVerification {
		t.Errorf("attempt 1 = %s/%s, want FAILED/VERIFICATION", attempts[0].Status, attempts[0].FailureKind)
	}
	if attempts[1].Status != task.AttemptSucceeded {
		t.Errorf("attempt 2 = %s, want SUCCEEDED", attempts[1].Status)
	}

	// Each attempt's branch holds its end state: the first, unverified and
	// marked so; the second, the delivery, built on top of the first.
	first, second := "aidev/"+created.Ref, "aidev/"+created.Ref+"-a2"
	if files := h.branchFiles(first); !strings.Contains(files, "wip.txt") || strings.Contains(files, "marker.txt") {
		t.Errorf("%s holds %q, want attempt 1's wip.txt and no marker", first, files)
	}
	if msg := gitRun(t, h.repoPath, "log", "-1", "--format=%s", first); !strings.Contains(msg, "not verified") {
		t.Errorf("attempt 1's commit message = %q, want it marked as not verified", msg)
	}
	if files := h.branchFiles(second); !strings.Contains(files, "wip.txt") || !strings.Contains(files, "marker.txt") {
		t.Errorf("%s holds %q, want both attempts' work", second, files)
	}
	if outcome.Worktree == nil || outcome.Worktree.Branch != second {
		t.Errorf("outcome worktree = %+v, want the delivery on %s", outcome.Worktree, second)
	}

	// The first attempt's record hands the directory over; the second owns
	// it and removed it on success.
	wt1, err := h.store.GetWorktreeByAttempt(h.ctx, attempts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	wt2, err := h.store.GetWorktreeByAttempt(h.ctx, attempts[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt1.Status != task.WorktreeReused || wt1.Path != wt2.Path || wt1.HeadCommit == "" {
		t.Errorf("attempt 1 worktree = %s at %s head %q, want REUSED at %s with its partial commit",
			wt1.Status, wt1.Path, wt1.HeadCommit, wt2.Path)
	}
	if wt2.Status != task.WorktreeRemoved {
		t.Errorf("attempt 2 worktree = %s, want REMOVED after success", wt2.Status)
	}
	if wt2.BaseCommit != wt1.BaseCommit {
		t.Errorf("attempt 2 base = %s, want the task's original base %s", wt2.BaseCommit, wt1.BaseCommit)
	}

	history := h.eventTypes(created.ID)
	if contains(history, "task.failed") {
		t.Errorf("history records task.failed although the task retried and succeeded: %v", history)
	}
	r, s := indexOf(history, "task.retry_scheduled"), lastIndexOf(history, "task.started")
	if r < 0 || s < r {
		t.Errorf("history = %v, want task.retry_scheduled before the second task.started", history)
	}
	payload := h.eventPayload(created.ID, "task.retry_scheduled")
	if payload["failure_kind"] != "VERIFICATION" || payload["next_attempt"] != float64(2) {
		t.Errorf("task.retry_scheduled payload = %v", payload)
	}
}

func TestRetryStopsAfterMaxRetries(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(context.Context, agent.Request) error { return nil } // never makes the marker

	created := h.createTask(withRetries(2))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED once the retries ran out", outcome.Task.Status)
	}
	if n := len(h.backend.Calls()); n != 3 {
		t.Errorf("the agent was called %d times, want 3 (1 + max_retries 2)", n)
	}
	attempts := attemptsInOrder(t, h, created.ID)
	if len(attempts) != 3 {
		t.Fatalf("%d attempts, want 3", len(attempts))
	}
	for i, a := range attempts {
		wt, err := h.store.GetWorktreeByAttempt(h.ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := task.WorktreeReused
		if i == len(attempts)-1 {
			want = task.WorktreeRetained // the last one keeps the directory for a person
		}
		if a.Status != task.AttemptFailed || wt.Status != want {
			t.Errorf("attempt %d = %s, worktree %s; want FAILED and %s", i+1, a.Status, wt.Status, want)
		}
	}
	history := h.eventTypes(created.ID)
	if n := countOf(history, "task.retry_scheduled"); n != 2 {
		t.Errorf("%d retries scheduled, want 2: %v", n, history)
	}
	if n := countOf(history, "task.failed"); n != 1 {
		t.Errorf("%d task.failed events, want exactly 1 at the end: %v", n, history)
	}
}

func TestNoRetryWithoutMaxRetries(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(context.Context, agent.Request) error { return nil }

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed || len(h.backend.Calls()) != 1 {
		t.Errorf("status %s after %d call(s), want FAILED after 1: retry is opt-in", outcome.Task.Status, len(h.backend.Calls()))
	}
}

// An agent that stopped early is retried; the prompt carries the error,
// and with no session to continue it carries the whole task too.
func TestRetryAfterTheAgentStoppedEarly(t *testing.T) {
	h := newHarness(t, nil)
	var calls atomic.Int32
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if calls.Add(1) == 1 {
			return errors.New("opencode ended with finish reason \"tool-calls\"")
		}
		return doTheWork(ctx, req)
	}

	created := h.createTask(withRetries(1))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED on the retry: %s", outcome.Task.Status, outcome.Message)
	}
	reqs := h.backend.Calls()
	if len(reqs) != 2 {
		t.Fatalf("%d calls, want 2", len(reqs))
	}
	for _, want := range []string{"finish reason", "You are implementing one specific change"} {
		if !strings.Contains(reqs[1].Prompt, want) {
			t.Errorf("retry prompt without a session lacks %q:\n%s", want, reqs[1].Prompt)
		}
	}
}

// Failures another try cannot fix are not retried, however many retries the
// task allows.
func TestFailuresAnotherTryCannotFixAreNotRetried(t *testing.T) {
	t.Run("a refused tool call", func(t *testing.T) {
		h := newHarness(t, nil)
		h.backend.Work = func(context.Context, agent.Request) error {
			return fmt.Errorf("opencode stopped: %w", agent.ErrToolRefused)
		}
		created := h.createTask(withRetries(3))
		outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
		if err != nil {
			t.Fatalf("RunTask: %v", err)
		}
		if outcome.Task.Status != task.StatusFailed || len(h.backend.Calls()) != 1 {
			t.Errorf("status %s after %d call(s), want FAILED after 1", outcome.Task.Status, len(h.backend.Calls()))
		}
	})

	t.Run("an intercepted runner", func(t *testing.T) {
		h := newHarness(t, nil)
		commitCheckScript(h)
		h.backend.Work = func(ctx context.Context, req agent.Request) error {
			return os.WriteFile(filepath.Join(req.WorkingDir, "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
		}
		created := h.createTask(func(in *worker.CreateTaskInput) {
			verifyWith("./check.sh")(in)
			in.MaxRetries = 3
		})
		outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
		if err != nil {
			t.Fatalf("RunTask: %v", err)
		}
		if outcome.Task.Status != task.StatusFailed || len(h.backend.Calls()) != 1 {
			t.Errorf("status %s after %d call(s), want FAILED after 1: a refusal to verify is not a failed check",
				outcome.Task.Status, len(h.backend.Calls()))
		}
	})
}

// attemptsInOrder lists a task's attempts first to last; the store lists the
// newest first.
func attemptsInOrder(t *testing.T, h *harness, taskID uuid.UUID) []task.TaskAttempt {
	t.Helper()
	attempts, err := h.store.ListAttempts(h.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].AttemptNumber < attempts[j].AttemptNumber })
	return attempts
}

func lastIndexOf(haystack []string, needle string) int {
	for i := len(haystack) - 1; i >= 0; i-- {
		if haystack[i] == needle {
			return i
		}
	}
	return -1
}

func countOf(haystack []string, needle string) int {
	n := 0
	for _, v := range haystack {
		if v == needle {
			n++
		}
	}
	return n
}

// The cancel watch renews the lease of the attempt that is running. A retry
// starts a new attempt while the watch keeps going; were the watch still
// renewing the first attempt, the second one's lease would lapse and
// `aidev task recover` would cancel a run that is alive (research C1).
func TestRetryKeepsTheNewAttemptsLeaseAlive(t *testing.T) {
	h := newHarness(t, nil)
	h.orchestrator.CancelPoll = 20 * time.Millisecond
	var calls atomic.Int32
	var before, after time.Time
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if calls.Add(1) == 1 {
			return nil // fails the checks: no marker
		}
		read := func() time.Time {
			a, err := h.store.LatestAttempt(ctx, h.lastTaskID)
			if err != nil || a.LeaseExpiresAt == nil {
				t.Errorf("LatestAttempt = %+v, %v", a, err)
				return time.Time{}
			}
			return *a.LeaseExpiresAt
		}
		before = read()
		time.Sleep(200 * time.Millisecond)
		after = read()
		return doTheWork(ctx, req)
	}

	created := h.createTask(withRetries(1))
	h.lastTaskID = created.ID
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED", outcome.Task.Status)
	}
	if !after.After(before) {
		t.Errorf("the second attempt's lease went from %s to %s while it ran, want it renewed", before, after)
	}
}

// What the failed attempt's checks wrote must not reach the delivery through
// the retry: the next attempt starts from the failed attempt's commit, not
// from the directory as its verification left it (research A6).
func TestRetryDoesNotCarryVerificationOutputIntoTheCommit(t *testing.T) {
	h := newHarness(t, nil)
	var calls atomic.Int32
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if calls.Add(1) == 1 {
			return nil
		}
		return doTheWork(ctx, req)
	}
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.MaxRetries = 1
		in.Verification = []task.VerificationStep{shell("echo 'mode: set' > cover.out; test -f marker.txt")}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: %s", outcome.Task.Status, outcome.Message)
	}
	files := h.branchFiles("aidev/" + created.Ref + "-a2")
	if !strings.Contains(files, "marker.txt") {
		t.Errorf("the delivery lacks marker.txt:\n%s", files)
	}
	if strings.Contains(files, "cover.out") {
		t.Errorf("the delivery carries cover.out, which only the first attempt's checks wrote:\n%s", files)
	}
}

// After a retried task fails for good, a person removes the last attempt's
// worktree; the earlier REUSED records, which shared that directory, must
// not then keep the task from being deleted.
func TestARetriedTaskCanBeCleanedUpAndDeleted(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(context.Context, agent.Request) error { return nil }
	created := h.createTask(withRetries(1))
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if _, stderr, err := h.runCLI(t, "worktree", "remove", created.Ref, "--force"); err != nil {
		t.Fatalf("worktree remove: %v\n%s", err, stderr)
	}
	stdout, stderr, err := h.runCLI(t, "worktree", "list", "--all")
	if err != nil {
		t.Fatalf("worktree list: %v\n%s", err, stderr)
	}
	if strings.Contains(stdout, "missing from disk") {
		t.Errorf("worktree list flags a REUSED record whose directory went with the last attempt:\n%s", stdout)
	}
	if _, stderr, err := h.runCLI(t, "task", "delete", created.Ref); err != nil {
		t.Errorf("task delete after removing the last worktree: %v\n%s", err, stderr)
	}
}
