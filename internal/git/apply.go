package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Applying a task's result to the repository a person works in. These are
// the only commands of aidev's that write to the main checkout, and only
// when that person runs `aidev task apply` or `aidev task undo`: nothing in
// a run calls them.

// ErrConflict means a merge or revert would conflict. It was aborted, so the
// checkout is exactly as it was; Conflicts names the files.
var ErrConflict = errors.New("the change conflicts with the checked-out branch")

// ConflictError carries the conflicting paths and answers errors.Is for
// ErrConflict.
type ConflictError struct {
	Op        string
	Conflicts []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s would conflict in %s; nothing was changed", e.Op, strings.Join(e.Conflicts, ", "))
}

func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// CheckedOutBranch returns the branch the repository's main checkout is on,
// or an error when HEAD is detached: there is no branch to apply to then,
// and guessing one would put work where the person is not looking.
func (m *Manager) CheckedOutBranch(ctx context.Context, repo Repository) (string, error) {
	res, err := m.run(ctx, repo.Path, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("%s is not on a branch (detached HEAD); check out the branch to apply to first", repo.Path)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// TrackedChanges lists files in the main checkout whose tracked content
// differs from HEAD. Untracked files are not listed: they are common and
// harmless, and a merge that would overwrite one refuses on its own.
func (m *Manager) TrackedChanges(ctx context.Context, repo Repository) ([]string, error) {
	res, err := m.run(ctx, repo.Path, nil, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	if !res.Succeeded() {
		return nil, fmt.Errorf("git status in %s: %s", repo.Path, firstLine(res.Stderr))
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		if len(line) > 3 {
			out = append(out, strings.TrimSpace(line[3:]))
		}
	}
	return out, nil
}

// MergeNoFF merges commit into the checked-out branch with a merge commit,
// even when a fast-forward would do, so the applied task stays one commit a
// person can find and revert. On a conflict it aborts and returns a
// *ConflictError. The identity is aidev's, as for every commit it makes.
func (m *Manager) MergeNoFF(ctx context.Context, repo Repository, commit, message string) (string, error) {
	res, err := m.run(ctx, repo.Path, nil,
		"-c", "user.name="+commitName, "-c", "user.email="+commitEmail,
		"merge", "--no-ff", "--no-edit", "-m", message, commit)
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", m.abortWithConflicts(ctx, repo, "the merge", []string{"merge", "--abort"}, res.Stderr)
	}
	return m.headOf(ctx, repo)
}

// Revert creates a commit that undoes commit on the checked-out branch.
// mainline 1 reverts a merge to its first parent; 0 reverts an ordinary
// commit. On a conflict it aborts and returns a *ConflictError.
func (m *Manager) Revert(ctx context.Context, repo Repository, commit string, mainline int) (string, error) {
	args := []string{"-c", "user.name=" + commitName, "-c", "user.email=" + commitEmail, "revert", "--no-edit"}
	if mainline > 0 {
		args = append(args, "-m", fmt.Sprint(mainline))
	}
	args = append(args, commit)
	res, err := m.run(ctx, repo.Path, nil, args...)
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", m.abortWithConflicts(ctx, repo, "the revert", []string{"revert", "--abort"}, res.Stderr)
	}
	return m.headOf(ctx, repo)
}

// abortWithConflicts lists the unmerged paths, aborts the operation and
// reports them. With no unmerged path the failure was something else, which
// is returned as it is.
func (m *Manager) abortWithConflicts(ctx context.Context, repo Repository, op string, abort []string, stderr string) error {
	list, err := m.run(ctx, repo.Path, nil, "diff", "--name-only", "--diff-filter=U")
	var conflicts []string
	if err == nil && list.Succeeded() {
		for _, p := range strings.Split(strings.TrimSpace(list.Stdout), "\n") {
			if p != "" {
				conflicts = append(conflicts, p)
			}
		}
	}
	if res, err := m.run(ctx, repo.Path, nil, abort...); err != nil || !res.Succeeded() {
		detail := ""
		if res.Stderr != "" {
			detail = ": " + firstLine(res.Stderr)
		}
		return fmt.Errorf("%s failed and could not be aborted%s; check %s by hand", op, detail, repo.Path)
	}
	if len(conflicts) == 0 {
		return fmt.Errorf("%s in %s failed: %s", op, repo.Path, firstLine(stderr))
	}
	return &ConflictError{Op: op, Conflicts: conflicts}
}

func (m *Manager) headOf(ctx context.Context, repo Repository) (string, error) {
	res, err := m.run(ctx, repo.Path, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("resolve HEAD in %s: %s", repo.Path, firstLine(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// DeleteBranch removes a branch, for the branch an automatic retry created
// before its attempt was taken away. Plumbing: `git branch -D` runs no hooks.
// A branch that is not there is not an error — the point is that it is gone.
func (m *Manager) DeleteBranch(ctx context.Context, repo Repository, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("delete branch: branch is required")
	}
	res, err := m.run(ctx, repo.Path, nil, "branch", "-D", "--", branch)
	if err != nil {
		return err
	}
	if !res.Succeeded() {
		if strings.Contains(res.Stderr, "not found") {
			return nil
		}
		return fmt.Errorf("delete branch %s in %s: %s", branch, repo.Path, firstLine(res.Stderr))
	}
	return nil
}

// AbandonBranch gives up a branch the worktree was moved onto — the branch an
// automatic retry created just before its attempt was taken away. git refuses
// to delete the branch a worktree has checked out, so HEAD is detached at the
// same commit first; the work is not lost, it stays reachable from the branch
// it was on before, and from HEAD. Plumbing only: `symbolic-ref` and
// `update-ref` run no hooks and touch no files.
func (w *Worktree) AbandonBranch(ctx context.Context, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("abandon branch: branch is required")
	}
	want := "refs/heads/" + branch

	res, err := w.m.run(ctx, w.Path, nil, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return err
	}
	if res.Succeeded() && strings.TrimSpace(res.Stdout) == want {
		// git refuses to delete HEAD's branch, so HEAD is made a direct ref
		// at the commit it already points at: --no-deref is what detaches it
		// without touching a single file in the worktree.
		head, err := w.m.run(ctx, w.Path, nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if !head.Succeeded() {
			return fmt.Errorf("resolve HEAD in %s: %s", w.Path, firstLine(head.Stderr))
		}
		detach, err := w.m.run(ctx, w.Path, nil, "update-ref", "--no-deref", "HEAD", strings.TrimSpace(head.Stdout))
		if err != nil {
			return err
		}
		if !detach.Succeeded() {
			return fmt.Errorf("detach HEAD in %s before abandoning %s: %s", w.Path, branch, firstLine(detach.Stderr))
		}
	}

	del, err := w.m.run(ctx, w.Path, nil, "update-ref", "-d", want)
	if err != nil {
		return err
	}
	if !del.Succeeded() {
		return fmt.Errorf("delete branch %s in %s: %s", branch, w.Path, firstLine(del.Stderr))
	}
	// The worktree is no longer on any branch, which is what it is.
	w.Branch = ""
	return nil
}
