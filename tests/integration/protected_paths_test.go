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

// Research §7b tier 2: the creator of a task can ring-fence paths the agent
// must not change. Touching one refuses verification before any check runs —
// the same refusal path as an intercepted runner, reported through the same
// event — so the decision never depends on the heuristic classifier, which
// only reports.

// commitSelfApprovalScript commits a check that leaves a trace: if it runs at
// all, ran-by-verification appears. The protected-path tests use it to prove
// verification was skipped rather than merely failed.
func commitSelfApprovalScript(h *harness) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.repoPath, "check.sh"), []byte(selfApproval), 0o755); err != nil {
		h.t.Fatal(err)
	}
	gitRun(h.t, h.repoPath, "add", "check.sh")
	gitRun(h.t, h.repoPath, "commit", "-qm", "add check.sh")
}

func TestProtectedPathRefusesVerificationBeforeAnyCheckRuns(t *testing.T) {
	h := newHarness(t, nil)
	commitSelfApprovalScript(h)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		// The work is done and correct; only the ring-fenced path is wrong.
		return os.WriteFile(filepath.Join(req.WorkingDir, ".env"), []byte("SECRET=1\n"), 0o600)
	}

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.ProtectedPaths = []string{".env*"}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	// The shared shape of every refusal: verification failure, the reason
	// naming what was touched, no check executed, worktree kept. The check
	// leaves a trace, so this also proves it never ran.
	assertIntercepted(t, h, created, outcome, ".env", "protected")

	payload := h.eventPayload(created.ID, "task.verification_intercepted")
	claims, _ := payload["protected_paths"].([]any)
	if len(claims) != 1 {
		t.Fatalf("event payload protected_paths = %v, want one claim", payload["protected_paths"])
	}
	claim, _ := claims[0].(map[string]any)
	if claim["pattern"] != ".env*" || claim["path"] != ".env" {
		t.Errorf("claim = %v, want pattern .env* and path .env", claim)
	}
	// Only the reason that fired is reported: a reader must not be told a
	// runner was replaced when none was.
	if _, present := payload["interceptions"]; present {
		t.Errorf("payload also carries interceptions = %v; nothing was intercepted", payload["interceptions"])
	}
}

// The protection must cost an honest attempt nothing: a pattern that matches
// nothing the agent changed does not fire, and the check still decides.
func TestProtectedPathLeftAloneStillSucceeds(t *testing.T) {
	h := newHarness(t, nil)
	commitCheckScript(h)
	h.backend.Work = doTheWork

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.ProtectedPaths = []string{"migrations/*", "docs/**"}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: the agent never went near a protected path", outcome.Task.Status)
	}
	if contains(h.eventTypes(created.ID), "task.verification_intercepted") {
		t.Error("an untouched protected path was reported as a refusal")
	}
}

// An attempt can trip both guards at once — replace the runner and touch a
// ring-fenced path. One event reports both reasons, because a reviewer needs
// the whole picture from the audit log, not the first refusal that happened.
func TestProtectedPathAndInterceptionShareOneEvent(t *testing.T) {
	h := newHarness(t, nil)
	commitSelfApprovalScript(h)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		// Rewrite the judge and step over the ring-fence in one attempt.
		if err := os.WriteFile(filepath.Join(req.WorkingDir, "check.sh"),
			[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.WorkingDir, ".env"), []byte("SECRET=1\n"), 0o600)
	}

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "./check.sh"}}
		in.ProtectedPaths = []string{".env*"}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	assertIntercepted(t, h, created, outcome, "check.sh", ".env")

	payload := h.eventPayload(created.ID, "task.verification_intercepted")
	if _, ok := payload["interceptions"]; !ok {
		t.Errorf("payload = %v, want the interception reported as well", payload)
	}
	claims, _ := payload["protected_paths"].([]any)
	if len(claims) != 1 {
		t.Errorf("payload protected_paths = %v, want the ring-fence reported too", payload["protected_paths"])
	}
}
