package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates a repository with one commit and returns its path.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")

	write(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	write(t, filepath.Join(dir, "go.mod"), "module probe\n\ngo 1.25\n")
	run("add", "-A")
	run("commit", "-qm", "init")
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newManager returns a Manager whose workspace root is outside the repository,
// which is the only configuration aidev permits.
func newManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestNewManagerRequiresAbsolutePath(t *testing.T) {
	if _, err := NewManager("relative/path"); err == nil {
		t.Error("a relative workspace root was accepted")
	}
}

func TestOpenRepository(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)

	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	// macOS resolves /var through a symlink, so compare resolved paths.
	if want, _ := filepath.EvalSymlinks(repoPath); repo.Path != want {
		t.Errorf("Path = %q, want %q", repo.Path, want)
	}
	if repo.RootCommit == "" {
		t.Error("RootCommit is empty; it identifies the project to OpenCode")
	}

	// A subdirectory resolves to the same top level.
	sub := filepath.Join(repoPath, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fromSub, err := m.OpenRepository(ctx, sub)
	if err != nil {
		t.Fatalf("OpenRepository from a subdirectory: %v", err)
	}
	if fromSub.Path != repo.Path {
		t.Errorf("top level from subdirectory = %q, want %q", fromSub.Path, repo.Path)
	}
}

func TestOpenRepositoryRejectsNonRepositories(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)

	for name, path := range map[string]string{
		"plain directory": t.TempDir(),
		"missing path":    filepath.Join(t.TempDir(), "nope"),
		"empty path":      "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.OpenRepository(ctx, path); !errors.Is(err, ErrNotARepository) {
				t.Errorf("err = %v, want ErrNotARepository", err)
			}
		})
	}
}

func TestResolveCommit(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}

	head, err := m.ResolveCommit(ctx, repo, "")
	if err != nil {
		t.Fatalf("ResolveCommit(HEAD): %v", err)
	}
	if len(head) != 40 {
		t.Errorf("commit = %q, want a full sha", head)
	}

	byBranch, err := m.ResolveCommit(ctx, repo, "main")
	if err != nil {
		t.Fatalf("ResolveCommit(main): %v", err)
	}
	if byBranch != head {
		t.Errorf("main = %s, HEAD = %s; expected the same commit", byBranch, head)
	}

	if _, err := m.ResolveCommit(ctx, repo, "no-such-ref"); !errors.Is(err, ErrUnknownRevision) {
		t.Errorf("err = %v, want ErrUnknownRevision", err)
	}
}

// The central isolation property: the agent's changes land in the worktree and
// the repository's own working tree is untouched.
func TestWorktreeIsolatesChangesFromTheRepository(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)
	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "TASK-000001-a1", Branch: "aidev/TASK-000001"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wt.BaseCommit == "" {
		t.Error("BaseCommit was not recorded")
	}
	if !strings.HasPrefix(wt.Path, m.WorkspaceRoot) {
		t.Errorf("worktree %q is not under the workspace root %q", wt.Path, m.WorkspaceRoot)
	}

	// Simulate an agent creating a file and editing another.
	write(t, filepath.Join(wt.Path, "greet.go"), "package main\n\nfunc Greet() string { return \"hi\" }\n")
	write(t, filepath.Join(wt.Path, "main.go"), "package main\n\nfunc main() { _ = Greet() }\n")

	if _, err := os.Stat(filepath.Join(repoPath, "greet.go")); !os.IsNotExist(err) {
		t.Error("the new file appeared in the repository's working tree; isolation is broken")
	}

	repoStatus, err := exec.Command("git", "-C", repoPath, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(repoStatus)) != "" {
		t.Errorf("the repository is no longer clean:\n%s", repoStatus)
	}

	status, err := wt.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Clean() {
		t.Fatal("the worktree reports clean after being modified")
	}
	var sawUntracked, sawModified bool
	for _, e := range status.Entries {
		switch {
		case e.Code == "??" && e.Path == "greet.go":
			sawUntracked = true
		case strings.Contains(e.Code, "M") && e.Path == "main.go":
			sawModified = true
		}
	}
	if !sawUntracked || !sawModified {
		t.Errorf("status did not report both changes: %+v", status.Entries)
	}
}

// git diff ignores untracked files, so a naive implementation would report no
// changes for exactly the new files an agent was asked to create. The fix must
// also leave the worktree's own index alone, because a failed attempt's worktree
// is kept for a human to inspect.
func TestDiffSeesNewFilesWithoutTouchingTheIndex(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "diff-probe", Branch: "aidev/diff-probe"})
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(wt.Path, "greet.go"), "package main\n\nfunc Greet() string {\n\treturn \"hi\"\n}\n")
	write(t, filepath.Join(wt.Path, "main.go"), "package main\n\nfunc main() { _ = Greet() }\n")

	diff, err := wt.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff.Patch, "greet.go") {
		t.Errorf("the patch does not mention the new file:\n%s", diff.Patch)
	}
	if !strings.Contains(diff.Patch, "func Greet()") {
		t.Errorf("the patch does not contain the new file's content:\n%s", diff.Patch)
	}
	if diff.ChangedFiles != 2 {
		t.Errorf("ChangedFiles = %d, want 2", diff.ChangedFiles)
	}
	if diff.Insertions == 0 {
		t.Errorf("Insertions = 0, want the added lines counted")
	}

	// The real index must be untouched: greet.go should still be untracked.
	status, err := wt.Status(ctx)
	if err != nil {
		t.Fatalf("Status after Diff: %v", err)
	}
	for _, e := range status.Entries {
		if e.Path == "greet.go" && e.Code != "??" {
			t.Errorf("greet.go status is %q after Diff, want %q: collecting a diff must not stage anything",
				e.Code, "??")
		}
	}
}

