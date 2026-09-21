package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Submodule is one gitlink the base commit pins, together with the local
// repository holding its objects.
type Submodule struct {
	// Path is relative to the worktree root, as git records it.
	Path string

	// Name is the section name in .gitmodules, which is what
	// <repo>/.git/modules/<name> is keyed by and need not equal Path.
	Name string

	// Commit is the SHA the parent repository pins at the base commit.
	Commit string

	// gitDir is the submodule's own repository. Populating the worktree means
	// adding a linked worktree of *this* repository, not touching the parent.
	gitDir string
}

// loadSubmodules gives a freshly created worktree the content of every
// submodule the base commit pins, each at the pinned commit.
//
// Each submodule becomes a linked worktree of the submodule's own repository,
// placed where the gitlink says. The alternative, `git submodule update --init`
// inside the worktree, re-clones every submodule from its remote for every task:
// a network fetch and a full copy of the object store each time, with no
// alternates (docs/research.md §7h). A linked worktree needs no network, shares
// the objects that are already on disk, and gives the task its own HEAD.
//
// On any failure the submodule worktrees added so far are removed again, so a
// failed creation does not leave half a checkout behind.
func (m *Manager) loadSubmodules(ctx context.Context, w *Worktree) error {
	subs, err := m.pinnedSubmodules(ctx, w.repo, w.BaseCommit)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}

	loaded := make([]Submodule, 0, len(subs))
	for _, sub := range subs {
		if err := m.addSubmoduleWorktree(ctx, w, sub); err != nil {
			// Best effort: the caller is about to see the original error, and a
			// failure to undo must not replace it with a less informative one.
			for _, done := range loaded {
				_ = m.removeSubmoduleWorktree(ctx, w, done.Path, true)
			}
			return err
		}
		loaded = append(loaded, sub)
		w.Submodules = append(w.Submodules, sub.Path)
	}
	return nil
}

// addSubmoduleWorktree checks one submodule out inside the worktree.
func (m *Manager) addSubmoduleWorktree(ctx context.Context, w *Worktree, sub Submodule) error {
	target, err := submodulePath(w.Path, sub.Path)
	if err != nil {
		return err
	}

	// A commit the parent pins is not necessarily one the local submodule
	// repository has fetched. Say so plainly rather than letting `worktree add`
	// report it as an unknown revision, because the fix is a fetch in a
	// repository the operator has to go and find.
	if res, err := m.run(ctx, sub.gitDir, nil, "cat-file", "-e", sub.Commit+"^{commit}"); err != nil {
		return err
	} else if !res.Succeeded() {
		return fmt.Errorf("%w: %s pins %s at %s, which %s does not have; fetch it there first",
			ErrSubmoduleUnavailable, w.repo.Path, sub.Path, sub.Commit, sub.gitDir)
	}

	// The path must be absolute: git resolves a relative one against the
	// submodule's git directory, silently creating the checkout under
	// .git/modules (docs/research.md §7h).
	res, err := m.run(ctx, sub.gitDir, nil, "worktree", "add", "--detach", "--", target, sub.Commit)
	if err != nil {
		return err
	}
	if !res.Succeeded() {
		return fmt.Errorf("%w: check out %s at %s: %s",
			ErrSubmoduleUnavailable, sub.Path, sub.Commit, firstLine(res.Stderr))
	}
	return nil
}

// removeSubmoduleWorktrees takes back every submodule checkout inside a
// worktree, so that the worktree itself can be removed.
//
// Two measured facts shape this (docs/research.md §7h). Git refuses outright to
// remove a working tree that contains submodules, so this has to happen first.
// And `git worktree remove` deletes the submodule directory, which leaves the
// parent worktree reporting " D <path>" — enough for git to refuse removal
// without --force, which is aidev's guard against discarding failed work. So the
// empty directory is put back, exactly as `git worktree add` leaves it.
func (m *Manager) removeSubmoduleWorktrees(ctx context.Context, w *Worktree, force bool) error {
	// A repository without submodules must not pay for this. The stat costs
	// nothing; running git to find out would be a command per removal.
	if _, err := os.Stat(filepath.Join(w.Path, ".gitmodules")); err != nil {
		return nil
	}

	paths, err := m.gitlinkPaths(ctx, w)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := m.removeSubmoduleWorktree(ctx, w, path, force); err != nil {
			return err
		}
	}
	return nil
}

