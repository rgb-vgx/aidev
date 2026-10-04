package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// B2: verification_mode decides where the pass runs — in the agent's
// worktree (in_place, the only behaviour before the mode existed) or in a
// detached checkout of the snapshot (clean). The difference is exactly what
// a commit would carry: a file git ignores is present for in_place and
// absent for clean.

// commitIgnoredCheck commits a check that only marker.txt can pass, plus the
// .gitignore entry that keeps marker.txt out of any commit. Together they
// are the false pass clean verification exists to refuse: in_place sees the
// agent's working directory, clean sees the tree a commit would carry.
func commitIgnoredCheck(h *harness) {
	h.t.Helper()
	writeFile(h.t, filepath.Join(h.repoPath, ".gitignore"), "marker.txt\n")
	if err := os.WriteFile(filepath.Join(h.repoPath, "check.sh"), []byte(honestCheck), 0o755); err != nil {
		h.t.Fatal(err)
	}
	gitRun(h.t, h.repoPath, "add", ".gitignore", "check.sh")
	gitRun(h.t, h.repoPath, "commit", "-qm", "check that only an ignored file can pass")
}

// The flagship of B2: the same work, the same check, two modes — one
// succeeds through a file no commit would carry, the other refuses it.
// Without clean mode the first outcome is indistinguishable from honest
// success, which is how a run passes on a runner the agent installed under
// an ignored path.
func TestInPlacePassesOnIgnoredFileThatCleanVerificationRejects(t *testing.T) {
	h := newHarness(t, nil)
	commitIgnoredCheck(h)
	// The agent creates marker.txt — ignored, so it is never in a commit.
	h.backend.Work = doTheWork

	inPlace := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.VerificationMode = "in_place"
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, inPlace.Ref)
	if err != nil {
		t.Fatalf("RunTask in_place: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		if a, aerr := h.store.GetAttempt(h.ctx, outcome.Attempt.ID); aerr == nil {
			t.Fatalf("in_place status = %s, want SUCCEEDED: the checks run where the agent worked (attempt: %s: %s)",
				outcome.Task.Status, a.FailureKind, a.Error)
		}
		t.Fatalf("in_place status = %s, want SUCCEEDED: the checks run where the agent worked", outcome.Task.Status)
	}
	if mode := h.eventPayload(inPlace.ID, "task.verification_started")["mode"]; mode != "in_place" {
		t.Errorf("verification_started mode = %v, want in_place", mode)
	}
	if mode := h.eventPayload(inPlace.ID, "task.created")["verification_mode"]; mode != "in_place" {
		t.Errorf("task.created verification_mode = %v, want in_place", mode)
	}

	clean := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.VerificationMode = "clean"
	})
	outcome, err = h.orchestrator.RunTask(h.ctx, clean.Ref)
	if err != nil {
		t.Fatalf("RunTask clean: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("clean status = %s, want FAILED: the ignored file is not in the snapshot", outcome.Task.Status)
	}
	attempt, err := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.FailureKind != task.FailureVerification {
		t.Errorf("failure kind = %s, want VERIFICATION: a check failed, not the plumbing", attempt.FailureKind)
	}
	if !strings.Contains(attempt.Error, "check.sh") {
		t.Errorf("failure reason %q does not name the failing check", attempt.Error)
	}
	if mode := h.eventPayload(clean.ID, "task.verification_started")["mode"]; mode != "clean" {
		t.Errorf("verification_started mode = %v, want clean", mode)
	}
}

// Setup and verification are one pass: setup first, a single step_index
// sequence, the phase recorded on each row — so a reader of verification_runs
// can tell which half a step belonged to without guessing from the command.
func TestSetupStepsRunBeforeVerificationInOneSequence(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "test", Args: []string{"-f", "prepared.txt"}}}
		in.SetupSteps = []task.VerificationStep{
			{Command: "sh", Args: []string{"-c", "echo ready > prepared.txt"}},
		}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: setup ran before the check that depends on it", outcome.Task.Status)
	}

	runs, err := h.store.ListVerificationRuns(h.ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d verification runs, want setup + one check", len(runs))
	}
	want := []struct {
		phase task.VerificationPhase
		index int
	}{{task.PhaseSetup, 0}, {task.PhaseVerify, 1}}
	for i, w := range want {
		if runs[i].Phase != w.phase {
			t.Errorf("run %d phase = %q, want %q", i, runs[i].Phase, w.phase)
		}
		if runs[i].StepIndex != w.index {
			t.Errorf("run %d step_index = %d, want %d: one sequence across both phases", i, runs[i].StepIndex, w.index)
		}
		if runs[i].Status != task.VerificationPassed {
			t.Errorf("run %d status = %s, want PASSED", i, runs[i].Status)
		}
	}
}

// A failing setup must fail the task rather than let the checks judge an
// unprepared tree — and the recorded rows must still account for every
// declared step, with the verify step skipped, not silently absent.
func TestFailingSetupFailsTheTaskBeforeAnyCheckRuns(t *testing.T) {
	h := newHarness(t, nil)
	commitCheckScript(h)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.SetupSteps = []task.VerificationStep{
			{Command: "sh", Args: []string{"-c", "exit 3"}},
		}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: the checkout was never prepared", outcome.Task.Status)
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
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want the failing setup and the skipped check", len(runs))
	}
	if runs[0].Status != task.VerificationFailed || runs[0].Phase != task.PhaseSetup {
		t.Errorf("run 0 = %s/%s, want FAILED/setup", runs[0].Status, runs[0].Phase)
	}
	if runs[1].Status != task.VerificationSkipped || runs[1].Phase != task.PhaseVerify {
		t.Errorf("run 1 = %s/%s, want SKIPPED/verify", runs[1].Status, runs[1].Phase)
	}
}

