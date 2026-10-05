package integration

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aidev/internal/git"
)

// The contract of `aidev task apply` and `task undo`: these are the only
// commands that write to the operator's own checkout, so every state they
// cannot handle is refused before anything is changed, and nothing is ever
// reinterpreted. The tests below cover the states a repository reaches on its
// own — a branch that moved, a commit that is gone, two applies at once — on
// top of the ones apply_test.go already holds (dirty checkout, conflict,
// detached HEAD, wrong branch, a task that did not succeed).

// Two applies at the same instant: one merges, the other is told the task is
// already applied. Neither leaves a second merge commit or a second event.
func TestConcurrentAppliesProduceOneMerge(t *testing.T) {
	m := newMCPHarness(t)
	created := succeededTask(t, m.harness)
	before := m.head()

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = m.orchestrator.Apply(m.ctx, created.Ref)
		}(i)
	}
	wg.Wait()

	succeeded, refused := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case strings.Contains(err.Error(), "already applied"):
			refused++
		default:
			t.Errorf("apply failed with %v, want either a merge or an 'already applied' refusal", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Errorf("applies: %d merged, %d refused; want exactly one of each", succeeded, refused)
	}
	if n := countOf(m.eventTypes(created.ID), "task.applied"); n != 1 {
		t.Errorf("%d task.applied events, want exactly 1: %v", n, m.eventTypes(created.ID))
	}
	if !m.mainHas("marker.txt") {
		t.Error("the merge did not land")
	}
	// Exactly one merge, whose first parent is what the branch was: the
	// range also holds the task's own commit, which a merge brings with it.
	if parents := strings.Fields(gitRun(t, m.repoPath, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("HEAD has %d parent(s), want a single merge commit with 2", len(parents))
	}
	if first := strings.TrimSpace(gitRun(t, m.repoPath, "rev-parse", "HEAD^1")); first != before {
		t.Errorf("the merge's first parent is %s, want the branch as it was (%s)", first, before)
	}
	if merges := strings.TrimSpace(gitRun(t, m.repoPath, "rev-list", "--merges", "--count", before+"..HEAD")); merges != "1" {
		t.Errorf("%s merge commits landed for one apply, want 1", merges)
	}
	if status := strings.TrimSpace(gitRun(t, m.repoPath, "status", "--porcelain")); status != "" {
		t.Errorf("the checkout is dirty after the race: %q", status)
	}
}

// The recorded commit is what apply delivers, not the branch name: the branch
// is a pointer a person may have deleted, and the verified commit is the work.
func TestApplyDeliversTheRecordedCommitNotTheBranchName(t *testing.T) {
	h := newHarness(t, nil)
	created := succeededTask(t, h)
	gitRun(t, h.repoPath, "branch", "-D", "aidev/"+created.Ref)

	stdout, stderr, err := h.runCLI(t, "task", "apply", created.Ref)
	if err != nil {
		t.Fatalf("apply after the branch was deleted: %v\n%s", err, stderr)
	}
	if !h.mainHas("marker.txt") || !strings.Contains(stdout, "merged into main") {
		t.Errorf("apply output %q, marker present %v; want the recorded commit delivered", stdout, h.mainHas("marker.txt"))
	}
}

// When the verified commit itself is gone — nothing points at it and git has
// collected it — apply refuses and the checkout is untouched: there is a
// commit to merge or there is nothing.
func TestApplyRefusesWhenTheRecordedCommitIsGone(t *testing.T) {
	h := newHarness(t, nil)
	created := succeededTask(t, h)
	before := h.head()

	gitRun(t, h.repoPath, "branch", "-D", "aidev/"+created.Ref)
	gitRun(t, h.repoPath, "reflog", "expire", "--expire=now", "--all")
	gitRun(t, h.repoPath, "gc", "--prune=now", "--quiet")

	_, _, err := h.runCLI(t, "task", "apply", created.Ref)
	if err == nil {
		t.Fatal("apply succeeded although the verified commit no longer exists")
	}
	if h.head() != before || h.mainHas("marker.txt") {
		t.Errorf("the checkout changed: HEAD %s (was %s)", h.head(), before)
	}
	if status := strings.TrimSpace(gitRun(t, h.repoPath, "status", "--porcelain")); status != "" {
		t.Errorf("the checkout is dirty after the refusal: %q", status)
	}
	if contains(h.eventTypes(created.ID), "task.applied") {
		t.Error("a task.applied event was recorded although nothing was applied")
	}
}