func TestDiffOnACleanWorktreeIsEmpty(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "clean", Branch: "aidev/clean"})
	if err != nil {
		t.Fatal(err)
	}

	diff, err := wt.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff.ChangedFiles != 0 || strings.TrimSpace(diff.Patch) != "" {
		t.Errorf("a fresh worktree reported changes: %+v", diff)
	}
}

// On success the agent's work is uncommitted, so removing the worktree would
// destroy the deliverable. Committing to the task's own branch makes it durable
// without merging anything.
func TestCommitPreservesWorkOnTheBranchAndAllowsRemoval(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)
	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "commit-probe", Branch: "aidev/commit-probe"})
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(wt.Path, "greet.go"), "package main\n\nfunc Greet() string { return \"hi\" }\n")

	before, err := wt.HeadCommit(ctx)
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}

	commit, err := wt.Commit(ctx, "aidev: TASK-000001 Add Greet")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if commit == "" {
		t.Fatal("Commit returned no sha despite pending changes")
	}
	if commit == before {
		t.Error("HEAD did not move")
	}

	status, err := wt.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Clean() {
		t.Errorf("the worktree is not clean after committing: %+v", status.Entries)
	}

	// Removal must now succeed without --force.
	if err := m.Remove(ctx, wt, false); err != nil {
		t.Fatalf("Remove after commit: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Error("the worktree directory still exists after Remove")
	}

	// The work survives on the branch, which is the point of committing.
	out, err := exec.Command("git", "-C", repoPath, "ls-tree", "--name-only", "aidev/commit-probe").Output()
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	if !strings.Contains(string(out), "greet.go") {
		t.Errorf("the branch does not contain the work after cleanup:\n%s", out)
	}
}

func TestCommitOnACleanWorktreeIsANoOp(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "noop", Branch: "aidev/noop"})
	if err != nil {
		t.Fatal(err)
	}

	commit, err := wt.Commit(ctx, "nothing to do")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if commit != "" {
		t.Errorf("Commit = %q, want empty when there is nothing to commit", commit)
	}

	if _, err := wt.Commit(ctx, "  "); err == nil {
		t.Error("an empty commit message was accepted")
	}
}

// Refusing to remove a dirty worktree is the cleanup policy's backstop: git
// itself prevents a failed attempt's work from being discarded.
func TestRemoveRefusesToDiscardUncommittedWork(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "dirty", Branch: "aidev/dirty"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, "work-in-progress.go"), "package main\n")

	err = m.Remove(ctx, wt, false)
	if !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("Remove = %v, want ErrWorktreeDirty", err)
	}
	if _, statErr := os.Stat(filepath.Join(wt.Path, "work-in-progress.go")); statErr != nil {
		t.Error("the refused removal still destroyed the work")
	}

	// force is the explicit, human-requested escape hatch.
	if err := m.Remove(ctx, wt, true); err != nil {
		t.Fatalf("Remove with force: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Error("force removal left the directory behind")
	}
}

func TestCreateRejectsDuplicateBranchAndPath(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "first", Branch: "aidev/dup"}); err != nil {
		t.Fatal(err)
	}

	// Same branch, different directory.
	_, err = m.Create(ctx, CreateRequest{Repository: repo, Name: "second", Branch: "aidev/dup"})
	if !errors.Is(err, ErrBranchExists) {
		t.Errorf("duplicate branch = %v, want ErrBranchExists", err)
	}

	// Same directory, different branch.
	_, err = m.Create(ctx, CreateRequest{Repository: repo, Name: "first", Branch: "aidev/other"})
	if !errors.Is(err, ErrWorktreeExists) {
		t.Errorf("existing path = %v, want ErrWorktreeExists", err)
	}
}

