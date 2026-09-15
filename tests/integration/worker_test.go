package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/worker"
)

func TestHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	h.backend.Summary = "Created marker.txt"
	h.backend.SessionID = "ses_test_happy_path"
	h.backend.FinishReason = "stop"

	created := h.createTask(nil)
	if created.Status != task.StatusPending {
		t.Fatalf("a new task is %s, want PENDING", created.Status)
	}

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: %s", outcome.Task.Status, outcome.Message)
	}

	// The agent was pointed at a worktree under the workspace root, not at the
	// repository.
	call, ok := h.backend.LastCall()
	if !ok {
		t.Fatal("the backend was never called")
	}
	if !strings.HasPrefix(call.WorkingDir, h.workspace) {
		t.Errorf("agent working dir = %q, want it under %q", call.WorkingDir, h.workspace)
	}
	if call.WorkingDir == h.repoPath {
		t.Error("the agent was run in the repository itself")
	}
	if !h.repoIsClean() {
		t.Error("the repository's working tree was modified")
	}
	if _, err := os.Stat(filepath.Join(h.repoPath, "marker.txt")); !os.IsNotExist(err) {
		t.Error("the agent's file appeared in the repository")
	}

	// The prompt told the agent what would judge it.
	if !strings.Contains(call.Prompt, "test -f marker.txt") {
		t.Errorf("prompt does not contain the verification command:\n%s", call.Prompt)
	}
	if !strings.Contains(call.Prompt, created.Ref) {
		t.Error("prompt does not identify the task")
	}

	// Verification results are persisted, and they are aidev's own.
	if outcome.Verification == nil || !outcome.Verification.Passed {
		t.Fatalf("verification report = %+v, want a pass", outcome.Verification)
	}
	runs, err := h.store.ListVerificationRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatalf("ListVerificationRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != task.VerificationPassed {
		t.Errorf("persisted verification = %+v, want one PASSED row", runs)
	}
	if runs[0].ExitCode == nil || *runs[0].ExitCode != 0 {
		t.Errorf("verification exit code = %v, want 0", runs[0].ExitCode)
	}

	// The worker run is recorded, with the diff aidev collected itself.
	workerRuns, err := h.store.ListWorkerRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatalf("ListWorkerRuns: %v", err)
	}
	if len(workerRuns) != 1 {
		t.Fatalf("got %d worker runs, want 1", len(workerRuns))
	}
	wr := workerRuns[0]
	if wr.Status != task.WorkerSucceeded {
		t.Errorf("worker status = %s", wr.Status)
	}
	if wr.SessionID != "ses_test_happy_path" {
		t.Errorf("session id = %q, want the backend's, recorded for a future retry", wr.SessionID)
	}
	if wr.ChangedFiles != 1 {
		t.Errorf("changed files = %d, want 1 (collected from git, not claimed by the agent)", wr.ChangedFiles)
	}
	if !strings.Contains(wr.Diff, "marker.txt") {
		t.Errorf("diff does not mention the new file:\n%s", wr.Diff)
	}

	// The attempt is closed successfully.
	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	if attempt.Status != task.AttemptSucceeded || attempt.FinishedAt == nil {
		t.Errorf("attempt = %+v, want SUCCEEDED and finished", attempt)
	}

	// Cleanup policy: the work is committed to the task branch and the worktree
	// directory is gone.
	branch := "aidev/" + created.Ref
	if !h.branchExists(branch) {
		t.Fatalf("branch %s does not exist", branch)
	}
	if !strings.Contains(h.branchFiles(branch), "marker.txt") {
		t.Errorf("the branch does not contain the work:\n%s", h.branchFiles(branch))
	}
	if _, err := os.Stat(call.WorkingDir); !os.IsNotExist(err) {
		t.Error("the worktree directory still exists after a successful run")
	}
	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatalf("GetWorktreeByAttempt: %v", err)
	}
	if wt.Status != task.WorktreeRemoved || wt.RemovedAt == nil {
		t.Errorf("worktree record = %+v, want REMOVED with a removal time", wt)
	}
	if wt.HeadCommit == "" || wt.HeadCommit == wt.BaseCommit {
		t.Errorf("head commit = %q, base = %q; want the new commit recorded", wt.HeadCommit, wt.BaseCommit)
	}

	// History tells the whole story, in order.
	history := h.eventTypes(created.ID)
	expected := []string{
		"task.created", "task.ready", "task.started", "task.worktree_created",
		"task.worker_started", "task.worker_completed", "task.verification_started",
		"task.verification_step_completed", "task.verification_completed",
		"task.worktree_removed", "task.succeeded",
	}
	for _, want := range expected {
		if !contains(history, want) {
			t.Errorf("history is missing %s; got %v", want, history)
		}
	}
	if a, b := indexOf(history, "task.verification_completed"), indexOf(history, "task.succeeded"); a < 0 || b < 0 || a > b {
		t.Errorf("succeeded is not recorded after verification: %v", history)
	}
	if a, b := indexOf(history, "task.worker_completed"), indexOf(history, "task.verification_started"); a < 0 || b < 0 || a > b {
		t.Errorf("verification is not recorded after the agent finished: %v", history)
	}
}

