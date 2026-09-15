// Package git wraps the git commands aidev needs, so that shell invocations are
// not scattered across the codebase and so that every one of them inherits a
// timeout, bounded output, and consistent error classification.
//
// The package enforces aidev's central safety property: an agent never runs in
// the repository's main working tree. Worktree paths are validated to resolve
// inside workspace_root and to lie outside the repository itself.
package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aidev/internal/procexec"
)

// Defaults for git invocations. Git operations are local and fast; a minute is
// generous even for a large checkout, and bounding them stops a hung git from
// stalling a task indefinitely.
const (
	DefaultCommand        = "git"
	DefaultTimeout        = 60 * time.Second
	DefaultMaxOutputBytes = 4 << 20 // 4 MiB: a diff can legitimately be large

	// commitIdentity is used when committing an attempt's result. Passing an
	// identity explicitly means aidev does not depend on the host having git
	// user.name and user.email configured, which Phase 0 confirmed works.
	commitName  = "aidev"
	commitEmail = "aidev@localhost"
)

// Manager creates and inspects worktrees under a single workspace root.
type Manager struct {
	// WorkspaceRoot is the only directory worktrees may be created in.
	WorkspaceRoot string

	Command        string
	Timeout        time.Duration
	MaxOutputBytes int
}

// NewManager returns a Manager with defaults applied. workspaceRoot must be an
// absolute path; it is created on demand.
func NewManager(workspaceRoot string) (*Manager, error) {
	if !filepath.IsAbs(workspaceRoot) {
		return nil, fmt.Errorf("workspace root %q must be absolute", workspaceRoot)
	}
	return &Manager{
		WorkspaceRoot:  filepath.Clean(workspaceRoot),
		Command:        DefaultCommand,
		Timeout:        DefaultTimeout,
		MaxOutputBytes: DefaultMaxOutputBytes,
	}, nil
}

// Repository is a validated git repository.
type Repository struct {
	// Path is the canonical top level of the repository's main working tree.
	Path string

	// RootCommit is the repository's first commit, empty for a repository with
	// no commits yet.
	RootCommit string
}

// OpenRepository verifies that path is inside a git repository and returns its
// canonical top level. Everything else in the package takes a Repository rather
// than a bare path, so an unvalidated path cannot reach a git command.
func (m *Manager) OpenRepository(ctx context.Context, path string) (Repository, error) {
	if strings.TrimSpace(path) == "" {
		return Repository{}, fmt.Errorf("%w: no path given", ErrNotARepository)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Repository{}, fmt.Errorf("resolve %q: %w", path, err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return Repository{}, fmt.Errorf("%w: %s is not a directory", ErrNotARepository, abs)
	}

	res, err := m.run(ctx, abs, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return Repository{}, err
	}
	if !res.Succeeded() {
		return Repository{}, fmt.Errorf("%w: %s", ErrNotARepository, firstLine(res.Stderr))
	}
	top := strings.TrimSpace(res.Stdout)
	if top == "" {
		return Repository{}, fmt.Errorf("%w: %s", ErrNotARepository, abs)
	}

	repo := Repository{Path: filepath.Clean(top)}

	// The root commit is how OpenCode identifies a project (docs/research.md
	// §2.8), which makes it useful context when diagnosing agent behaviour. A
	// repository with no commits yet is not an error here.
	if rootRes, err := m.run(ctx, repo.Path, nil, "rev-list", "--max-parents=0", "HEAD"); err == nil && rootRes.Succeeded() {
		lines := strings.Fields(rootRes.Stdout)
		if len(lines) > 0 {
			repo.RootCommit = lines[len(lines)-1]
		}
	}
	return repo, nil
}

// ResolveCommit returns the commit a ref points at. An empty ref means HEAD.
func (m *Manager) ResolveCommit(ctx context.Context, repo Repository, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		ref = "HEAD"
	}
	res, err := m.run(ctx, repo.Path, nil, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("%w: %s in %s", ErrUnknownRevision, ref, repo.Path)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// CreateRequest describes a worktree to create.
type CreateRequest struct {
	Repository Repository

	// Name becomes the directory under WorkspaceRoot. It is restricted to
	// characters that are unambiguous in a path.
	Name string

	// Branch is the new branch to create. Required: aidev never checks out an
	// existing branch into a task worktree, because that would let two tasks
	// share a branch.
	Branch string

	// BaseRef is what the branch starts from. Empty means HEAD.
	BaseRef string
}

// Worktree is a created, isolated checkout.
type Worktree struct {
	Path       string
	Branch     string
	BaseCommit string

	repo Repository
	m    *Manager
}

// Repository returns the repository this worktree belongs to.
func (w *Worktree) Repository() Repository { return w.repo }

// Create makes a new worktree on a new branch.
//
// The path is validated before git is invoked: it must resolve inside
// workspace_root and outside the repository. Those two checks are what make
// "the agent cannot touch the main working tree" a property of the code rather
// than a convention.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Worktree, error) {
	if req.Repository.Path == "" {
		return nil, fmt.Errorf("create worktree: repository is required")
	}
	if strings.TrimSpace(req.Branch) == "" {
		return nil, fmt.Errorf("create worktree: branch is required")
	}
	if err := validateBranchName(req.Branch); err != nil {
		return nil, err
	}

	path, err := m.resolveWorktreePath(req.Name)
	if err != nil {
		return nil, err
	}
	if err := validateIsolation(req.Repository.Path, path); err != nil {
		return nil, err
	}

	baseCommit, err := m.ResolveCommit(ctx, req.Repository, req.BaseRef)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrWorktreeExists, path)
	}
	if err := m.ensureWorkspaceRoot(); err != nil {
		return nil, err
	}

	res, err := m.run(ctx, req.Repository.Path, nil,
		"worktree", "add", "-b", req.Branch, "--", path, baseCommit)
	if err != nil {
		return nil, err
	}
	if !res.Succeeded() {
		return nil, classifyWorktreeAdd(res.Stderr, req.Branch, path)
	}

	return &Worktree{
		Path:       path,
		Branch:     req.Branch,
		BaseCommit: baseCommit,
		repo:       req.Repository,
		m:          m,
	}, nil
}

