package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CurrentTree records what the working directory holds — untracked files
// join, ignored files stay out, deletions leave — without touching the
// worktree's real index, which a human may still be inspecting after a
// failed attempt.
func TestCurrentTreeReflectsTheWorkingDirectoryNotTheIndex(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "TASK-000001-a1", Branch: "aidev/TASK-000001"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The three cases clean verification exists for: work the agent never
	// added, work git ignores, and a file the agent removed.
	write(t, filepath.Join(wt.Path, "greet.go"), "package main\n\nfunc Greet() string { return \"hi\" }\n")
	write(t, filepath.Join(wt.Path, ".gitignore"), "*.log\n")
	write(t, filepath.Join(wt.Path, "debug.log"), "noisy\n")
	if err := os.Remove(filepath.Join(wt.Path, "main.go")); err != nil {
		t.Fatal(err)
	}

	tree, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatalf("CurrentTree: %v", err)
	}
	if tree == "" {
		t.Fatal("CurrentTree returned no tree id")
	}

	ls, err := exec.Command("git", "-C", wt.Path, "ls-tree", "-r", tree).Output()
	if err != nil {
		t.Fatal(err)
	}
	entries := string(ls)
	for _, want := range []string{"greet.go", ".gitignore"} {
		if !strings.Contains(entries, "\t"+want) {
			t.Errorf("tree does not contain the untracked %s:\n%s", want, entries)
		}
	}
	if strings.Contains(entries, "debug.log") {
		t.Errorf("tree contains the ignored debug.log; a commit would never carry it:\n%s", entries)
	}
	if strings.Contains(entries, "main.go") {
		t.Errorf("tree still contains the deleted main.go:\n%s", entries)
	}

	// The snapshot must be side-effect free: the real index still reports the
	// same untracked file, so an inspection of the worktree afterwards is not
	// confused by verification having staged anything.
	status, err := exec.Command("git", "-C", wt.Path, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(status), "?? greet.go") {
		t.Errorf("worktree status after snapshot = %q, want the untracked file still untracked", status)
	}
}

// A submodule in the snapshot would check out as an empty directory in the
// detached worktree, and the checks would fail against sources that are not
// there — a result nobody could diagnose. CheckGitlinks refuses instead,
// naming the offender. CurrentTree itself must not refuse: the success commit
// is built from it, and in-place verification supports submodules.
func TestCheckGitlinksRefusesASubmoduleSnapshot(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)
	repo := openParent(t, m, f)

	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "task-1", Branch: "aidev/task-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tree, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatalf("CurrentTree refused a tree with a gitlink: %v", err)
	}
	if err := wt.CheckGitlinks(ctx, tree); err == nil {
		t.Fatal("a snapshot containing a gitlink was accepted")
	} else if !strings.Contains(err.Error(), "gitlink") || !strings.Contains(err.Error(), f.path) {
		t.Errorf("error = %v, want it to name the gitlink and its path", err)
	}
}

// HasGitlinks is the creation-time counterpart: it inspects the base commit
// before any worktree exists, so a task that could never verify clean is
// refused up front rather than at verification time.
func TestHasGitlinks(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)

	t.Run("plain repository", func(t *testing.T) {
		repo, err := m.OpenRepository(ctx, newRepo(t))
		if err != nil {
			t.Fatal(err)
		}
		found, path, err := m.HasGitlinks(ctx, repo, "HEAD")
		if err != nil {
			t.Fatalf("HasGitlinks: %v", err)
		}
		if found {
			t.Errorf("HasGitlinks = true at %q on a repository without submodules", path)
		}
	})

	t.Run("repository pinning a submodule", func(t *testing.T) {
		f := newSubmoduleRepo(t)
		repo := openParent(t, m, f)
		found, path, err := m.HasGitlinks(ctx, repo, "HEAD")
		if err != nil {
			t.Fatalf("HasGitlinks: %v", err)
		}
		if !found {
			t.Fatal("HasGitlinks = false on a commit that pins a submodule")
		}
		if path != f.path {
			t.Errorf("gitlink path = %q, want %q", path, f.path)
		}
	})
}

// CommitTree plus CreateDetached is the clean-verification round trip: the
// snapshot becomes a commit nobody's branch points at, and the detached
// checkout holds exactly that content — what a commit would carry, no more
// and no less.
func TestSnapshotCommitAndDetachedCheckoutAgree(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "TASK-000002-a1", Branch: "aidev/TASK-000002"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	write(t, filepath.Join(wt.Path, "marker.txt"), "done\n")

	tree, err := wt.CurrentTree(ctx)
	if err != nil {
		t.Fatalf("CurrentTree: %v", err)
	}
	commit, err := wt.CommitTree(ctx, tree, "aidev verification snapshot")
	if err != nil {
		t.Fatalf("CommitTree: %v", err)
	}

	// No ref may point at the snapshot: an interrupted verification leaves
	// nothing for git's gc to find once the detached worktree is gone.
	head := gitIn(t, wt.Path, "rev-parse", "HEAD")
	if strings.TrimSpace(head) == commit {
		t.Error("commit-tree moved HEAD; the snapshot must be reachable only from the temporary checkout")
	}

	detached, err := m.CreateDetached(ctx, repo, "verify-a1", commit)
	if err != nil {
		t.Fatalf("CreateDetached: %v", err)
	}
	defer func() {
		if err := m.Remove(ctx, detached, true); err != nil {
			t.Errorf("Remove: %v", err)
		}
	}()

	if detached.Branch != "" {
		t.Errorf("Branch = %q, want empty: a detached checkout is not a delivery", detached.Branch)
	}
	content, err := os.ReadFile(filepath.Join(detached.Path, "marker.txt"))
	if err != nil {
		t.Fatalf("the snapshot content is missing from the detached checkout: %v", err)
	}
	if string(content) != "done\n" {
		t.Errorf("marker.txt = %q, want the snapshot's content", content)
	}

	// Anything the checks do in the detached checkout cannot reach back into
	// the worktree the agent's work lives in.
	if err := os.WriteFile(filepath.Join(detached.Path, "marker.txt"), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	back, err := os.ReadFile(filepath.Join(wt.Path, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != "done\n" {
		t.Errorf("the agent's worktree was modified by the detached checkout: %q", back)
	}
}

// CommitTree validates its inputs rather than handing git an empty argument,
// which would fail with an error that says nothing about who called it.
func TestCommitTreeValidatesInput(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "TASK-000003-a1", Branch: "aidev/TASK-000003"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := wt.CommitTree(ctx, "  ", "msg"); err == nil {
		t.Error("an empty tree id was accepted")
	}
	if _, err := wt.CommitTree(ctx, "0123456789012345678901234567890123456789", "  "); err == nil {
		t.Error("an empty message was accepted")
	}
}

// CreateDetached validates the same way Create does: no repository, no
// commit, no silently-relative path.
func TestCreateDetachedValidatesInput(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateDetached(ctx, Repository{}, "name", "abc"); err == nil {
		t.Error("a missing repository was accepted")
	}
	if _, err := m.CreateDetached(ctx, repo, "name", "  "); err == nil {
		t.Error("a missing commit was accepted")
	}
}