// The central claim of the product, as a test: an agent that reports success
// without doing the work does not produce a successful task.
func TestAgentClaimingSuccessWithoutDoingTheWorkFails(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = nil // does nothing at all
	h.backend.Status = task.WorkerSucceeded
	h.backend.Summary = "All done! Tests pass."
	h.backend.FinishReason = "stop"

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: the agent's claim must not decide the outcome", outcome.Task.Status)
	}

	// The agent's own run is still recorded as a success: it did finish. What
	// changes the outcome is verification.
	workerRuns, err := h.store.ListWorkerRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workerRuns) != 1 || workerRuns[0].Status != task.WorkerSucceeded {
		t.Errorf("worker run = %+v, want the agent's own run recorded as SUCCEEDED", workerRuns)
	}
	if workerRuns[0].Summary != "All done! Tests pass." {
		t.Errorf("the agent's claim was not recorded verbatim: %q", workerRuns[0].Summary)
	}

	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.FailureKind != task.FailureVerification {
		t.Errorf("failure kind = %s, want VERIFICATION", attempt.FailureKind)
	}

	runs, err := h.store.ListVerificationRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != task.VerificationFailed {
		t.Errorf("verification = %+v, want one FAILED row", runs)
	}
}

// A failed attempt's worktree is kept, and nothing is committed to its branch, so
// a human can look at exactly what the agent left behind.
func TestFailedVerificationRetainsTheWorktree(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(_ context.Context, req agent.Request) error {
		// Partial work: a file, but not the one verification requires.
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

	call, _ := h.backend.LastCall()
	if _, err := os.Stat(filepath.Join(call.WorkingDir, "half-done.txt")); err != nil {
		t.Errorf("the failed attempt's work was destroyed: %v", err)
	}

	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED", wt.Status)
	}
	if wt.RemovedAt != nil {
		t.Error("a retained worktree must not have a removal time: it still exists")
	}

	retained, err := h.store.ListRetainedWorktrees(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 1 {
		t.Errorf("got %d retained worktrees, want 1 discoverable by an operator", len(retained))
	}

	// Nothing was committed: the branch is still at the base commit.
	branch := "aidev/" + created.Ref
	if strings.Contains(h.branchFiles(branch), "half-done.txt") {
		t.Error("failed work was committed to the branch")
	}

	history := h.eventTypes(created.ID)
	if !contains(history, "task.worktree_retained") || !contains(history, "task.failed") {
		t.Errorf("history = %v, want retention and failure recorded", history)
	}
	if !h.repoIsClean() {
		t.Error("the repository was modified by a failed attempt")
	}
}

func TestAgentFailureIsClassifiedAndRecorded(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Status = task.WorkerFailed
	h.backend.FailureKind = task.FailureAgentError
	h.backend.Stderr = "the model returned an error"

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED", outcome.Task.Status)
	}

	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.FailureKind != task.FailureAgentError {
		t.Errorf("failure kind = %s, want AGENT_ERROR preserved from the backend", attempt.FailureKind)
	}

	// Verification must not have run: there is nothing to verify.
	runs, err := h.store.ListVerificationRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("got %d verification runs, want none after an agent failure", len(runs))
	}
	if contains(h.eventTypes(created.ID), "task.verification_started") {
		t.Error("verification was started despite the agent failing")
	}
}

