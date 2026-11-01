package integration

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// A task that succeeded on a retry delivered a branch whose history carried
// every earlier attempt's unverified commit under the verified one: merging
// TASK-000172 brought "attempt 1, not verified" into main, and the review
// skill had to tell reviewers to read the whole range. The earlier attempt's
// branch already keeps that commit for anyone who wants it; the delivery is
// the verified tree, so after a retry it is one commit on the base. A first
// attempt's delivery is unchanged: an agent's own commits stay under it
// (agent_tree_test.go).

func TestARetriedSuccessIsOneCommitOnTheBase(t *testing.T) {
	h := newHarness(t, nil)
	var calls atomic.Int32
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if calls.Add(1) == 1 {
			writeFile(t, req.WorkingDir+"/wip.txt", "half done\n")
			return nil // no marker yet: the check fails and the task retries
		}
		return doTheWork(ctx, req)
	}
	created := h.createTask(withRetries(1))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v; want SUCCEEDED on the retry", outcome.Task.Status, err)
	}

	first, second := "aidev/"+created.Ref, "aidev/"+created.Ref+"-a2"
	base := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", "main"))

	if n := strings.TrimSpace(gitRun(t, h.repoPath, "rev-list", "--count", base+".."+second)); n != "1" {
		t.Errorf("%s has %s commits over the base, want 1: the verified delivery alone", second, n)
	}
	if parent := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", second+"^")); parent != base {
		t.Errorf("the delivery's parent is %s, want the base %s", parent, base)
	}
	if files := h.branchFiles(second); !strings.Contains(files, "wip.txt") || !strings.Contains(files, "marker.txt") {
		t.Errorf("%s holds %q, want both attempts' work in the one commit", second, files)
	}
	if msg := gitRun(t, h.repoPath, "log", "-1", "--format=%s", second); strings.Contains(msg, "not verified") {
		t.Errorf("the delivery's message is %q, want the task's own", msg)
	}
	// The earlier attempt's branch still holds what it left.
	if msg := gitRun(t, h.repoPath, "log", "-1", "--format=%s", first); !strings.Contains(msg, "not verified") {
		t.Errorf("%s now says %q, want attempt 1's unverified commit kept", first, msg)
	}
	if head := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", second)); outcome.Worktree == nil || outcome.Worktree.HeadCommit != head {
		t.Errorf("the recorded head commit is not the delivered one (%s): %+v", head, outcome.Worktree)
	}
}

// A retry that ends where the base began delivers nothing new, and its
// branch must not keep pointing at the earlier attempt's unverified commit:
// that would hand the reviewer exactly what this rule keeps out.
func TestARetryThatUndoesItsWorkDeliversTheBase(t *testing.T) {
	h := newHarness(t, nil)
	var calls atomic.Int32
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if calls.Add(1) == 1 {
			writeFile(t, req.WorkingDir+"/wip.txt", "should not be here\n")
			return nil // the check below fails while wip.txt exists
		}
		return os.Remove(req.WorkingDir + "/wip.txt")
	}
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.MaxRetries = 1
		in.Verification = []task.VerificationStep{{Command: "test", Args: []string{"!", "-f", "wip.txt"}}}
	})
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v: %s", outcome.Task.Status, err, outcome.Message)
	}
	second := "aidev/" + created.Ref + "-a2"
	base := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", "main"))
	if head := strings.TrimSpace(gitRun(t, h.repoPath, "rev-parse", second)); head != base {
		msg := gitRun(t, h.repoPath, "log", "-1", "--format=%s", second)
		t.Errorf("%s points at %s (%q), want the base %s: no unverified commit in the delivery", second, head, strings.TrimSpace(msg), base)
	}
}