// The checked-out branch moved after the task ran. Merging is still the right
// thing to do — the task's commit does not care — so apply succeeds, and the
// commit that landed in between is still there: the merge brings the two
// together rather than rewriting either.
func TestApplyMergesIntoABranchThatMovedOn(t *testing.T) {
	h := newHarness(t, nil)
	created := succeededTask(t, h)

	writeFile(t, filepath.Join(h.repoPath, "unrelated.txt"), "someone else's work\n")
	gitRun(t, h.repoPath, "add", "unrelated.txt")
	gitRun(t, h.repoPath, "commit", "-qm", "unrelated work meanwhile")
	moved := h.head()

	if _, stderr, err := h.runCLI(t, "task", "apply", created.Ref); err != nil {
		t.Fatalf("apply into a branch that moved: %v\n%s", err, stderr)
	}
	if !h.mainHas("marker.txt") || !h.mainHas("unrelated.txt") {
		t.Error("the merge lost one side: both the task's file and the unrelated commit must survive")
	}
	if msg := gitRun(t, h.repoPath, "log", "-1", "--format=%s", moved); !strings.Contains(msg, "unrelated work") {
		t.Errorf("the commit made meanwhile is gone: %q", msg)
	}
	if parents := strings.Fields(gitRun(t, h.repoPath, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("HEAD has %d parent(s), want the merge to have both sides", len(parents))
	}
}

// Undo after the branch moved so that the revert conflicts: refused, with the
// checkout untouched. Undoing by force would be reinterpreting history.
func TestUndoRefusesWhenTheRevertWouldConflict(t *testing.T) {
	h := newHarness(t, nil)
	created := succeededTask(t, h)
	if _, stderr, err := h.runCLI(t, "task", "apply", created.Ref); err != nil {
		t.Fatalf("apply: %v\n%s", err, stderr)
	}
	// Someone changes the applied file after the merge.
	writeFile(t, filepath.Join(h.repoPath, "marker.txt"), "changed by hand after the merge\n")
	gitRun(t, h.repoPath, "add", "marker.txt")
	gitRun(t, h.repoPath, "commit", "-qm", "hand edit on top of the applied change")
	before := h.head()

	_, _, err := h.runCLI(t, "task", "undo", created.Ref)
	if err == nil {
		t.Fatal("undo succeeded although the change no longer reverts cleanly")
	}
	if !errors.Is(err, git.ErrConflict) {
		t.Errorf("undo error = %v, want a conflict error", err)
	}
	if h.head() != before {
		t.Errorf("HEAD moved to %s although the revert conflicted", h.head())
	}
	if status := strings.TrimSpace(gitRun(t, h.repoPath, "status", "--porcelain")); status != "" {
		t.Errorf("the checkout is left mid-revert: %q", status)
	}
	if contains(h.eventTypes(created.ID), "task.apply_undone") {
		t.Error("a task.apply_undone event was recorded although nothing was reverted")
	}
	if content, err := os.ReadFile(filepath.Join(h.repoPath, "marker.txt")); err != nil || !strings.Contains(string(content), "by hand") {
		t.Errorf("the hand edit was lost: %q, %v", content, err)
	}
}

// Apply and undo report the branch they act on, so a caller can check before
// acting rather than discovering it after.
func TestApplyReportsTheBranchItActsOn(t *testing.T) {
	h := newHarness(t, nil)
	created := succeededTask(t, h)

	res, err := h.orchestrator.Apply(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Into != "main" || res.Branch != "aidev/"+created.Ref || res.Commit == "" {
		t.Errorf("ApplyResult = %+v, want into main, branch aidev/%s and the merge commit", res, created.Ref)
	}
	undo, err := h.orchestrator.Undo(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if undo.Into != res.Into || undo.Commit == "" || undo.Commit == res.Commit {
		t.Errorf("Undo result = %+v, want the same branch and a new commit", undo)
	}
	if !strings.Contains(undo.Message, "reverted") {
		t.Errorf("undo message = %q", undo.Message)
	}
}
