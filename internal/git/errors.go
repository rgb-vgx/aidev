package git

import "errors"

// Sentinel errors for the git failures aidev has to react to differently.
//
// They are matched from git's stderr text rather than from its exit status,
// because Phase 0 measured inconsistent statuses for comparable problems: a
// branch that already exists exits 255, while a branch already in use by another
// worktree exits 128 (docs/research.md §4.2).
var (
	// ErrNotARepository means the path is not inside a git repository.
	ErrNotARepository = errors.New("not a git repository")

	// ErrBranchExists means the requested branch name is already taken.
	ErrBranchExists = errors.New("branch already exists")

	// ErrBranchInUse means another worktree has the branch checked out.
	ErrBranchInUse = errors.New("branch is already checked out in another worktree")

	// ErrWorktreeExists means the target directory is already present.
	ErrWorktreeExists = errors.New("worktree path already exists")

	// ErrWorktreeDirty means removal was refused because the worktree holds
	// uncommitted or untracked work. aidev treats this as a feature: it is what
	// stops a failed attempt's work from being discarded.
	ErrWorktreeDirty = errors.New("worktree contains uncommitted work")

	// ErrUnknownRevision means the requested base ref does not resolve.
	ErrUnknownRevision = errors.New("revision does not exist")

	// ErrOutsideWorkspace means a path would resolve outside WORKSPACE_ROOT.
	ErrOutsideWorkspace = errors.New("path escapes the configured workspace root")

	// ErrInsideRepository means a worktree path would land inside the
	// repository it is isolating work from, which would defeat the isolation.
	ErrInsideRepository = errors.New("worktree path is inside the repository")

	// ErrInvalidName means a worktree name contains characters aidev will not
	// put into a filesystem path.
	ErrInvalidName = errors.New("invalid worktree name")
)
