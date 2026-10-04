package git

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// CurrentTree records the worktree's current content as a tree object and
// returns its id: the tree a commit made right now would carry, not the
// working directory as it happens to look.
//
// The snapshot runs on a fresh temporary index — never the real one, and
// never the intent-to-add copy Diff() uses. Intent-to-add entries have no
// content, so a tree built from them would record a file the worktree does
// not actually contain; and touching the real index would race anyone else
// reading it. read-tree HEAD seeds the index with what is checked out, then
// add -A brings it in line with the working directory: untracked files
// (except those git ignores) join, deletions leave, modifications land.
//
// It makes no claim about what the tree contains: a submodule entry is fine
// here, because the success commit is built from this tree on the task's own
// branch. Clean verification, which checks the tree out fresh, adds
// CheckGitlinks on top.
func (w *Worktree) CurrentTree(ctx context.Context) (string, error) {
	temp, err := os.CreateTemp("", "aidev-snapshot-index-*")
	if err != nil {
		return "", fmt.Errorf("create snapshot index: %w", err)
	}
	path := temp.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := temp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("create snapshot index: %w", err)
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + path}
	if err := w.snapshotStep(ctx, env, "read-tree HEAD", "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if err := w.snapshotStep(ctx, env, "add --all", "add", "--all", "--", "."); err != nil {
		return "", err
	}
	write, err := w.m.run(ctx, w.Path, env, "write-tree")
	if err != nil {
		return "", err
	}
	if !write.Succeeded() {
		return "", fmt.Errorf("snapshot write-tree: %s", firstLine(write.Stderr))
	}
	tree := strings.TrimSpace(write.Stdout)
	if tree == "" {
		return "", fmt.Errorf("write-tree produced no tree id")
	}
	return tree, nil
}

// CheckGitlinks refuses a tree that contains a submodule entry, naming the
// first one it finds. A submodule in the tree would be checked out as an
// empty directory in a detached worktree, and the checks would run against
// sources that are not there — a false failure nobody can diagnose from the
// result alone.
func (w *Worktree) CheckGitlinks(ctx context.Context, tree string) error {
	ls, err := w.m.run(ctx, w.Path, nil, "ls-tree", "-r", tree)
	if err != nil {
		return err
	}
	if !ls.Succeeded() {
		return fmt.Errorf("snapshot ls-tree: %s", firstLine(ls.Stderr))
	}
	for _, line := range strings.Split(ls.Stdout, "\n") {
		if !strings.HasPrefix(line, "160000 ") {
			continue
		}
		name := line
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			name = parts[1]
		}
		return fmt.Errorf("clean verification does not support submodules: %s is a gitlink", name)
	}
	return nil
}

// CommitTree writes a commit object for tree with HEAD as its parent and
// returns its id. No branch or ref is updated: the commit exists in the
// object database and is reachable only from the temporary worktree that
// will be checked out from it, so an interrupted verification leaves no
// trace on any ref (git's own gc collects it once nothing refers to it).
//
// The identity is supplied per invocation, as in Worktree.Commit, so the
// commit does not depend on the host having git configured.
func (w *Worktree) CommitTree(ctx context.Context, tree, message string) (string, error) {
	return w.CommitTreeOnto(ctx, tree, "HEAD", message)
}

// CommitTreeOnto is CommitTree with the parent named explicitly. The success
// commit uses it with the commit HEAD was on when the agent's tree was
// snapshotted, so the tree and its parent are the pair that was recorded
// together — whatever verification did to HEAD in between cannot pair the
// agent's tree with a different history.
func (w *Worktree) CommitTreeOnto(ctx context.Context, tree, parent, message string) (string, error) {
	if strings.TrimSpace(tree) == "" {
		return "", fmt.Errorf("commit-tree: tree id is required")
	}
	if strings.TrimSpace(parent) == "" {
		return "", fmt.Errorf("commit-tree: parent is required")
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("commit-tree: message is required")
	}
	res, err := w.m.run(ctx, w.Path, nil,
		"-c", "user.name="+commitName,
		"-c", "user.email="+commitEmail,
		"commit-tree", tree, "-p", parent, "-m", message)
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("commit-tree in %s: %s", w.Path, firstLine(res.Stderr))
	}
	commit := strings.TrimSpace(res.Stdout)
	if commit == "" {
		return "", fmt.Errorf("commit-tree produced no commit id")
	}
	return commit, nil
}

// TreeOf returns the id of the tree commit points at. Comparing it with
// CurrentTree answers "would a commit on top of this one change anything?"
// without building that commit.
func (w *Worktree) TreeOf(ctx context.Context, commit string) (string, error) {
	if strings.TrimSpace(commit) == "" {
		return "", fmt.Errorf("resolve tree: commit is required")
	}
	res, err := w.m.run(ctx, w.Path, nil, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("resolve the tree of %s in %s: %s", commit, w.Path, firstLine(res.Stderr))
	}
	tree := strings.TrimSpace(res.Stdout)
	if tree == "" {
		return "", fmt.Errorf("rev-parse %s^{tree} produced no tree id", commit)
	}
	return tree, nil
}

