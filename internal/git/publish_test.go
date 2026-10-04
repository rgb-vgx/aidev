package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newPublishWorktree is a worktree on its own branch with one change the
// agent made, ready to be snapshotted and published.
func newPublishWorktree(t *testing.T, name string) (*Manager, *Worktree) {
	t.Helper()
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: name, Branch: "aidev/" + name})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return m, wt
}

// The success path of research A6, end to end at the git level: snapshot,
// commit the snapshot onto the commit it was taken on, move the branch by
// compare-and-set, realign the index. What verification writes after the
// snapshot must not be in the commit, and a worktree whose files match the
// commit must read as clean afterwards — otherwise keep-on-dirty removal
// would retain every delivered worktree.
func TestPublishingASnapshotLeavesLaterFilesOut(t *testing.T) {
	ctx := context.Background()
	_, wt := newPublishWorktree(t, "TASK-000010")

	write(t, filepath.Join(wt.Path, "marker.txt"), "done\n")
	head, err := wt.HeadCommit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatalf("CurrentTree: %v", err)
	}

	// What verification might leave behind: an output file nobody ignored.
	write(t, filepath.Join(wt.Path, "cover.out"), "mode: set\n")

	commit, err := wt.CommitTreeOnto(ctx, tree, head, "TASK-000010 deliver")
	if err != nil {
		t.Fatalf("CommitTreeOnto: %v", err)
	}
	if err := wt.UpdateBranch(ctx, wt.Branch, commit, head); err != nil {
		t.Fatalf("UpdateBranch: %v", err)
	}
	if err := wt.SyncIndex(ctx); err != nil {
		t.Fatalf("SyncIndex: %v", err)
	}

	if got := gitIn(t, wt.Path, "rev-parse", "refs/heads/"+wt.Branch); got != commit {
		t.Errorf("branch = %s, want the published commit %s", got, commit)
	}
	if got := gitIn(t, wt.Path, "rev-parse", commit+"^"); got != head {
		t.Errorf("parent = %s, want the commit the snapshot was taken on %s", got, head)
	}
	files := gitIn(t, wt.Path, "ls-tree", "-r", "--name-only", commit)
	if !strings.Contains(files, "marker.txt") {
		t.Errorf("the commit lacks the agent's marker.txt:\n%s", files)
	}
	if strings.Contains(files, "cover.out") {
		t.Errorf("the commit carries cover.out, which appeared after the snapshot:\n%s", files)
	}

	// The index now matches HEAD, so the only thing status reports is the
	// file the commit deliberately left out — no phantom staged deletions.
	status := gitIn(t, wt.Path, "status", "--porcelain")
	if status != "?? cover.out" {
		t.Errorf("status after publishing = %q, want only the untracked cover.out", status)
	}
	// And the file stays on disk: SyncIndex must never touch the working tree.
	if _, err := os.Stat(filepath.Join(wt.Path, "cover.out")); err != nil {
		t.Errorf("SyncIndex removed a file from the working tree: %v", err)
	}
}

// A branch that moved after the snapshot is not overwritten: the
// compare-and-set fails and the branch keeps the commit someone else put
// there. Retrying against whatever the ref holds now would stack the agent's
// tree on history nobody verified.
func TestUpdateBranchRefusesABranchThatMoved(t *testing.T) {
	ctx := context.Background()
	_, wt := newPublishWorktree(t, "TASK-000011")

	base, err := wt.HeadCommit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, "marker.txt"), "done\n")
	tree, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := wt.CommitTreeOnto(ctx, tree, base, "deliver")
	if err != nil {
		t.Fatal(err)
	}

	// Someone else moves the branch first.
	gitIn(t, wt.Path, "commit", "--allow-empty", "-qm", "moved under aidev")
	moved := gitIn(t, wt.Path, "rev-parse", "HEAD")

	err = wt.UpdateBranch(ctx, wt.Branch, commit, base)
	if err == nil {
		t.Fatal("UpdateBranch overwrote a branch that had moved since the snapshot")
	}
	if !strings.Contains(err.Error(), wt.Branch) {
		t.Errorf("error = %v, want it to name the branch", err)
	}
	if got := gitIn(t, wt.Path, "rev-parse", "refs/heads/"+wt.Branch); got != moved {
		t.Errorf("branch = %s after a refused update, want it left at %s", got, moved)
	}
}

