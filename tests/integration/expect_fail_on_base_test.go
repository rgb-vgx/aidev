package integration

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/task"
	"aidev/internal/worker"
)

// Research B3, the red-before-green gate: a task whose point is to fix a bug
// must not be able to "succeed" by doing nothing. With expect_fail_on_base
// the verification commands run on the base commit before the agent is
// called; if they already pass there, they cannot tell the before state from
// the after state, and the attempt fails without ever calling the agent.

// A base that is already green fails the attempt immediately: verification
// failure, the reason naming the problem, the agent never called, and the
// record showing the gate ran — not only that it fired.
func TestExpectFailOnBaseFailsWhenBaseIsAlreadyGreen(t *testing.T) {
	h := newHarness(t, nil)
	// main.go ships with the harness repository, so this check passes on the
	// base commit without the agent doing anything.
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "test", Args: []string{"-f", "main.go"}}}
		in.ExpectFailOnBase = true
	})

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED", outcome.Task.Status)
	}

	attempt, err := h.store.LatestAttempt(h.ctx, created.ID)
	if err != nil {
		t.Fatalf("LatestAttempt: %v", err)
	}
	if attempt.FailureKind != task.FailureVerification {
		t.Errorf("failure kind = %s, want VERIFICATION", attempt.FailureKind)
	}
	if !strings.Contains(attempt.Error, "do not distinguish before from after") {
		t.Errorf("attempt error = %q, want it to say the commands cannot tell before from after", attempt.Error)
	}

	history := h.eventTypes(created.ID)
	// The agent was never called, and the post-agent verdict never ran: the
	// gate fired before either.
	if contains(history, "task.worker_started") {
		t.Errorf("history = %v, want no worker_started: the agent must not be called on a green base", history)
	}
	if contains(history, "task.verification_completed") {
		t.Errorf("history = %v, want no verification_completed: the gate fires before the post-agent verdict", history)
	}
	// The gate's own record: a reviewer must see that it ran and what it
	// found, on both paths — and before the failure that it caused.
	baseAt := indexOf(history, "task.base_check_completed")
	if baseAt < 0 {
		t.Fatalf("history = %v, want task.base_check_completed", history)
	}
	failedAt := indexOf(history, "task.failed")
	if failedAt < 0 {
		t.Fatalf("history = %v, want task.failed", history)
	}
	if baseAt > failedAt {
		t.Errorf("base_check_completed at %d comes after task.failed at %d", baseAt, failedAt)
	}
	payload := h.eventPayload(created.ID, "task.base_check_completed")
	if payload["passed"] != true {
		t.Errorf("base check passed = %v, want true", payload["passed"])
	}
	summary, _ := payload["summary"].(string)
	if summary == "" {
		t.Errorf("base check payload = %v, want a summary of what passed", payload)
	}
	// The report is carried into the failure so the reader sees what "already
	// passes on the base" meant without re-running anything.
	if got, _ := h.eventPayload(created.ID, "task.failed")["verification"].(string); got == "" {
		t.Errorf("task.failed payload = %v, want the verification summary", h.eventPayload(created.ID, "task.failed"))
	}

	// The detached checkout the check ran in is scaffolding: it must be gone
	// from the repository's worktree registry even though the attempt failed.
	if listed := gitRun(t, h.repoPath, "worktree", "list"); strings.Contains(listed, "basecheck-") {
		t.Errorf("the base-check worktree was not removed:\n%s", listed)
	}
}

// Setup steps belong to the base check too: a check that only passes once
// setup has prepared the tree must not be mistaken for an already-green base.
// Here the verification can only pass if setup ran during the base check, so
// the gate firing is the proof that it did.
func TestExpectFailOnBaseRunsSetupSteps(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.SetupSteps = []task.VerificationStep{
			{Command: "sh", Args: []string{"-c", "echo ready > prepared.txt"}},
		}
		in.Verification = []task.VerificationStep{{Command: "test", Args: []string{"-f", "prepared.txt"}}}
		in.ExpectFailOnBase = true
	})

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: the check passed, so setup must have run in the base check", outcome.Task.Status)
	}
	if payload := h.eventPayload(created.ID, "task.base_check_completed"); payload["passed"] != true {
		t.Errorf("base check passed = %v, want true", payload["passed"])
	}
	if contains(h.eventTypes(created.ID), "task.worker_started") {
		t.Error("the agent was called although the check passed on the base")
	}
}