// StatusEntry is one line of porcelain status.
type StatusEntry struct {
	// Code is git's two-character status code, for example " M" or "??".
	Code string
	Path string
}

// Status is a worktree's working-tree state.
type Status struct {
	Entries []StatusEntry
}

// Clean reports whether there is nothing to commit.
func (s Status) Clean() bool { return len(s.Entries) == 0 }

var statusLine = regexp.MustCompile(`^(..) (.*)$`)

// Status reports the worktree's modified and untracked files.
func (w *Worktree) Status(ctx context.Context) (Status, error) {
	res, err := w.m.run(ctx, w.Path, nil, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return Status{}, err
	}
	if !res.Succeeded() {
		return Status{}, fmt.Errorf("git status in %s: %s", w.Path, firstLine(res.Stderr))
	}

	var status Status
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line == "" {
			continue
		}
		if m := statusLine.FindStringSubmatch(line); m != nil {
			status.Entries = append(status.Entries, StatusEntry{Code: m[1], Path: m[2]})
		}
	}
	return status, nil
}

// Diff is what an agent changed in a worktree.
type Diff struct {
	Patch        string
	Truncated    bool
	ChangedFiles int
	Insertions   int
	Deletions    int
}

// Diff collects the worktree's changes, including files the agent created.
//
// Two things make this less trivial than `git diff`. First, git ignores
// untracked files, so new files — the agent's main output — would be invisible
// without `git add -N` (docs/research.md §4.4). Second, running `git add -N` on
// the worktree's real index would change what a human later sees in `git status`
// when inspecting a retained failed attempt. So the intent-to-add is recorded in
// a throwaway copy of the index and the real one is left untouched
// (docs/research.md §7b).
func (w *Worktree) Diff(ctx context.Context) (Diff, error) {
	tempIndex, cleanup, err := w.temporaryIndex()
	if err != nil {
		return Diff{}, err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tempIndex}

	if res, err := w.m.run(ctx, w.Path, env, "add", "-N", "--", "."); err != nil {
		return Diff{}, err
	} else if !res.Succeeded() {
		return Diff{}, fmt.Errorf("stage intent-to-add in %s: %s", w.Path, firstLine(res.Stderr))
	}

	// A diff against the index loses work the agent committed, when the index
	// matches the files. Comparing against the base commit keeps it in the
	// record (docs/research.md 7e).
	patchArgs := []string{"diff", "--no-color"}
	statArgs := []string{"diff", "--numstat"}
	if strings.TrimSpace(w.BaseCommit) != "" {
		patchArgs = append(patchArgs, w.BaseCommit)
		statArgs = append(statArgs, w.BaseCommit)
	}

	patchRes, err := w.m.run(ctx, w.Path, env, patchArgs...)
	if err != nil {
		return Diff{}, err
	}
	if !patchRes.Succeeded() {
		return Diff{}, fmt.Errorf("git diff in %s: %s", w.Path, firstLine(patchRes.Stderr))
	}

	statRes, err := w.m.run(ctx, w.Path, env, statArgs...)
	if err != nil {
		return Diff{}, err
	}
	if !statRes.Succeeded() {
		return Diff{}, fmt.Errorf("git diff --numstat in %s: %s", w.Path, firstLine(statRes.Stderr))
	}

	diff := Diff{Patch: patchRes.Stdout, Truncated: patchRes.StdoutTruncated}
	for _, line := range strings.Split(statRes.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		diff.ChangedFiles++
		// Binary files are reported as "-", which is not an error.
		if n, err := strconv.Atoi(fields[0]); err == nil {
			diff.Insertions += n
		}
		if n, err := strconv.Atoi(fields[1]); err == nil {
			diff.Deletions += n
		}
	}
	return diff, nil
}

