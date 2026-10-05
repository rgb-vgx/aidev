package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// AbandonBranch is how a retry whose attempt was taken away gives up the
// branch it had just created: the worktree detaches to the same commit, so
// nothing is lost, and the branch is gone.
func TestAbandonBranchDetachesThenDeletes(t *testing.T) {
	ctx := context.Background()
	_, wt := newPublishWorktree(t, "TASK-000020")
	base := strings.TrimSpace(gitIn(t, wt.Path, "rev-parse", "HEAD"))
	if err := wt.ContinueOnNewBranch(ctx, "aidev/TASK-000020-a2"); err != nil {
		t.Fatalf("ContinueOnNewBranch: %v", err)
	}

	if err := wt.AbandonBranch(ctx, "aidev/TASK-000020-a2"); err != nil {
		t.Fatalf("AbandonBranch: %v", err)
	}
	if wt.Branch != "" {
		t.Errorf("Branch = %q, want empty: the worktree is detached now", wt.Branch)
	}
	branches := gitIn(t, wt.Path, "branch", "--list", "--format=%(refname:short)")
	if strings.Contains(branches, "TASK-000020-a2") {
		t.Errorf("branch still exists: %s", branches)
	}
	if head := strings.TrimSpace(gitIn(t, wt.Path, "rev-parse", "HEAD")); head != base {
		t.Errorf("HEAD = %s after detaching, want %s: the commit must stay reachable", head, base)
	}
	// The original branch is untouched, and the worktree is usable.
	if !strings.Contains(branches, "aidev/TASK-000020\n") {
		t.Errorf("the branch the worktree was on before is gone: %s", branches)
	}
	write(t, filepath.Join(wt.Path, "after.txt"), "still writable\n")
	if err := wt.ContinueOnNewBranch(ctx, "aidev/TASK-000020-a3"); err != nil {
		t.Fatalf("ContinueOnNewBranch after an abandon: %v", err)
	}
	// A branch that is not there is not an error: the point is that it is gone.
	if err := wt.AbandonBranch(ctx, "aidev/TASK-000020-a3"); err != nil {
		t.Fatalf("second AbandonBranch: %v", err)
	}
	if err := wt.AbandonBranch(ctx, "aidev/TASK-000020-a3"); err != nil {
		t.Errorf("abandoning an absent branch = %v, want nil", err)
	}
}
