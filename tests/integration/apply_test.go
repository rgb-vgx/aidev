package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/task"
)

// `aidev task apply` and `aidev task undo`: the step from a verified result to
// the person's own branch, taken only when they ask. A merge commit makes the
// change one thing to find and revert; undo reverts it with a new commit;
// applying again after an undo re-applies instead of silently doing nothing;
// and whenever the checkout is not in a state to receive it, nothing changes.

// succeededTask runs the default task to SUCCEEDED and returns its reference.
func succeededTask(t *testing.T, h *harness) task.Task {
	t.Helper()
	h.backend.Work = doTheWork
	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v; want SUCCEEDED", outcome.Task.Status, err)
	}
	return created
}

func (h *harness) mainHas(file string) bool {
	_, err := os.Stat(filepath.Join(h.repoPath, file))
	return err == nil
}

func (h *harness) head() string {
	return strings.TrimSpace(gitRun(h.t, h.repoPath, "rev-parse", "HEAD"))
}

func TestApplyAndUndo(t *testing.T) {
	h := newHarness(t, nil)
	created := succeededTask(t, h)

	stdout, stderr, err := h.runCLI(t, "task", "apply", created.Ref)
	if err != nil {
		t.Fatalf("task apply: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "merged into main") {
		t.Errorf("apply output = %q", stdout)
	}
	if !h.mainHas("marker.txt") {
		t.Fatal("main does not have the task's marker.txt after apply")
	}
	if parents := strings.Fields(gitRun(t, h.repoPath, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("HEAD has %d parent(s), want a merge commit with 2", len(parents))
	}
	if got, _ := h.store.GetTask(h.ctx, created.ID); got.Status != task.StatusSucceeded {
		t.Errorf("status after apply = %s, want it left SUCCEEDED", got.Status)
	}
	payload := h.eventPayload(created.ID, "task.applied")
	if payload["into"] != "main" || payload["commit"] != h.head() {
		t.Errorf("task.applied = %v, want into main at HEAD", payload)
	}

	if _, _, err := h.runCLI(t, "task", "apply", created.Ref); err == nil || !strings.Contains(err.Error(), "already applied") {
		t.Errorf("a second apply = %v, want a refusal saying it is already applied", err)
	}

	if _, stderr, err := h.runCLI(t, "task", "undo", created.Ref); err != nil {
		t.Fatalf("task undo: %v\n%s", err, stderr)
	}
	if h.mainHas("marker.txt") {
		t.Error("marker.txt is still on main after undo")
	}
	if _, _, err := h.runCLI(t, "task", "undo", created.Ref); err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Errorf("a second undo = %v, want a refusal saying it is not applied", err)
	}

	// Applying again after an undo must bring the change back; a plain merge
	// of a reverted branch would report success and change nothing.
	stdout, stderr, err = h.runCLI(t, "task", "apply", created.Ref)
	if err != nil {
		t.Fatalf("re-apply: %v\n%s", err, stderr)
	}
	if !h.mainHas("marker.txt") || !strings.Contains(stdout, "re-applied") {
		t.Errorf("re-apply output %q, marker present %v; want the change back", stdout, h.mainHas("marker.txt"))
	}
	if _, stderr, err := h.runCLI(t, "task", "undo", created.Ref); err != nil {
		t.Fatalf("undo of the re-apply: %v\n%s", err, stderr)
	}
	if h.mainHas("marker.txt") {
		t.Error("marker.txt survived undoing the re-apply")
	}
}

func TestApplyLeavesAnUnreadyCheckoutAlone(t *testing.T) {
	t.Run("uncommitted changes", func(t *testing.T) {
		h := newHarness(t, nil)
		created := succeededTask(t, h)
		writeFile(t, filepath.Join(h.repoPath, "main.go"), "package main\n\n// edited by hand\nfunc main() {}\n")
		before := h.head()
		_, _, err := h.runCLI(t, "task", "apply", created.Ref)
		if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
			t.Errorf("apply on a dirty checkout = %v, want a refusal naming the uncommitted changes", err)
		}
		if h.head() != before || h.mainHas("marker.txt") {
			t.Error("the dirty checkout was changed")
		}
	})

	t.Run("a conflict", func(t *testing.T) {
		h := newHarness(t, nil)
		created := succeededTask(t, h)
		writeFile(t, filepath.Join(h.repoPath, "marker.txt"), "written by a person meanwhile\n")
		gitRun(t, h.repoPath, "add", "marker.txt")
		gitRun(t, h.repoPath, "commit", "-qm", "a conflicting marker")
		before := h.head()
		_, _, err := h.runCLI(t, "task", "apply", created.Ref)
		if err == nil || !strings.Contains(err.Error(), "marker.txt") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("apply into a conflicting branch = %v, want the conflict named and nothing changed", err)
		}
		if h.head() != before {
			t.Error("HEAD moved although the merge conflicted")
		}
		if status := strings.TrimSpace(gitRun(t, h.repoPath, "status", "--porcelain")); status != "" {
			t.Errorf("the checkout is left mid-merge: %q", status)
		}
		if _, err := os.Stat(filepath.Join(h.repoPath, ".git", "MERGE_HEAD")); err == nil {
			t.Error("MERGE_HEAD is left behind")
		}
	})

	t.Run("a detached HEAD", func(t *testing.T) {
		h := newHarness(t, nil)
		created := succeededTask(t, h)
		gitRun(t, h.repoPath, "checkout", "-q", "--detach")
		if _, _, err := h.runCLI(t, "task", "apply", created.Ref); err == nil || !strings.Contains(err.Error(), "detached") {
			t.Errorf("apply on a detached HEAD = %v, want a refusal", err)
		}
	})

	t.Run("a task that did not succeed", func(t *testing.T) {
		h := newHarness(t, nil)
		created := h.createTask(nil)
		if _, _, err := h.runCLI(t, "task", "apply", created.Ref); err == nil || !strings.Contains(err.Error(), "SUCCEEDED") {
			t.Errorf("apply of a PENDING task = %v, want a refusal", err)
		}
	})

	t.Run("undo on another branch", func(t *testing.T) {
		h := newHarness(t, nil)
		created := succeededTask(t, h)
		if _, stderr, err := h.runCLI(t, "task", "apply", created.Ref); err != nil {
			t.Fatalf("apply: %v\n%s", err, stderr)
		}
		gitRun(t, h.repoPath, "checkout", "-q", "-b", "elsewhere")
		_, _, err := h.runCLI(t, "task", "undo", created.Ref)
		if err == nil || !strings.Contains(err.Error(), "check out main") {
			t.Errorf("undo on the wrong branch = %v, want it to say which branch to check out", err)
		}
	})
}