// HeadCommit returns the commit the worktree currently has checked out.
func (w *Worktree) HeadCommit(ctx context.Context) (string, error) {
	res, err := w.m.run(ctx, w.Path, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("resolve HEAD in %s: %s", w.Path, firstLine(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Commit records everything in the worktree on its branch and returns the new
// commit. It returns an empty string when there was nothing to commit.
//
// This is how a successful attempt's work survives the worktree being removed:
// the branch keeps it, reviewable with ordinary git commands. It is not a merge —
// nothing outside the task's own branch is touched.
func (w *Worktree) Commit(ctx context.Context, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("commit in %s: message is required", w.Path)
	}

	status, err := w.Status(ctx)
	if err != nil {
		return "", err
	}
	if status.Clean() {
		return "", nil
	}

	if res, err := w.m.run(ctx, w.Path, nil, "add", "--all", "--", "."); err != nil {
		return "", err
	} else if !res.Succeeded() {
		return "", fmt.Errorf("git add in %s: %s", w.Path, firstLine(res.Stderr))
	}

	// The identity is supplied per invocation so that committing does not
	// depend on the host having git configured.
	res, err := w.m.run(ctx, w.Path, nil,
		"-c", "user.name="+commitName,
		"-c", "user.email="+commitEmail,
		"commit", "--no-verify", "--message", message)
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("git commit in %s: %s", w.Path, firstLine(res.Stderr))
	}
	return w.HeadCommit(ctx)
}

// Remove deletes the worktree directory and its administrative files.
//
// force is required to discard uncommitted work, and aidev only passes it when a
// human has asked. Without it git refuses, which is exactly the guard that keeps
// a failed attempt's work from being thrown away.
func (m *Manager) Remove(ctx context.Context, w *Worktree, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", w.Path)

	res, err := m.run(ctx, w.repo.Path, nil, args...)
	if err != nil {
		return err
	}
	if !res.Succeeded() {
		if strings.Contains(res.Stderr, "contains modified or untracked files") {
			return fmt.Errorf("%w: %s", ErrWorktreeDirty, w.Path)
		}
		return fmt.Errorf("remove worktree %s: %s", w.Path, firstLine(res.Stderr))
	}
	return nil
}

// Prune drops administrative records for worktree directories that no longer
// exist, for example after one was deleted outside aidev.
func (m *Manager) Prune(ctx context.Context, repo Repository) error {
	res, err := m.run(ctx, repo.Path, nil, "worktree", "prune")
	if err != nil {
		return err
	}
	if !res.Succeeded() {
		return fmt.Errorf("prune worktrees in %s: %s", repo.Path, firstLine(res.Stderr))
	}
	return nil
}

// Attach returns a handle for a worktree aidev created earlier, so that a path
// recorded in the database can be operated on in a later process. The path is
// re-validated, because a stored value is not automatically trustworthy.
func (m *Manager) Attach(ctx context.Context, repo Repository, path, branch string) (*Worktree, error) {
	clean := filepath.Clean(path)
	if err := m.withinWorkspace(clean); err != nil {
		return nil, err
	}
	if err := validateIsolation(repo.Path, clean); err != nil {
		return nil, err
	}
	if info, err := os.Stat(clean); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("worktree %s is no longer present", clean)
	}
	return &Worktree{Path: clean, Branch: branch, repo: repo, m: m}, nil
}

// temporaryIndex copies the worktree's index so that intent-to-add entries can
// be made without touching the real one. A worktree with no index yet (fresh
// checkout) simply starts from an empty file.
func (w *Worktree) temporaryIndex() (string, func(), error) {
	temp, err := os.CreateTemp("", "aidev-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary git index: %w", err)
	}
	path := temp.Name()
	cleanup := func() { _ = os.Remove(path) }

	if err := temp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create temporary git index: %w", err)
	}

	existing := filepath.Join(w.Path, ".git")
	if info, err := os.Stat(existing); err == nil && info.IsDir() {
		// A plain repository: .git is a directory holding the index.
		existing = filepath.Join(existing, "index")
	} else {
		// A linked worktree: .git is a file pointing at the admin directory.
		dir, err := w.gitDir()
		if err != nil {
			cleanup()
			return "", nil, err
		}
		existing = filepath.Join(dir, "index")
	}

	data, err := os.ReadFile(existing)
	if err != nil {
		if os.IsNotExist(err) {
			return path, cleanup, nil
		}
		cleanup()
		return "", nil, fmt.Errorf("read git index %s: %w", existing, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy git index: %w", err)
	}
	return path, cleanup, nil
}

