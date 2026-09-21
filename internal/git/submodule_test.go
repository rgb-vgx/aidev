package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// submoduleFixture is a parent repository pinning one submodule, plus the
// submodule's own repository. The parent pins the submodule's *first* commit
// while the submodule's branch has moved on, so a test that checked out the
// branch tip instead of the pinned commit would be caught.
type submoduleFixture struct {
	parent string // the parent repository's main working tree
	child  string // the submodule's origin repository
	pinned string // the commit the parent pins
	tip    string // the submodule's branch tip, which is not what is pinned
	path   string // the submodule's path inside the parent, e.g. "vendor/child"
}

func newSubmoduleRepo(t *testing.T) submoduleFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	f := submoduleFixture{
		parent: filepath.Join(root, "parent"),
		child:  filepath.Join(root, "child"),
		path:   "vendor/child",
	}

	if err := os.MkdirAll(f.child, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.child, "init", "-q", "-b", "main")
	write(t, filepath.Join(f.child, "lib.txt"), "pinned\n")
	gitIn(t, f.child, "add", "-A")
	gitIn(t, f.child, "commit", "-qm", "pinned")
	f.pinned = gitIn(t, f.child, "rev-parse", "HEAD")
	write(t, filepath.Join(f.child, "lib.txt"), "moved on\n")
	gitIn(t, f.child, "commit", "-qam", "moved on")
	f.tip = gitIn(t, f.child, "rev-parse", "HEAD")

	if err := os.MkdirAll(f.parent, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.parent, "init", "-q", "-b", "main")
	write(t, filepath.Join(f.parent, "README.md"), "parent\n")
	gitIn(t, f.parent, "add", "-A")
	gitIn(t, f.parent, "commit", "-qm", "init")
	// file:// submodules are refused by default since CVE-2022-39253.
	gitIn(t, f.parent, "-c", "protocol.file.allow=always",
		"submodule", "add", "-q", "--", f.child, f.path)
	gitIn(t, filepath.Join(f.parent, f.path), "checkout", "-q", "--detach", f.pinned)
	gitIn(t, f.parent, "add", f.path)
	gitIn(t, f.parent, "commit", "-qm", "pin the submodule")

	return f
}

func openParent(t *testing.T, m *Manager, f submoduleFixture) Repository {
	t.Helper()
	repo, err := m.OpenRepository(context.Background(), f.parent)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	return repo
}

// With the flag on, the worktree must carry the submodule's sources at exactly
// the commit the parent pins — that is the whole point, since a verification
// command reads those sources.
func TestSubmodulesAreCheckedOutAtThePinnedCommit(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)
	repo := openParent(t, m, f)

	wt, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "task-1", Branch: "aidev/task-1", LoadSubmodules: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(wt.Path, f.path, "lib.txt"))
	if err != nil {
		t.Fatalf("the submodule has no sources in the worktree: %v", err)
	}
	if got := strings.TrimSpace(string(content)); got != "pinned" {
		t.Errorf("submodule content = %q, want the pinned commit's %q", got, "pinned")
	}

	head := gitIn(t, filepath.Join(wt.Path, f.path), "rev-parse", "HEAD")
	if head != f.pinned {
		t.Errorf("submodule HEAD = %s, want the pinned %s (the branch tip is %s)", head, f.pinned, f.tip)
	}
	if len(wt.Submodules) != 1 || wt.Submodules[0] != f.path {
		t.Errorf("Submodules = %v, want [%s]", wt.Submodules, f.path)
	}

	// The gitlink matches what the parent pins, so the worktree is clean and
	// nothing spurious would reach the task's commit.
	status, err := wt.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Clean() {
		t.Errorf("a freshly populated worktree is not clean: %+v", status.Entries)
	}
}

