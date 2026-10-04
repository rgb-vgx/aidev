package git

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// SnapshotTree records the worktree's current content as a tree object and
// returns its id. It is the input to a clean verification: the tree a commit
// would carry, not the working directory as the agent happened to leave it.
//
// The snapshot runs on a fresh temporary index — never the real one, and
// never the intent-to-add copy Diff() uses. Intent-to-add entries have no
// content, so a tree built from them would record a file the worktree does
// not actually contain; and touching the real index would race anyone else
// reading it. read-tree HEAD seeds the index with what is checked out, then
// add -A brings it in line with the working directory: untracked files
// (except those git ignores) join, deletions leave, modifications land.
func (w *Worktree) SnapshotTree(ctx context.Context) (string, error) {
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

	// A submodule entry in the tree would be checked out as an empty
	// directory in the detached worktree, and the checks would run against
	// sources that are not there — a false failure nobody can diagnose from
	// the result alone. Refuse instead, naming the offender.
	ls, err := w.m.run(ctx, w.Path, nil, "ls-tree", "-r", tree)
	if err != nil {
		return "", err
	}
	if !ls.Succeeded() {
		return "", fmt.Errorf("snapshot ls-tree: %s", firstLine(ls.Stderr))
	}
	for _, line := range strings.Split(ls.Stdout, "\n") {
		if !strings.HasPrefix(line, "160000 ") {
			continue
		}
		name := line
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			name = parts[1]
		}
		return "", fmt.Errorf("clean verification does not support submodules: %s is a gitlink", name)
	}
	return tree, nil
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
	if strings.TrimSpace(tree) == "" {
		return "", fmt.Errorf("commit-tree: tree id is required")
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("commit-tree: message is required")
	}
	res, err := w.m.run(ctx, w.Path, nil,
		"-c", "user.name="+commitName,
		"-c", "user.email="+commitEmail,
		"commit-tree", tree, "-p", "HEAD", "-m", message)
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