// gitDir reads the administrative directory of a linked worktree from its .git
// file, whose content is "gitdir: <path>".
func (w *Worktree) gitDir() (string, error) {
	data, err := os.ReadFile(filepath.Join(w.Path, ".git"))
	if err != nil {
		return "", fmt.Errorf("read %s/.git: %w", w.Path, err)
	}
	line := strings.TrimSpace(string(data))
	rest, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", fmt.Errorf("unexpected .git content in %s", w.Path)
	}
	return strings.TrimSpace(rest), nil
}

// run invokes git with aidev's process guarantees.
func (m *Manager) run(ctx context.Context, dir string, env []string, args ...string) (procexec.Result, error) {
	command := m.Command
	if command == "" {
		command = DefaultCommand
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxOutput := m.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = DefaultMaxOutputBytes
	}

	return procexec.Run(ctx, procexec.Spec{
		Command: command,
		Args:    args,
		Dir:     dir,
		// Keep git's output machine-readable and independent of the user's
		// configuration: no pager, no colour, no localised messages. Error
		// classification matches English text, so the locale must be fixed.
		ExtraEnv: append([]string{
			"GIT_TERMINAL_PROMPT=0",
			"GIT_PAGER=cat",
			"GIT_OPTIONAL_LOCKS=0",
			"LC_ALL=C",
		}, env...),
		Timeout:        timeout,
		MaxOutputBytes: maxOutput,
	})
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

// classifyWorktreeAdd maps git's stderr onto a sentinel error. Matching text is
// necessary because the exit statuses are not distinct (docs/research.md §4.2).
func classifyWorktreeAdd(stderr, branch, path string) error {
	switch {
	case strings.Contains(stderr, "already exists") && strings.Contains(stderr, "branch named"):
		return fmt.Errorf("%w: %s", ErrBranchExists, branch)
	case strings.Contains(stderr, "is already used by worktree"):
		return fmt.Errorf("%w: %s", ErrBranchInUse, branch)
	case strings.Contains(stderr, "already exists"):
		return fmt.Errorf("%w: %s", ErrWorktreeExists, path)
	default:
		return fmt.Errorf("create worktree %s: %s", path, firstLine(stderr))
	}
}

// CurrentBranch reports the branch the repository currently has checked out,
// falling back to "main" when it cannot be determined.
//
// A repository using "master", "trunk" or anything else must not be silently
// assumed to use "main": the value becomes the project's default base ref, and
// getting it wrong would make every task branch from the wrong place.
func (m *Manager) CurrentBranch(ctx context.Context, repo Repository) string {
	res, err := m.run(ctx, repo.Path, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || !res.Succeeded() {
		return "main"
	}
	branch := strings.TrimSpace(res.Stdout)
	if branch == "" || branch == "HEAD" {
		// Detached HEAD: there is no current branch to record.
		return "main"
	}
	return branch
}
