package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxNameLength bounds a worktree directory name so that the resulting path
// stays comfortably inside filesystem limits.
const MaxNameLength = 100

// validateName rejects anything aidev will not put into a filesystem path.
//
// The allowed set is deliberately narrow rather than a blocklist of dangerous
// sequences: a name reaches here from a task reference or a user-supplied value,
// and an allowlist cannot be defeated by an escaping trick nobody thought of.
func validateName(name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return fmt.Errorf("%w: name is empty", ErrInvalidName)
	case len(trimmed) > MaxNameLength:
		return fmt.Errorf("%w: %q is %d characters, limit is %d", ErrInvalidName, trimmed, len(trimmed), MaxNameLength)
	case trimmed == "." || trimmed == "..":
		return fmt.Errorf("%w: %q", ErrInvalidName, trimmed)
	case strings.HasPrefix(trimmed, "-"):
		// A leading dash would be read as a flag by any command taking the path.
		return fmt.Errorf("%w: %q must not start with a dash", ErrInvalidName, trimmed)
	case strings.HasPrefix(trimmed, "."):
		return fmt.Errorf("%w: %q must not start with a dot", ErrInvalidName, trimmed)
	}

	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return fmt.Errorf("%w: %q contains %q; only letters, digits, dot, underscore and dash are allowed",
				ErrInvalidName, trimmed, string(r))
		}
	}
	return nil
}

// validateBranchName applies the subset of git's ref rules that aidev can check
// cheaply. Git enforces the rest; this exists to give a clear message for the
// mistakes that are likely, rather than to reimplement check-ref-format.
func validateBranchName(branch string) error {
	trimmed := strings.TrimSpace(branch)
	switch {
	case trimmed == "":
		return fmt.Errorf("invalid branch name: empty")
	case strings.HasPrefix(trimmed, "-"):
		return fmt.Errorf("invalid branch name %q: must not start with a dash", trimmed)
	case strings.HasPrefix(trimmed, "/") || strings.HasSuffix(trimmed, "/"):
		return fmt.Errorf("invalid branch name %q: must not start or end with a slash", trimmed)
	case strings.Contains(trimmed, ".."):
		return fmt.Errorf("invalid branch name %q: must not contain %q", trimmed, "..")
	case strings.Contains(trimmed, "//"):
		return fmt.Errorf("invalid branch name %q: must not contain %q", trimmed, "//")
	case strings.HasSuffix(trimmed, ".lock"):
		return fmt.Errorf("invalid branch name %q: must not end with .lock", trimmed)
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid branch name %q: contains a control character", trimmed)
		}
		if strings.ContainsRune(" ~^:?*[\\", r) {
			return fmt.Errorf("invalid branch name %q: must not contain %q", trimmed, string(r))
		}
	}
	return nil
}

// resolveWorktreePath turns a name into an absolute path under WorkspaceRoot and
// verifies it cannot escape.
func (m *Manager) resolveWorktreePath(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	if m.WorkspaceRoot == "" {
		return "", fmt.Errorf("workspace root is not configured")
	}
	path := filepath.Join(m.WorkspaceRoot, strings.TrimSpace(name))
	if err := m.withinWorkspace(path); err != nil {
		return "", err
	}
	return path, nil
}

// withinWorkspace verifies that path is inside WorkspaceRoot.
//
// Comparison happens after resolving symlinks on whatever part of each path
// already exists, because a symlink inside the workspace root could otherwise
// point anywhere on the filesystem and a textual prefix check would be satisfied
// by a path that physically is not there.
func (m *Manager) withinWorkspace(path string) error {
	root := resolveExisting(filepath.Clean(m.WorkspaceRoot))
	candidate := resolveExisting(filepath.Clean(path))

	if candidate == root {
		return fmt.Errorf("%w: %s is the workspace root itself", ErrOutsideWorkspace, path)
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrOutsideWorkspace, path)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s resolves outside %s", ErrOutsideWorkspace, path, root)
	}
	return nil
}

// validateIsolation refuses a worktree path that would sit inside the repository
// it is meant to isolate work from, or that would contain the repository.
//
// Without this, a workspace root configured inside the repository would put the
// agent's checkout in the main working tree — the exact situation worktrees exist
// to prevent, and one that workspace_root containment alone would not catch.
func validateIsolation(repoPath, worktreePath string) error {
	repo := resolveExisting(filepath.Clean(repoPath))
	wt := resolveExisting(filepath.Clean(worktreePath))

	if repo == wt {
		return fmt.Errorf("%w: %s is the repository itself", ErrInsideRepository, worktreePath)
	}
	if isInside(repo, wt) {
		return fmt.Errorf("%w: %s is inside %s", ErrInsideRepository, worktreePath, repoPath)
	}
	if isInside(wt, repo) {
		return fmt.Errorf("%w: %s would contain the repository %s", ErrInsideRepository, worktreePath, repoPath)
	}
	return nil
}

// isInside reports whether child is under parent.
func isInside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting resolves symlinks on the longest existing prefix of path and
// re-appends the remainder, so that a path whose leaf does not exist yet can
// still be compared physically.
func resolveExisting(path string) string {
	remainder := ""
	current := path

	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the root without finding anything that exists.
			return path
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// ensureWorkspaceRoot creates the workspace root if it does not exist.
func (m *Manager) ensureWorkspaceRoot() error {
	if err := os.MkdirAll(m.WorkspaceRoot, 0o755); err != nil {
		return fmt.Errorf("create workspace root %s: %w", m.WorkspaceRoot, err)
	}
	return nil
}