func TestAgentTimeoutIsRecordedAsTimeout(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.DefaultTaskTimeout = 200 * time.Millisecond })
	h.backend.Delay = time.Hour

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED", outcome.Task.Status)
	}

	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.FailureKind != task.FailureTimeout {
		t.Errorf("failure kind = %s, want TIMEOUT rather than a generic failure", attempt.FailureKind)
	}
}

// Cancelling the caller's context mid-run must still leave a complete record:
// the writes that record a cancellation run on a detached context.
func TestCancellationDuringTheAgentRunIsPersisted(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Delay = time.Hour

	created := h.createTask(nil)

	ctx, cancel := context.WithCancel(h.ctx)
	go func() {
		time.Sleep(400 * time.Millisecond)
		cancel()
	}()

	outcome, err := h.orchestrator.RunTask(ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusCancelled {
		t.Fatalf("status = %s, want CANCELLED", outcome.Task.Status)
	}

	// Re-read from the database: the point is that the record survived.
	reloaded, err := h.store.GetTask(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != task.StatusCancelled {
		t.Errorf("persisted status = %s, want CANCELLED: a cancelled task must not be left RUNNING", reloaded.Status)
	}

	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != task.AttemptCancelled || attempt.FinishedAt == nil {
		t.Errorf("attempt = %+v, want CANCELLED and finished", attempt)
	}

	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeRetained {
		t.Errorf("worktree status = %s, want RETAINED after cancellation", wt.Status)
	}
}

func TestCleanupPolicyNeverKeepsTheWorktree(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.WorktreeCleanup = config.CleanupNever })
	h.backend.Work = doTheWork

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED", outcome.Task.Status)
	}

	call, _ := h.backend.LastCall()
	if _, err := os.Stat(call.WorkingDir); err != nil {
		t.Errorf("the worktree was removed despite WORKTREE_CLEANUP=never: %v", err)
	}

	// The work is still committed, so the branch is usable either way.
	if !strings.Contains(h.branchFiles("aidev/"+created.Ref), "marker.txt") {
		t.Error("the work was not committed under the never policy")
	}
	wt, err := h.store.GetWorktreeByAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Status != task.WorktreeActive {
		t.Errorf("worktree status = %s, want ACTIVE when nothing was removed", wt.Status)
	}
}

// A gated task must not run, and must not leave a worktree or an attempt behind.
func TestApprovalGateBlocksExecution(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) { in.RequiresApproval = true })

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if !errors.Is(err, worker.ErrApprovalRequired) {
		t.Fatalf("RunTask = %v, want ErrApprovalRequired", err)
	}
	if outcome.Task.Status != task.StatusWaitingApproval {
		t.Fatalf("status = %s, want WAITING_APPROVAL", outcome.Task.Status)
	}
	if outcome.Approval == nil || outcome.Approval.Status != task.ApprovalPending {
		t.Errorf("approval = %+v, want a pending request", outcome.Approval)
	}

	if len(h.backend.Calls()) != 0 {
		t.Error("the agent ran despite the approval gate")
	}
	attempts, err := h.store.ListAttempts(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Errorf("got %d attempts, want none: the gate is checked before anything is claimed", len(attempts))
	}
	if !contains(h.eventTypes(created.ID), "task.approval_required") {
		t.Errorf("history = %v, want approval_required", h.eventTypes(created.ID))
	}

	// Approving releases the task, and it then runs normally.
	if _, err := h.orchestrator.Approve(h.ctx, created.Ref, true, "thuyetmt", "looks safe"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	afterApproval, err := h.store.GetTask(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterApproval.Status != task.StatusReady {
		t.Fatalf("status after approval = %s, want READY", afterApproval.Status)
	}

	outcome, err = h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask after approval: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: %s", outcome.Task.Status, outcome.Message)
	}
	if !contains(h.eventTypes(created.ID), "task.approval_granted") {
		t.Error("the approval decision was not recorded")
	}
}