// TreeOf plus CurrentTree is how the success path decides there is nothing
// to commit; TreeChanges is what the drift warning lists. Both must agree
// with what git itself says changed.
func TestTreeOfAndTreeChanges(t *testing.T) {
	ctx := context.Background()
	_, wt := newPublishWorktree(t, "TASK-000012")

	headTree, err := wt.TreeOf(ctx, "HEAD")
	if err != nil {
		t.Fatalf("TreeOf: %v", err)
	}
	untouched, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if untouched != headTree {
		t.Errorf("an untouched worktree snapshots to %s, want HEAD's tree %s", untouched, headTree)
	}

	write(t, filepath.Join(wt.Path, "main.go"), "package main\n\n// edited\nfunc main() {}\n")
	write(t, filepath.Join(wt.Path, "new.txt"), "new\n")
	after, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := wt.TreeChanges(ctx, headTree, after)
	if err != nil {
		t.Fatalf("TreeChanges: %v", err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Path] = c.Status
	}
	if got["main.go"] != "M" || got["new.txt"] != "A" || len(got) != 2 {
		t.Errorf("TreeChanges = %v, want main.go M and new.txt A", changes)
	}

	if none, err := wt.TreeChanges(ctx, headTree, headTree); err != nil || len(none) != 0 {
		t.Errorf("TreeChanges of a tree against itself = %v, %v; want nothing", none, err)
	}
}

// The new publishing primitives validate their inputs rather than handing
// git an empty argument, which would fail with an error that says nothing
// about who called it.
func TestPublishingPrimitivesValidateInput(t *testing.T) {
	ctx := context.Background()
	_, wt := newPublishWorktree(t, "TASK-000013")
	const id = "0123456789012345678901234567890123456789"

	if _, err := wt.CommitTreeOnto(ctx, id, "  ", "msg"); err == nil {
		t.Error("CommitTreeOnto accepted an empty parent")
	}
	if err := wt.UpdateBranch(ctx, "  ", id, id); err == nil {
		t.Error("UpdateBranch accepted an empty branch")
	}
	if err := wt.UpdateBranch(ctx, wt.Branch, id, "  "); err == nil {
		t.Error("UpdateBranch accepted an empty expected value; that would be an unconditional overwrite")
	}
	if _, err := wt.TreeOf(ctx, "  "); err == nil {
		t.Error("TreeOf accepted an empty commit")
	}
	if _, err := wt.TreeChanges(ctx, id, "  "); err == nil {
		t.Error("TreeChanges accepted an empty tree id")
	}
}

// An automatic retry continues in the same directory on a new branch: the
// files stay exactly where the failed attempt left them, uncommitted ones
// included, and the old branch is not moved.
func TestContinueOnNewBranchKeepsTheFiles(t *testing.T) {
	ctx := context.Background()
	_, wt := newPublishWorktree(t, "TASK-000014")
	old := wt.Branch
	before := gitIn(t, wt.Path, "rev-parse", "HEAD")
	write(t, filepath.Join(wt.Path, "half-done.txt"), "work in progress\n")

	if err := wt.ContinueOnNewBranch(ctx, "aidev/TASK-000014-a2"); err != nil {
		t.Fatalf("ContinueOnNewBranch: %v", err)
	}
	if wt.Branch != "aidev/TASK-000014-a2" {
		t.Errorf("Branch = %q, want the new branch", wt.Branch)
	}
	if got := gitIn(t, wt.Path, "symbolic-ref", "--short", "HEAD"); got != "aidev/TASK-000014-a2" {
		t.Errorf("HEAD is on %q, want the new branch", got)
	}
	if got := gitIn(t, wt.Path, "rev-parse", "HEAD"); got != before {
		t.Errorf("HEAD moved to %s, want it still at %s", got, before)
	}
	if got := gitIn(t, wt.Path, "rev-parse", "refs/heads/"+old); got != before {
		t.Errorf("the old branch moved to %s", got)
	}
	if status := gitIn(t, wt.Path, "status", "--porcelain"); status != "?? half-done.txt" {
		t.Errorf("status = %q, want the uncommitted file still there and nothing else changed", status)
	}

	if err := wt.ContinueOnNewBranch(ctx, old); err == nil {
		t.Error("ContinueOnNewBranch reused a branch that already exists")
	}
}
