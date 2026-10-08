package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Protected paths guard what a task delivers, and git never delivers a file it
// ignores. TASK-000093 (Axiom-Office, 2026-10-08) was refused for
// tests/lo/__pycache__/ and tests/wps/out/ "changing" under tests/** — both in
// the repository's .gitignore, both written by the test commands the agent ran,
// none of them work the agent did. Its code passed every check by hand.
//
// Ignored files still count where they can change what a check does: a runner
// shadowed by an ignored file is still intercepted (research §7e), and that has
// its own test. Here only the protected-path decision is in question.

// commitIgnoreRules commits a .gitignore the way a Python or tooling-heavy
// repository has one.
func commitIgnoreRules(h *harness) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.repoPath, ".gitignore"), []byte("__pycache__/\nout/\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	gitRun(h.t, h.repoPath, "add", ".gitignore")
	gitRun(h.t, h.repoPath, "commit", "-qm", "ignore build output")
}

func writeUnder(t *testing.T, root string, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIgnoredFilesInAProtectedPathAreNotAViolation(t *testing.T) {
	h := newHarness(t, nil)
	commitCheckScript(h)
	commitIgnoreRules(h)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		// What running the tests leaves behind, then the work itself.
		writeUnder(t, req.WorkingDir, "tests/out/report.json", `{"loaded":true}`)
		writeUnder(t, req.WorkingDir, "tests/__pycache__/test_x.cpython-312.pyc", "bytecode")
		return doTheWork(ctx, req)
	}

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.ProtectedPaths = []string{"tests/*", "tests/**"}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s (%s), want SUCCEEDED: git ignores everything the agent left under tests/",
			outcome.Task.Status, outcome.Message)
	}
	history := h.eventTypes(created.ID)
	if contains(history, "task.verification_intercepted") {
		t.Errorf("ignored files were reported as touching a protected path: %v",
			h.eventPayload(created.ID, "task.verification_intercepted"))
	}
	// The same files are not test edits either: a reviewer told "tests
	// changed: tests/out/" goes looking for an edit that is not there.
	if contains(history, "task.verification_tests_modified") {
		t.Errorf("ignored files were reported as modified tests: %v",
			h.eventPayload(created.ID, "task.verification_tests_modified"))
	}
}

// In place, verification writes into the worktree, and a retry starts from what
// the failed attempt left — including what its checks generated. The second
// attempt must not be refused for the first attempt's test output.
func TestARetryIsNotRefusedForTheIgnoredOutputOfTheChecksBeforeIt(t *testing.T) {
	h := newHarness(t, nil)
	commitIgnoreRules(h)
	check := "#!/bin/sh\nmkdir -p tests/out\necho run >> tests/out/log\ntest -f marker.txt\n"
	if err := os.WriteFile(filepath.Join(h.repoPath, "check.sh"), []byte(check), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, h.repoPath, "add", "check.sh")
	gitRun(t, h.repoPath, "commit", "-qm", "a check that leaves output behind")

	attempts := 0
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		attempts++
		if attempts == 1 {
			return nil // the first attempt does nothing, so its checks fail
		}
		return doTheWork(ctx, req)
	}

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.ProtectedPaths = []string{"tests/**"}
		in.MaxRetries = 1
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s (%s), want SUCCEEDED on the second attempt", outcome.Task.Status, outcome.Message)
	}
	if contains(h.eventTypes(created.ID), "task.verification_intercepted") {
		t.Errorf("the retry was refused for the checks' own output: %v",
			h.eventPayload(created.ID, "task.verification_intercepted"))
	}
}

// The control: a file git does see — untracked and not ignored — in a protected
// path is still refused, exactly as before.
func TestAVisibleNewFileInAProtectedPathIsStillAViolation(t *testing.T) {
	h := newHarness(t, nil)
	commitSelfApprovalScript(h)
	commitIgnoreRules(h)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		writeUnder(t, req.WorkingDir, "tests/out/report.json", "ignored")
		writeUnder(t, req.WorkingDir, "tests/test_extra.py", "def test_nothing(): pass\n")
		return doTheWork(ctx, req)
	}

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.ProtectedPaths = []string{"tests/**"}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	assertIntercepted(t, h, created, outcome, "tests/test_extra.py", "protected")
	if outcome.Attempt != nil {
		attempt, _ := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
		if contains([]string{attempt.Error}, "tests/out") {
			t.Errorf("the refusal also names the ignored tests/out: %s", attempt.Error)
		}
	}
	payload := h.eventPayload(created.ID, "task.verification_intercepted")
	claims, _ := payload["protected_paths"].([]any)
	for _, c := range claims {
		if m, _ := c.(map[string]any); m["path"] == "tests/out/" || m["path"] == "tests/out/report.json" {
			t.Errorf("the refusal claims the ignored %v", m["path"])
		}
	}
}

// The other half of the split: interception still sees ignored files. A
// verification step run from a virtualenv the agent created — .venv is in
// nearly every Python .gitignore — is a judge the agent wrote, ignored or not.
func TestARunnerInAnIgnoredDirectoryIsStillIntercepted(t *testing.T) {
	h := newHarness(t, nil)
	if err := os.WriteFile(filepath.Join(h.repoPath, ".gitignore"), []byte(".venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, h.repoPath, "add", ".gitignore")
	gitRun(t, h.repoPath, "commit", "-qm", "ignore the virtualenv")
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		writeUnder(t, req.WorkingDir, ".venv/bin/check", selfApproval)
		if err := os.Chmod(filepath.Join(req.WorkingDir, ".venv/bin/check"), 0o755); err != nil {
			return err
		}
		return doTheWork(ctx, req)
	}

	created := h.createTask(verifyWith("./.venv/bin/check"))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	assertIntercepted(t, h, created, outcome, ".venv")
}