// Removal must leave nothing behind in the submodule's repository. A worktree
// record there would outlive the task, and the next `git worktree list` would
// show a directory that no longer exists.
func TestRemoveTakesBackTheSubmoduleWorktree(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)
	repo := openParent(t, m, f)
	childGitDir := filepath.Join(f.parent, ".git", "modules", f.path)

	wt, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "task-2", Branch: "aidev/task-2", LoadSubmodules: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if listing := gitIn(t, childGitDir, "worktree", "list"); !strings.Contains(listing, wt.Path) {
		t.Fatalf("the submodule repository does not know about the checkout:\n%s", listing)
	}

	// No --force: a clean worktree must be removable, or the guard that keeps a
	// failed attempt's work would be the thing standing in the way.
	if err := m.Remove(ctx, wt, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if listing := gitIn(t, childGitDir, "worktree", "list"); strings.Contains(listing, wt.Path) {
		t.Errorf("the task's worktree is still registered in the submodule repository:\n%s", listing)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("the worktree directory survived removal: %v", err)
	}
}

// Isolation is the promise aidev makes about a worktree, and a submodule is
// part of the worktree. Two tasks against one repository must not share a
// submodule HEAD.
func TestParallelWorktreesDoNotShareASubmoduleHead(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)
	repo := openParent(t, m, f)

	first, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "task-a", Branch: "aidev/task-a", LoadSubmodules: true,
	})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "task-b", Branch: "aidev/task-b", LoadSubmodules: true,
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	gitIn(t, filepath.Join(first.Path, f.path), "checkout", "-q", "--detach", f.tip)

	if head := gitIn(t, filepath.Join(second.Path, f.path), "rev-parse", "HEAD"); head != f.pinned {
		t.Errorf("the second worktree's submodule HEAD moved to %s; it must stay at %s", head, f.pinned)
	}
	if head := gitIn(t, filepath.Join(f.parent, f.path), "rev-parse", "HEAD"); head != f.pinned {
		t.Errorf("the repository's own submodule HEAD moved to %s; aidev must not touch it", head)
	}
	if status := gitIn(t, f.parent, "status", "--porcelain"); status != "" {
		t.Errorf("the repository's main working tree is no longer clean:\n%s", status)
	}
}

// The default must change nothing: neither the checkout nor the number of git
// invocations. A single-repository project pays nothing for a feature it does
// not use.
func TestSubmodulesAreNotLoadedByDefault(t *testing.T) {
	ctx := context.Background()
	f := newSubmoduleRepo(t)

	m := newManager(t)
	recorder := newGitRecorder(t)
	m.Command = recorder.path

	repo := openParent(t, m, f)
	recorder.reset(t)

	wt, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "task-3", Branch: "aidev/task-3",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(wt.Path, f.path))
	if err != nil {
		t.Fatalf("read the submodule directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the submodule directory holds %d entries; with the flag off it must stay empty", len(entries))
	}
	if len(wt.Submodules) != 0 {
		t.Errorf("Submodules = %v, want none", wt.Submodules)
	}

	// Exactly the two invocations Create made before this change: resolve the
	// base commit, then add the worktree.
	want := []string{"rev-parse", "worktree"}
	got := recorder.subcommands(t)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("git invocations = %v, want %v; the flag is off, so nothing extra may run", got, want)
	}
}

// gitRecorder is a git wrapper that logs each invocation, so that "runs no
// extra git command" can be asserted rather than assumed.
type gitRecorder struct {
	path string
	log  string
}

func newGitRecorder(t *testing.T) *gitRecorder {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	r := &gitRecorder{path: filepath.Join(dir, "git-recorder"), log: filepath.Join(dir, "calls.log")}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + r.log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(r.path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *gitRecorder) reset(t *testing.T) {
	t.Helper()
	if err := os.Remove(r.log); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// subcommands returns the first non-flag argument of each recorded invocation.
func (r *gitRecorder) subcommands(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "-") {
				out = append(out, field)
				break
			}
		}
	}
	return out
}