// The project row is a template for tasks created after it changes, never a
// leash on tasks that already exist: each task freezes its own mode at
// creation, so the goalposts cannot move under a run in progress, and an
// explicit choice at creation beats the template.
func TestProjectVerificationModeIsFrozenIntoEachTask(t *testing.T) {
	h := newHarness(t, nil)

	// The first task registers the project with the default mode.
	first := h.createTask(nil)
	if first.VerificationMode != task.VerificationInPlace {
		t.Fatalf("first task mode = %q, want the in_place default", first.VerificationMode)
	}

	repo, err := h.git.OpenRepository(h.ctx, h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := h.store.GetProjectByPath(h.ctx, repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.SetProjectVerificationMode(h.ctx, project.ID, task.VerificationClean); err != nil {
		t.Fatalf("SetProjectVerificationMode: %v", err)
	}

	// A task created after the switch inherits it…
	second := h.createTask(nil)
	if second.VerificationMode != task.VerificationClean {
		t.Errorf("inherited mode = %q, want clean from the project template", second.VerificationMode)
	}
	// …the first keeps what it was created with, reloaded from the store so
	// the assertion is about the row, not the value returned earlier.
	reloaded, err := h.store.GetTask(h.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.VerificationMode != task.VerificationInPlace {
		t.Errorf("first task mode after the project switched = %q, want it frozen at in_place", reloaded.VerificationMode)
	}

	// An explicit choice at creation beats the template.
	explicit := h.createTask(func(in *worker.CreateTaskInput) { in.VerificationMode = "in_place" })
	if explicit.VerificationMode != task.VerificationInPlace {
		t.Errorf("explicit mode = %q, want the caller's choice to win over the template", explicit.VerificationMode)
	}

	// Switching back does not rewrite what already exists.
	if _, err := h.store.SetProjectVerificationMode(h.ctx, project.ID, task.VerificationInPlace); err != nil {
		t.Fatal(err)
	}
	reloaded, err = h.store.GetTask(h.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.VerificationMode != task.VerificationClean {
		t.Errorf("second task mode after switching back = %q, want it frozen at clean", reloaded.VerificationMode)
	}

	// The audit log records the mode with the task, so a reviewer of the
	// history can see where the checks were meant to run.
	if mode := h.eventPayload(second.ID, "task.created")["verification_mode"]; mode != "clean" {
		t.Errorf("task.created verification_mode = %v, want clean", mode)
	}
}

// Setup commands get the same independence protection as the checks they
// prepare for, and the refusal numbers steps on the combined sequence — so
// "step 2" in the reason is the same index a reader finds in
// verification_runs, even with setup occupying index 0.
func TestInterceptionCountsSetupAndVerificationAsOneSequence(t *testing.T) {
	h := newHarness(t, nil)
	commitCheckScript(h)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.WorkingDir, "check.sh"), []byte(selfApproval), 0o755)
	}

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.SetupSteps = []task.VerificationStep{{Command: "sh", Args: []string{"-c", "true"}}}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	// The replaced check is the second step of the combined sequence (setup
	// holds index 0), so the refusal must say step 2 — numbering that starts
	// at 1 again for verification alone would point a reviewer at the setup
	// row.
	assertIntercepted(t, h, created, outcome, "step 2 `./check.sh`")
	payload := h.eventPayload(created.ID, "task.verification_intercepted")
	claims, _ := payload["interceptions"].([]any)
	if len(claims) != 1 {
		t.Fatalf("event payload interceptions = %v, want one claim", payload["interceptions"])
	}
	claim, _ := claims[0].(map[string]any)
	if claim["step_index"] != float64(1) {
		t.Errorf("claim step_index = %v, want 1 (the combined sequence's index)", claim["step_index"])
	}
}

// Clean verification cannot honour submodules — the detached checkout would
// hold empty directories — so a task that could never verify clean is
// refused at creation, naming the gitlink, instead of failing mysteriously
// at verification time.
func TestCleanVerificationRefusesRepositoriesWithSubmodules(t *testing.T) {
	h := newHarness(t, nil)

	// A child repository for the harness repo to pin.
	child := t.TempDir()
	gitRun(t, child, "init", "-q", "-b", "main")
	gitRun(t, child, "config", "user.email", "test@example.com")
	gitRun(t, child, "config", "user.name", "Test")
	writeFile(t, filepath.Join(child, "lib.txt"), "pinned\n")
	gitRun(t, child, "add", "-A")
	gitRun(t, child, "commit", "-qm", "init")
	gitRun(t, h.repoPath, "-c", "protocol.file.allow=always",
		"submodule", "add", "--", child, "vendor/child")
	gitRun(t, h.repoPath, "commit", "-qm", "pin the submodule")

	_, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
		RepoPath:         h.repoPath,
		Title:            "A task that could never verify clean",
		Verification:     []task.VerificationStep{{Command: "true"}},
		VerificationMode: "clean",
	})
	if err == nil {
		t.Fatal("clean verification was accepted for a repository pinning a submodule")
	}
	for _, want := range []string{"submodule", "vendor/child"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}

	// in_place has no such limitation: the same repository is accepted, so
	// the refusal is about the mode, not about submodules in general.
	if _, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
		RepoPath:     h.repoPath,
		Title:        "The same repository, verified in place",
		Verification: []task.VerificationStep{{Command: "true"}},
	}); err != nil {
		t.Errorf("in_place creation on a submodule repository: %v", err)
	}
}