func TestCreateValidatesInput(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		req     CreateRequest
		wantErr error
	}{
		{"path traversal in name", CreateRequest{Repository: repo, Name: "../escape", Branch: "aidev/x"}, ErrInvalidName},
		{"slash in name", CreateRequest{Repository: repo, Name: "a/b", Branch: "aidev/x"}, ErrInvalidName},
		{"absolute name", CreateRequest{Repository: repo, Name: "/etc", Branch: "aidev/x"}, ErrInvalidName},
		{"dotdot name", CreateRequest{Repository: repo, Name: "..", Branch: "aidev/x"}, ErrInvalidName},
		{"leading dash", CreateRequest{Repository: repo, Name: "-rf", Branch: "aidev/x"}, ErrInvalidName},
		{"hidden name", CreateRequest{Repository: repo, Name: ".git", Branch: "aidev/x"}, ErrInvalidName},
		{"empty name", CreateRequest{Repository: repo, Name: "", Branch: "aidev/x"}, ErrInvalidName},
		{"null byte", CreateRequest{Repository: repo, Name: "ok\x00bad", Branch: "aidev/x"}, ErrInvalidName},
		{"unknown base ref", CreateRequest{Repository: repo, Name: "ok", Branch: "aidev/x", BaseRef: "nope"}, ErrUnknownRevision},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.Create(ctx, tc.req); !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	for _, branch := range []string{"", "  ", "-bad", "/bad", "bad/", "a..b", "a//b", "has space", "tip.lock", "a~b", "a^b", "a:b"} {
		t.Run("branch "+branch, func(t *testing.T) {
			if _, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "okname", Branch: branch}); err == nil {
				t.Errorf("branch %q was accepted", branch)
			}
		})
	}
}

// A workspace root inside the repository would put the agent's checkout in the
// main working tree, which containment in WORKSPACE_ROOT alone would not catch.
func TestWorkspaceRootInsideRepositoryIsRefused(t *testing.T) {
	ctx := context.Background()
	repoPath := newRepo(t)

	m, err := NewManager(filepath.Join(repoPath, ".aidev", "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Create(ctx, CreateRequest{Repository: repo, Name: "inside", Branch: "aidev/inside"})
	if !errors.Is(err, ErrInsideRepository) {
		t.Fatalf("err = %v, want ErrInsideRepository", err)
	}
}

// A symlink inside the workspace root must not become an escape hatch: a textual
// prefix check would be satisfied by a path that physically is elsewhere.
func TestSymlinkCannotEscapeTheWorkspace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	outside := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.withinWorkspace(filepath.Join(root, "escape", "target")); !errors.Is(err, ErrOutsideWorkspace) {
		t.Errorf("err = %v, want ErrOutsideWorkspace", err)
	}
}

func TestWithinWorkspaceRejectsTraversalAndRootItself(t *testing.T) {
	m := newManager(t)
	if err := m.withinWorkspace(m.WorkspaceRoot); !errors.Is(err, ErrOutsideWorkspace) {
		t.Errorf("the workspace root itself = %v, want ErrOutsideWorkspace", err)
	}
	if err := m.withinWorkspace(filepath.Join(m.WorkspaceRoot, "..", "elsewhere")); !errors.Is(err, ErrOutsideWorkspace) {
		t.Errorf("traversal = %v, want ErrOutsideWorkspace", err)
	}
	if err := m.withinWorkspace(filepath.Join(m.WorkspaceRoot, "ok")); err != nil {
		t.Errorf("a legitimate path was rejected: %v", err)
	}
}

func TestAttachRevalidatesAndPrune(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repo, err := m.OpenRepository(ctx, newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Create(ctx, CreateRequest{Repository: repo, Name: "attach", Branch: "aidev/attach"})
	if err != nil {
		t.Fatal(err)
	}

	again, err := m.Attach(ctx, repo, wt.Path, wt.Branch)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if again.Path != wt.Path || again.Branch != wt.Branch {
		t.Errorf("Attach returned %+v, want the same path and branch", again)
	}

	// A stored path is not automatically trustworthy.
	if _, err := m.Attach(ctx, repo, "/etc", "aidev/x"); !errors.Is(err, ErrOutsideWorkspace) {
		t.Errorf("Attach to /etc = %v, want ErrOutsideWorkspace", err)
	}
	if _, err := m.Attach(ctx, repo, filepath.Join(m.WorkspaceRoot, "gone"), "aidev/x"); err == nil {
		t.Error("Attach to a missing directory was accepted")
	}

	// Deleting the directory outside aidev leaves a stale record that prune clears.
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	if err := m.Prune(ctx, repo); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	out, err := exec.Command("git", "-C", repo.Path, "worktree", "list").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), wt.Path) {
		t.Errorf("prune left a stale worktree record:\n%s", out)
	}
}

func TestBaseRefStartsTheBranchElsewhere(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)
	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}

	first, err := m.ResolveCommit(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// Add a second commit so HEAD and the first commit differ.
	write(t, filepath.Join(repoPath, "second.go"), "package main\n")
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "second"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	wt, err := m.Create(ctx, CreateRequest{
		Repository: repo, Name: "based", Branch: "aidev/based", BaseRef: first,
	})
	if err != nil {
		t.Fatalf("Create with BaseRef: %v", err)
	}
	if wt.BaseCommit != first {
		t.Errorf("BaseCommit = %s, want %s", wt.BaseCommit, first)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "second.go")); !os.IsNotExist(err) {
		t.Error("the worktree contains a file from after its base commit")
	}
}