// A red base is what the gate exists to allow: the run proceeds, the agent
// works, and the task succeeds — with the gate's record still in the history,
// because a reviewer needs to see that the check happened at all.
func TestExpectFailOnBaseProceedsWhenBaseIsRed(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.ExpectFailOnBase = true
	})

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: the base was red and the work was done", outcome.Task.Status)
	}

	history := h.eventTypes(created.ID)
	if !contains(history, "task.worker_started") {
		t.Error("the agent was not called although the base was red")
	}
	if !contains(history, "task.verification_completed") {
		t.Error("the post-agent verdict did not run")
	}
	if !contains(history, "task.base_check_completed") {
		t.Errorf("history = %v, want the gate's record even when it does not fire", history)
	}
	payload := h.eventPayload(created.ID, "task.base_check_completed")
	if payload["passed"] != false {
		t.Errorf("base check passed = %v, want false", payload["passed"])
	}

	// Same scaffolding promise as on the firing path: the detached checkout
	// is removed whether the gate fired or let the run through.
	if listed := gitRun(t, h.repoPath, "worktree", "list"); strings.Contains(listed, "basecheck-") {
		t.Errorf("the base-check worktree was not removed:\n%s", listed)
	}
	if !h.repoIsClean() {
		t.Error("the repository's own working tree was touched")
	}
}

// B3 on the command line: the flag sets the field, `task get` shows what a
// run of this task will mean, and the help names the flag — a gate nobody can
// discover is a gate nobody will use.
func TestTaskCreateTakesExpectFailOnBase(t *testing.T) {
	h := newHarness(t, nil)

	stdout, stderr, err := h.runCLI(t, "task", "create",
		"--repo", h.repoPath,
		"--title", "Fix the bug the test reports",
		"--verify", "test -f marker.txt",
		"--expect-fail-on-base",
		"--json")
	if err != nil {
		t.Fatalf("task create --expect-fail-on-base: %v\nstderr: %s", err, stderr)
	}

	var created struct {
		Ref              string `json:"ref"`
		ExpectFailOnBase bool   `json:"expect_fail_on_base"`
	}
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("the created task is not JSON: %v\n%s", err, stdout)
	}
	if !created.ExpectFailOnBase {
		t.Errorf("expect_fail_on_base = false, want true from the flag")
	}

	shown, _, err := h.runCLI(t, "task", "get", created.Ref)
	if err != nil {
		t.Fatalf("task get: %v", err)
	}
	if !strings.Contains(shown, "base check") {
		t.Errorf("task get does not show the base check:\n%s", shown)
	}

	_, helpErr, _ := h.runCLI(t, "task", "create", "-h")
	// Go's flag package prints a single dash; accept either spelling so the
	// assertion is about the flag existing, not about its typography.
	if !strings.Contains(helpErr, "-expect-fail-on-base") {
		t.Errorf("task create help does not mention -expect-fail-on-base:\n%s", helpErr)
	}
}

// The base check runs in a detached checkout, which — like clean verification
// — cannot hold submodule content: the gitlink would read as an empty
// directory and the checks would go red for the wrong reason, silently
// disabling the gate. Refused at creation, naming the gitlink, instead.
func TestExpectFailOnBaseRefusesRepositoriesWithSubmodules(t *testing.T) {
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
		Title:            "A task whose base check could never be trusted",
		Verification:     []task.VerificationStep{{Command: "true"}},
		ExpectFailOnBase: true,
	})
	if err == nil {
		t.Fatal("expect_fail_on_base was accepted for a repository pinning a submodule")
	}
	for _, want := range []string{"expect_fail_on_base", "submodule", "vendor/child"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}

	// Without the flag the same repository is accepted: the refusal is about
	// the gate, not about submodules in general.
	if _, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
		RepoPath:     h.repoPath,
		Title:        "The same repository, without a base check",
		Verification: []task.VerificationStep{{Command: "true"}},
	}); err != nil {
		t.Errorf("creation without expect_fail_on_base on a submodule repository: %v", err)
	}
}