// removeSubmoduleWorktree removes one submodule checkout and restores the empty
// directory git expects in its place. A submodule that was never populated is
// not an error: the flag may have been off when the worktree was created.
func (m *Manager) removeSubmoduleWorktree(ctx context.Context, w *Worktree, path string, force bool) error {
	target, err := submodulePath(w.Path, path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		return nil
	}

	gitDir, err := m.repositoryAt(ctx, target)
	if err != nil {
		return err
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", target)

	res, err := m.run(ctx, gitDir, nil, args...)
	if err != nil {
		return err
	}
	if !res.Succeeded() {
		if strings.Contains(res.Stderr, "contains modified or untracked files") {
			return fmt.Errorf("%w: %s", ErrWorktreeDirty, target)
		}
		return fmt.Errorf("remove submodule worktree %s: %s", target, firstLine(res.Stderr))
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("restore submodule directory %s: %w", target, err)
	}
	return nil
}

// pinnedSubmodules reports the gitlinks a commit records, with the local
// repository each one lives in.
//
// The commit is the authority on what is pinned; .gitmodules only supplies the
// name, and the two can disagree — a gitmodules entry whose gitlink the commit
// does not record is not a submodule of this checkout.
func (m *Manager) pinnedSubmodules(ctx context.Context, repo Repository, commit string) ([]Submodule, error) {
	if strings.TrimSpace(commit) == "" {
		return nil, nil
	}

	res, err := m.run(ctx, repo.Path, nil, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", commit)
	if err != nil {
		return nil, err
	}
	if !res.Succeeded() {
		return nil, fmt.Errorf("list submodules of %s at %s: %s", repo.Path, commit, firstLine(res.Stderr))
	}

	var subs []Submodule
	for _, entry := range strings.Split(res.Stdout, "\x00") {
		// "<mode> <type> <object>\t<path>"
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			continue
		}
		subs = append(subs, Submodule{Path: path, Commit: fields[2]})
	}
	if len(subs) == 0 {
		return nil, nil
	}

	names, err := m.submoduleNames(ctx, repo, commit)
	if err != nil {
		return nil, err
	}
	for i := range subs {
		subs[i].Name = names[subs[i].Path]
		gitDir, err := m.submoduleGitDir(ctx, repo, subs[i])
		if err != nil {
			return nil, err
		}
		subs[i].gitDir = gitDir
	}
	return subs, nil
}

// submoduleNames maps each submodule path to its .gitmodules section name, read
// from the commit rather than from the working tree so that it describes what is
// being checked out. A commit with no .gitmodules yields an empty map, which is
// not an error: the name is only needed for one of the two possible layouts.
func (m *Manager) submoduleNames(ctx context.Context, repo Repository, commit string) (map[string]string, error) {
	res, err := m.run(ctx, repo.Path, nil,
		"config", "--blob", commit+":.gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	if !res.Succeeded() {
		return names, nil
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		key, path, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
		if name != "" && path != "" {
			names[path] = name
		}
	}
	return names, nil
}

// submoduleGitDir finds the repository holding a submodule's objects.
//
// There are two layouts in the wild and a real repository can have both at once
// (docs/research.md §7h): an absorbed submodule keeps its git directory at
// <repo>/.git/modules/<name>, a cloned one keeps an ordinary .git directory
// inside the submodule itself. Asking git about the submodule directory answers
// for either, and .git/modules/<name> covers the remaining case where the main
// working tree has the submodule deinitialised.
func (m *Manager) submoduleGitDir(ctx context.Context, repo Repository, sub Submodule) (string, error) {
	inMainTree, err := submodulePath(repo.Path, sub.Path)
	if err != nil {
		return "", err
	}
	if gitDir, err := m.repositoryAt(ctx, inMainTree); err == nil {
		return gitDir, nil
	}

	if sub.Name != "" {
		if common, err := m.gitCommonDir(ctx, repo.Path); err == nil {
			candidate := filepath.Join(common, "modules", sub.Name)
			if gitDir, err := m.repositoryAt(ctx, candidate); err == nil {
				return gitDir, nil
			}
		}
	}

	return "", fmt.Errorf("%w: %s has no local repository for submodule %s; "+
		"run `git submodule update --init %s` in %s once, and aidev will use it from then on",
		ErrSubmoduleUnavailable, repo.Path, sub.Path, sub.Path, repo.Path)
}

// repositoryAt returns the git directory of the repository whose working tree is
// exactly dir.
//
// The equality check is the point. Asked about an empty submodule directory, git
// walks up and answers for the enclosing repository instead (docs/research.md
// §7h) — which would have aidev add a worktree of the parent where a submodule
// belongs.
func (m *Manager) repositoryAt(ctx context.Context, dir string) (string, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrNotARepository, dir)
	}
	res, err := m.run(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("%w: %s", ErrNotARepository, dir)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 2 {
		return "", fmt.Errorf("%w: %s", ErrNotARepository, dir)
	}
	top, gitDir := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	if resolveExisting(top) != resolveExisting(dir) {
		return "", fmt.Errorf("%w: %s belongs to %s", ErrNotARepository, dir, top)
	}
	return gitDir, nil
}

// gitCommonDir returns the git directory shared by every worktree of dir.
func (m *Manager) gitCommonDir(ctx context.Context, dir string) (string, error) {
	res, err := m.run(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !res.Succeeded() {
		return "", fmt.Errorf("%w: %s", ErrNotARepository, dir)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// gitlinkPaths lists the submodule paths a worktree's index records. Reading the
// index rather than the base commit means removal finds what is actually there,
// including a worktree attached in a later process.
func (m *Manager) gitlinkPaths(ctx context.Context, w *Worktree) ([]string, error) {
	res, err := m.run(ctx, w.Path, nil, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, err
	}
	if !res.Succeeded() {
		return nil, fmt.Errorf("list submodules in %s: %s", w.Path, firstLine(res.Stderr))
	}

	var paths []string
	for _, entry := range strings.Split(res.Stdout, "\x00") {
		// "<mode> <object> <stage>\t<path>"
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// submodulePath joins a git-recorded submodule path onto a root and refuses
// anything that would land outside it.
//
// Git will not put ".." in a tree path, so this should be unreachable; it is
// here because the value comes from repository content, and "the format makes it
// impossible" is a weaker guarantee than a check.
func submodulePath(root, sub string) (string, error) {
	clean := filepath.Clean(sub)
	if sub == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: submodule path %q", ErrInvalidName, sub)
	}
	joined := filepath.Join(root, clean)
	if !isInside(root, joined) {
		return "", fmt.Errorf("%w: submodule path %q escapes %s", ErrInvalidName, sub, root)
	}
	return joined, nil
}