// UpdateBranch points branch at commit only if it currently is at expected —
// a compare-and-set on the ref, so a branch that moved under aidev (another
// process, a recovered run) fails the publication instead of being silently
// overwritten. It never re-reads the ref to try again: accepting whatever is
// there now would commit onto history nobody chose.
func (w *Worktree) UpdateBranch(ctx context.Context, branch, commit, expected string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("update-ref: branch is required")
	}
	if strings.TrimSpace(commit) == "" || strings.TrimSpace(expected) == "" {
		return fmt.Errorf("update-ref %s: commit and expected value are required", branch)
	}
	res, err := w.m.run(ctx, w.Path, nil,
		"update-ref", "refs/heads/"+branch, commit, expected)
	if err != nil {
		return err
	}
	if !res.Succeeded() {
		return fmt.Errorf("update-ref %s in %s (expected %s): %s",
			branch, w.Path, expected, firstLine(res.Stderr))
	}
	return nil
}

// SyncIndex rewrites the real index to match HEAD, leaving the files alone.
// After a commit built from a snapshot tree the index still describes the
// tree the worktree was created from, so status would report every committed
// file as staged-for-deletion and a clean worktree would look dirty — and
// removal without --force refuses a dirty one. A mixed reset realigns the
// index without touching the working directory, so status then reports
// exactly the difference between the files and what was committed.
func (w *Worktree) SyncIndex(ctx context.Context) error {
	res, err := w.m.run(ctx, w.Path, nil, "reset")
	if err != nil {
		return err
	}
	if !res.Succeeded() {
		return fmt.Errorf("reset index in %s: %s", w.Path, firstLine(res.Stderr))
	}
	return nil
}

// TreeChange is one path that differs between two trees, with git's own
// status letter: M modified, A added, D deleted, T type changed.
type TreeChange struct {
	Status string
	Path   string
}

// TreeChanges lists the paths that differ between two tree objects. Rename
// detection is off, so the status letter is one of A/M/D/T and a path is
// never two entries — a warning that names half a rename would send a
// reviewer looking for a file that no longer exists under that name.
func (w *Worktree) TreeChanges(ctx context.Context, from, to string) ([]TreeChange, error) {
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return nil, fmt.Errorf("tree diff: both tree ids are required")
	}
	// The same diff-driver caveats as Worktree.Diff apply: reading the file
	// list must not execute code the agent installed in the repository.
	res, err := w.m.run(ctx, w.Path, nil,
		"diff", "--name-status", "--no-renames", "--no-ext-diff", "--no-textconv",
		from, to)
	if err != nil {
		return nil, err
	}
	if !res.Succeeded() {
		return nil, fmt.Errorf("git diff %s %s in %s: %s", from, to, w.Path, firstLine(res.Stderr))
	}
	// A truncated list is worse than no list: it would tell a reviewer the
	// verification touched only some of the files it actually touched.
	if res.StdoutTruncated {
		return nil, fmt.Errorf("tree diff in %s: git diff produced more output than aidev captures, "+
			"so the list of changed files is truncated and cannot be trusted", w.Path)
	}
	var out []TreeChange
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		status, path, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("tree diff in %s: unexpected git output %q", w.Path, line)
		}
		out = append(out, TreeChange{Status: status, Path: path})
	}
	return out, nil
}

// HasGitlinks reports whether the tree at commit contains a submodule entry,
// naming the first one it finds. Clean verification cannot honour submodules —
// the detached checkout would contain empty directories — so a task that would
// verify clean is refused at creation rather than failing mysteriously later.
func (m *Manager) HasGitlinks(ctx context.Context, repo Repository, commit string) (bool, string, error) {
	res, err := m.run(ctx, repo.Path, nil, "ls-tree", "-r", commit)
	if err != nil {
		return false, "", err
	}
	if !res.Succeeded() {
		return false, "", fmt.Errorf("ls-tree %s: %s", commit, firstLine(res.Stderr))
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if !strings.HasPrefix(line, "160000 ") {
			continue
		}
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			return true, parts[1], nil
		}
		return true, "", nil
	}
	return false, "", nil
}

// CreateDetached checks out commit into a new temporary worktree with no
// branch, under the workspace root like every other worktree aidev makes.
// It exists for clean verification: a place to run the checks where the
// tree being checked is exactly what will be committed, and where anything
// the checks do cannot leak back into the agent's worktree.
//
// Branch is left empty on the returned worktree — the checkout is detached,
// and removing it must not be mistaken for delivering work to a branch.
func (m *Manager) CreateDetached(ctx context.Context, repo Repository, name, commit string) (*Worktree, error) {
	if repo.Path == "" {
		return nil, fmt.Errorf("create detached worktree: repository is required")
	}
	if strings.TrimSpace(commit) == "" {
		return nil, fmt.Errorf("create detached worktree: commit is required")
	}
	path, err := m.resolveWorktreePath(name)
	if err != nil {
		return nil, err
	}
	if err := validateIsolation(repo.Path, path); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrWorktreeExists, path)
	}
	if err := m.ensureWorkspaceRoot(); err != nil {
		return nil, err
	}

	res, err := m.run(ctx, repo.Path, nil, "worktree", "add", "--detach", "--", path, commit)
	if err != nil {
		return nil, err
	}
	if !res.Succeeded() {
		return nil, classifyWorktreeAdd(res.Stderr, "", path)
	}
	return &Worktree{Path: path, BaseCommit: commit, repo: repo, m: m}, nil
}

// snapshotStep runs one git command of the snapshot on the temporary index,
// naming the step on failure so a broken snapshot says which command broke
// rather than only that it did.
func (w *Worktree) snapshotStep(ctx context.Context, env []string, step string, args ...string) error {
	res, err := w.m.run(ctx, w.Path, env, args...)
	if err != nil {
		return err
	}
	if !res.Succeeded() {
		return fmt.Errorf("snapshot %s: %s", step, firstLine(res.Stderr))
	}
	return nil
}
