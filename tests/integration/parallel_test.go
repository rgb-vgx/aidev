package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Tasks of one repository running side by side (docs/research.md §7k): each
// run creates its own branch and worktree in the shared repository while the
// others are mid-run, and none of that may read as one attempt tampering with
// shared state — a sibling's branch is another task's work, not a breach.
func TestTasksOfOneRepositoryRunSideBySide(t *testing.T) {
	h := newHarness(t, nil)
	const n = 3
	var started sync.WaitGroup
	started.Add(n)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		// Hold every agent until all are running, so the runs overlap for
		// sure: each one's worktree and branch appear while the others'
		// agents are working.
		started.Done()
		started.Wait()
		time.Sleep(100 * time.Millisecond)
		return os.WriteFile(filepath.Join(req.WorkingDir, "marker.txt"), []byte(req.TaskRef+"\n"), 0o600)
	}

	tasks := make([]task.Task, n)
	for i := range tasks {
		tasks[i] = h.createTask(func(in *worker.CreateTaskInput) { in.Title = fmt.Sprintf("parallel %d", i) })
	}

	var wg sync.WaitGroup
	outcomes := make([]worker.Outcome, n)
	errs := make([]error, n)
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = h.orchestrator.RunTask(h.ctx, tasks[i].Ref)
		}(i)
	}
	wg.Wait()

	for i, created := range tasks {
		if errs[i] != nil {
			t.Errorf("%s: RunTask: %v", created.Ref, errs[i])
			continue
		}
		if outcomes[i].Task.Status != task.StatusSucceeded {
			attempt, _ := h.store.LatestAttempt(h.ctx, created.ID)
			t.Errorf("%s: status %s (%s: %s), want SUCCEEDED", created.Ref, outcomes[i].Task.Status, attempt.FailureKind, attempt.Error)
			continue
		}
		// Each delivery holds its own work only.
		content := gitRun(t, h.repoPath, "show", "aidev/"+created.Ref+":marker.txt")
		if strings.TrimSpace(content) != created.Ref {
			t.Errorf("%s: marker.txt on its branch = %q, want its own reference", created.Ref, content)
		}
		history := h.eventTypes(created.ID)
		if contains(history, "task.containment_breach") {
			t.Errorf("%s: a sibling task's run was taken for a containment breach: %v", created.Ref, history)
		}
		if contains(history, "task.shared_refs_changed") {
			payload := h.eventPayload(created.ID, "task.shared_refs_changed")
			t.Errorf("%s: sibling tasks' branches were reported as shared refs changing: %v", created.Ref, payload)
		}
	}
}

// Only overlapping runs are excused. A branch of a task that finished before
// this attempt started is nobody's work in progress: an agent moving it is
// still reported.
func TestMovingAFinishedTasksBranchIsStillReported(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	earlier := h.createTask(nil)
	if out, err := h.orchestrator.RunTask(h.ctx, earlier.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("earlier task = %v, %v", out.Task.Status, err)
	}
	earlierBranch := "aidev/" + earlier.Ref

	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		gitRun(t, req.WorkingDir, "branch", "-f", earlierBranch, "HEAD~0")
		return doTheWork(ctx, req)
	}
	later := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, later.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	payload := h.eventPayload(later.ID, "task.shared_refs_changed")
	refs, _ := payload["refs"].(map[string]any)
	if _, ok := refs["refs/heads/"+earlierBranch]; !ok {
		t.Errorf("shared_refs_changed = %v, want it to name the finished task's branch the agent moved", payload)
	}
}