// A submodule whose pinned commit is not in any local repository is reported as
// such, rather than as an unknown revision from a repository the operator would
// have to go and find. aidev does not fetch: worktree creation is local.
func TestMissingSubmoduleRepositoryIsReportedClearly(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)
	repo := openParent(t, m, f)

	// Deinitialise the submodule the way `git submodule deinit` does, leaving
	// the gitlink pinned but no local repository behind it.
	if err := os.RemoveAll(filepath.Join(f.parent, ".git", "modules")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.parent, f.path)); err != nil {
		t.Fatal(err)
	}

	_, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "task-4", Branch: "aidev/task-4", LoadSubmodules: true,
	})
	if err == nil {
		t.Fatal("Create succeeded with no repository for the submodule")
	}
	if !strings.Contains(err.Error(), "submodule") {
		t.Errorf("error = %v, want one that names the submodule as the problem", err)
	}

	// A half-populated worktree is worse than none, so nothing is left behind.
	if _, statErr := os.Stat(filepath.Join(m.WorkspaceRoot, "task-4")); !os.IsNotExist(statErr) {
		t.Errorf("a failed Create left its worktree directory behind: %v", statErr)
	}
}

// Content edited inside a submodule must not break committing the task's own
// work: `git add --all` stages nothing for a gitlink whose commit has not moved,
// so a status that reported it would make `git commit` fail (research §7h).
func TestEditsInsideASubmoduleDoNotBlockTheCommit(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)
	repo := openParent(t, m, f)

	wt, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "task-5", Branch: "aidev/task-5", LoadSubmodules: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	write(t, filepath.Join(wt.Path, f.path, "lib.txt"), "the agent scribbled here\n")
	write(t, filepath.Join(wt.Path, "new.txt"), "the actual work\n")

	commit, err := wt.Commit(ctx, "task work")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if commit == "" {
		t.Fatal("Commit reported nothing to commit, but the worktree has a new file")
	}

	// Read-only means read-only: the submodule scribble is not in the commit.
	files := gitIn(t, wt.Path, "show", "--name-only", "--format=", commit)
	if !strings.Contains(files, "new.txt") {
		t.Errorf("the commit does not contain the task's work:\n%s", files)
	}
	if strings.Contains(files, f.path) {
		t.Errorf("the commit touches the submodule %s:\n%s", f.path, files)
	}
}

// A submodule is not always absorbed into .git/modules. Cloned rather than
// absorbed, it keeps an ordinary .git directory of its own, and ~/work/pingpong
// has both layouts at once (docs/research.md §7h). Guessing either one would
// fail on a real repository.
func TestSubmoduleWithItsOwnGitDirectory(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)

	// De-absorb: move the git directory back inside the submodule, which is
	// what a plain clone leaves behind.
	inTree := filepath.Join(f.parent, f.path)
	absorbed := filepath.Join(f.parent, ".git", "modules", f.path)
	if err := os.Remove(filepath.Join(inTree, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(absorbed, filepath.Join(inTree, ".git")); err != nil {
		t.Fatal(err)
	}
	// core.worktree still points where the absorbed layout put it, which now
	// resolves nowhere; a cloned submodule has no such setting.
	gitIn(t, f.parent, "config", "--file", filepath.Join(inTree, ".git", "config"),
		"--unset", "core.worktree")

	repo := openParent(t, m, f)
	wt, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "task-6", Branch: "aidev/task-6", LoadSubmodules: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if head := gitIn(t, filepath.Join(wt.Path, f.path), "rev-parse", "HEAD"); head != f.pinned {
		t.Errorf("submodule HEAD = %s, want the pinned %s", head, f.pinned)
	}

	if err := m.Remove(ctx, wt, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if listing := gitIn(t, inTree, "worktree", "list"); strings.Contains(listing, wt.Path) {
		t.Errorf("the task's worktree is still registered in the submodule:\n%s", listing)
	}
}