func TestApprovalDeniedFailsTheTask(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(func(in *worker.CreateTaskInput) { in.RequiresApproval = true })

	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); !errors.Is(err, worker.ErrApprovalRequired) {
		t.Fatalf("RunTask = %v, want ErrApprovalRequired", err)
	}
	outcome, err := h.orchestrator.Approve(h.ctx, created.Ref, false, "thuyetmt", "touches production config")
	if err != nil {
		t.Fatalf("Approve(false): %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Errorf("status = %s, want FAILED", outcome.Task.Status)
	}
	if outcome.Approval == nil || outcome.Approval.Status != task.ApprovalDenied {
		t.Errorf("approval = %+v, want DENIED", outcome.Approval)
	}

	// A denied task stays denied rather than quietly becoming runnable.
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); !errors.Is(err, worker.ErrNotRunnable) {
		t.Errorf("RunTask after denial = %v, want ErrNotRunnable", err)
	}
	if !contains(h.eventTypes(created.ID), "task.approval_denied") {
		t.Error("the denial was not recorded")
	}
}

func TestCancelBeforeRunning(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)

	outcome, err := h.orchestrator.Cancel(h.ctx, created.Ref, "no longer needed")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if outcome.Task.Status != task.StatusCancelled {
		t.Errorf("status = %s, want CANCELLED", outcome.Task.Status)
	}
	if !contains(h.eventTypes(created.ID), "task.cancelled") {
		t.Error("the cancellation was not recorded")
	}

	if _, err := h.orchestrator.Cancel(h.ctx, created.Ref, "again"); !errors.Is(err, worker.ErrNotRunnable) {
		t.Errorf("cancelling twice = %v, want ErrNotRunnable", err)
	}
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); !errors.Is(err, worker.ErrNotRunnable) {
		t.Errorf("running a cancelled task = %v, want ErrNotRunnable", err)
	}
}

func TestTerminalTaskCannotBeRerun(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	created := h.createTask(nil)

	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	_, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if !errors.Is(err, worker.ErrNotRunnable) {
		t.Errorf("re-running a succeeded task = %v, want ErrNotRunnable", err)
	}
	if len(h.backend.Calls()) != 1 {
		t.Errorf("the agent ran %d times, want 1", len(h.backend.Calls()))
	}
}

func TestCreateTaskValidatesItsInputs(t *testing.T) {
	h := newHarness(t, nil)

	t.Run("not a repository", func(t *testing.T) {
		_, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
			RepoPath:     t.TempDir(),
			Title:        "x",
			Verification: []task.VerificationStep{{Command: "true"}},
		})
		if err == nil {
			t.Fatal("a non-repository path was accepted")
		}
	})

	t.Run("unknown base ref", func(t *testing.T) {
		_, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
			RepoPath:     h.repoPath,
			Title:        "x",
			BaseRef:      "no-such-branch",
			Verification: []task.VerificationStep{{Command: "true"}},
		})
		if err == nil {
			t.Fatal("an unresolvable base ref was accepted at creation")
		}
	})

	t.Run("no verification", func(t *testing.T) {
		_, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
			RepoPath: h.repoPath,
			Title:    "x",
		})
		if err == nil {
			t.Fatal("a task with no verification command was accepted")
		}
		if !strings.Contains(err.Error(), "verification command is required") {
			t.Errorf("error = %v, want it to explain why verification is required", err)
		}
	})

	// A name the backend does not recognise must be refused before any work
	// starts. Without this check OpenCode would warn, silently run its default
	// agent, and exit 0, leaving a record that claims an agent it never used.
	t.Run("unknown agent is refused", func(t *testing.T) {
		h.backend.KnownAgents = []string{"build", "plan"}
		t.Cleanup(func() { h.backend.KnownAgents = nil })

		_, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
			RepoPath:     h.repoPath,
			Title:        "x",
			Agent:        "reviewer",
			Verification: []task.VerificationStep{{Command: "true"}},
		})
		if err == nil {
			t.Fatal("an agent the backend does not know was accepted")
		}
		if !strings.Contains(err.Error(), "build, plan") {
			t.Errorf("error = %v, want the available agents listed", err)
		}
	})

	// Naming nothing is different from naming something wrong: a blank value
	// means "use the default", which is not a silent substitution.
	t.Run("blank agent uses the configured default", func(t *testing.T) {
		created, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
			RepoPath:     h.repoPath,
			Title:        "blank agent",
			Agent:        "   ",
			Verification: []task.VerificationStep{{Command: "true"}},
		})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if created.Agent != "build" {
			t.Errorf("agent = %q, want the configured default", created.Agent)
		}
	})
}

