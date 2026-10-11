package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A repository pinning a submodule must read as pinned: clean verification
// refuses such a tree, so the default for it must stay in_place.
func TestPinsSubmodulesDetectsAPinnedRepository(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)
	repo := openParent(t, m, f)

	pinned, err := m.PinsSubmodules(ctx, repo)
	if err != nil {
		t.Fatalf("PinsSubmodules: %v", err)
	}
	if !pinned {
		t.Error("PinsSubmodules = false for a repository pinning a submodule, want true")
	}
}

// A plain repository pins nothing, so its new projects default to clean.
func TestPinsSubmodulesClearForAPlainRepository(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)
	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	pinned, err := m.PinsSubmodules(ctx, repo)
	if err != nil {
		t.Fatalf("PinsSubmodules: %v", err)
	}
	if pinned {
		t.Error("PinsSubmodules = true for a repository with no submodules, want false")
	}
}

// A .gitmodules file alone pins submodules even when the index records no
// gitlink yet: the checkout is one `git submodule add` away from refusing
// clean verification, so the default must already avoid it.
func TestPinsSubmodulesSeesAGitmodulesFileAlone(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repoPath := newRepo(t)
	write(t, filepath.Join(repoPath, ".gitmodules"), "[submodule \"vendor/child\"]\n\tpath = vendor/child\n")
	repo, err := m.OpenRepository(ctx, repoPath)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	pinned, err := m.PinsSubmodules(ctx, repo)
	if err != nil {
		t.Fatalf("PinsSubmodules: %v", err)
	}
	if !pinned {
		t.Error("PinsSubmodules = false with a .gitmodules file present, want true")
	}
}

// A gitlink without its .gitmodules file is still pinned: the commit the
// tree carries is what clean verification refuses, not the file.
func TestPinsSubmodulesSeesAGitlinkWithoutGitmodules(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	f := newSubmoduleRepo(t)
	if err := os.Remove(filepath.Join(f.parent, ".gitmodules")); err != nil {
		t.Fatal(err)
	}
	repo := openParent(t, m, f)

	pinned, err := m.PinsSubmodules(ctx, repo)
	if err != nil {
		t.Fatalf("PinsSubmodules: %v", err)
	}
	if !pinned {
		t.Error("PinsSubmodules = false with a gitlink in the index, want true")
	}
}
