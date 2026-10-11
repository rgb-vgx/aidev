package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
)

// The shared config is a command-execution channel (core.hooksPath,
// core.fsmonitor, filter and diff drivers, aliases, includes), so an agent
// writing it fails containment. But the same file is written by other parties
// while a task runs: VS Code adds branch.<name>.vscode-merge-base when a branch
// is created, `git push -u` adds branch.<name>.remote and .merge. TASK-000094
// (Axiom-Office, 2026-10-08) failed as CONTAINMENT because the planner created
// its next spec branch in the main repository while VS Code had it open; the
// agent never touched the config.
//
// A change confined to such per-branch tracking and editor bookkeeping is now a
// warning in the record, not a breach. Every other key still fails the attempt.

// gitInRepo writes the main repository's config the way an editor or a person
// in another terminal does, while the agent is running.
func gitInRepo(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestBranchBookkeepingInTheSharedConfigIsNotABreach(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		// Meanwhile, outside the agent: a branch is created and an editor
		// and a push record their bookkeeping.
		gitInRepo(t, h.repoPath, "config", "branch.spec/next-task.vscode-merge-base", "origin/main")
		gitInRepo(t, h.repoPath, "config", "branch.spec/next-task.remote", "origin")
		gitInRepo(t, h.repoPath, "config", "branch.spec/next-task.merge", "refs/heads/spec/next-task")
		return doTheWork(ctx, req)
	}

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s (%s), want SUCCEEDED: only branch bookkeeping changed", outcome.Task.Status, outcome.Message)
	}
	history := h.eventTypes(created.ID)
	if contains(history, "task.containment_breach") {
		t.Errorf("branch bookkeeping was reported as a breach: %v", h.eventPayload(created.ID, "task.containment_breach"))
	}
	// Still on the record, for a reviewer.
	if !contains(history, "task.shared_refs_changed") {
		t.Fatalf("history = %v, want the config change warned about", history)
	}
	payload := h.eventPayload(created.ID, "task.shared_refs_changed")
	keys, _ := payload["config_keys"].([]any)
	if len(keys) != 3 {
		t.Errorf("warning config_keys = %v, want the three keys that changed", payload["config_keys"])
	}
}

// The other side: a key that can run a command, written next to harmless ones,
// still fails the attempt, and the breach names it.
func TestADangerousKeyBesideBookkeepingIsStillABreach(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = workAndTamper(t, func(req agent.Request) error {
		gitInRepo(t, h.repoPath, "config", "branch.main.vscode-merge-base", "origin/main")
		return gitInWorktree(req, "config", "core.hooksPath", "/tmp/evil-hooks")
	})

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED", outcome.Task.Status)
	}
	attempt, _ := h.store.GetAttempt(h.ctx, outcome.Attempt.ID)
	if attempt.FailureKind != task.FailureContainment {
		t.Errorf("failure kind = %s, want CONTAINMENT", attempt.FailureKind)
	}
	if !strings.Contains(attempt.Error, "core.hookspath") {
		t.Errorf("error = %q, want it to name core.hookspath", attempt.Error)
	}
	if strings.Contains(attempt.Error, "vscode-merge-base") {
		t.Errorf("error = %q names the harmless key too", attempt.Error)
	}
}

// A branch's merge options can choose a merge strategy, and a strategy is an
// executable found on PATH: not bookkeeping.
func TestBranchMergeOptionsAreNotBookkeeping(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = workAndTamper(t, func(req agent.Request) error {
		return gitInWorktree(req, "config", "branch.main.mergeoptions", "--strategy=evil")
	})
	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusFailed {
		t.Fatalf("status = %s, want FAILED: mergeoptions is not inert", outcome.Task.Status)
	}
}