func TestCreateTaskRegistersTheProjectOnce(t *testing.T) {
	h := newHarness(t, nil)
	first := h.createTask(nil)
	second := h.createTask(nil)

	if first.ProjectID != second.ProjectID {
		t.Errorf("two tasks in the same repository have different projects: %s and %s",
			first.ProjectID, second.ProjectID)
	}
	project, err := h.store.GetProject(h.ctx, first.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if project.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want the repository's actual branch", project.DefaultBranch)
	}
}

func TestRunTaskRejectsUnknownIdentifier(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.orchestrator.RunTask(h.ctx, "TASK-999999"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Each verification step is recorded, including the ones that never ran, so the
// history of a multi-step failure is complete.
func TestMultiStepVerificationRecordsEveryStep(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{
			{Command: "test", Args: []string{"-f", "marker.txt"}},
			{Command: "test", Args: []string{"-f", "never-created.txt"}},
			{Command: "true"},
		}
	})

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED", outcome.Task.Status)
	}

	runs, err := h.store.ListVerificationRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d verification rows, want one per declared step", len(runs))
	}
	want := []task.VerificationStatus{task.VerificationPassed, task.VerificationFailed, task.VerificationSkipped}
	for i, w := range want {
		if runs[i].Status != w {
			t.Errorf("step %d = %s, want %s", i, runs[i].Status, w)
		}
	}
}

// Routing a task to a model is only useful if aidev remembers which model actually
// ran it: the configured default changes over time, and a run recorded as "the
// default" cannot be compared with anything later. The run record therefore holds
// the resolved model and agent, not the task's blanks.
func TestTheModelAndAgentThatRanAreRecorded(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.OpenCodeModel = "cfg/default-model" })

	asked := h.createTask(func(in *worker.CreateTaskInput) {
		in.Model = "opencode/mimo-v2.5-free"
		in.Agent = "build"
	})
	h.backend.Work = doTheWork
	outcome, err := h.orchestrator.RunTask(h.ctx, asked.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	call, ok := h.backend.LastCall()
	if !ok {
		t.Fatal("the backend was never called")
	}
	if call.Model != "opencode/mimo-v2.5-free" {
		t.Errorf("the agent was asked for model %q, want the task's own", call.Model)
	}

	runs, err := h.store.ListWorkerRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d worker runs, want 1", len(runs))
	}
	if runs[0].Model != "opencode/mimo-v2.5-free" {
		t.Errorf("recorded model = %q, want the model that ran", runs[0].Model)
	}
	if runs[0].Agent != "build" {
		t.Errorf("recorded agent = %q, want the agent that ran", runs[0].Agent)
	}
}

// A task that names no model runs on the configured one, and the record says which
// that was — "" would make the history unreadable a month later.
func TestATaskWithNoModelRecordsTheConfiguredOne(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.OpenCodeModel = "cfg/default-model" })

	h.backend.Work = doTheWork
	outcome, err := h.orchestrator.RunTask(h.ctx, h.createTask(nil).Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	call, _ := h.backend.LastCall()
	if call.Model != "cfg/default-model" {
		t.Errorf("the agent was asked for model %q, want the configured default", call.Model)
	}

	runs2, err := h.store.ListWorkerRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs2) != 1 || runs2[0].Model != "cfg/default-model" {
		t.Errorf("recorded model = %+v, want the model that actually ran", runs2)
	}
}
