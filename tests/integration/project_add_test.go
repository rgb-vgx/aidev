package integration

import (
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/task"
)

// `aidev project add` chooses the verification default from the repository
// and says what it chose: a plain repository verifies in a fresh checkout,
// where files git ignores cannot make the checks pass.
func TestProjectAddPrintsCleanForAPlainRepository(t *testing.T) {
	h := newHarness(t, nil)

	stdout, _, err := h.runCLI(t, "project", "add", h.repoPath)
	if err != nil {
		t.Fatalf("project add: %v", err)
	}
	for _, want := range []string{"registered", "fresh checkout", "files git ignores"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("project add output does not mention %q:\n%s", want, stdout)
		}
	}

	repo, err := h.git.OpenRepository(h.ctx, h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := h.store.GetProjectByPath(h.ctx, repo.Path)
	if err != nil {
		t.Fatalf("GetProjectByPath: %v", err)
	}
	if project.VerificationMode != task.VerificationClean {
		t.Errorf("stored mode = %q, want clean", project.VerificationMode)
	}
}

// A repository pinning submodules cannot verify clean — the detached
// checkout would hold empty directories — so registering one keeps in_place
// and says why, naming the command that overrides it.
func TestProjectAddPrintsInPlaceForASubmoduleRepository(t *testing.T) {
	h := newHarness(t, nil)

	child := t.TempDir()
	gitRun(t, child, "init", "-q", "-b", "main")
	gitRun(t, child, "config", "user.email", "test@example.com")
	gitRun(t, child, "config", "user.name", "Test")
	writeFile(t, filepath.Join(child, "lib.txt"), "pinned\n")
	gitRun(t, child, "add", "-A")
	gitRun(t, child, "commit", "-qm", "init")
	gitRun(t, h.repoPath, "-c", "protocol.file.allow=always",
		"submodule", "add", "--", child, "vendor/child")
	gitRun(t, h.repoPath, "commit", "-qm", "pin the submodule")

	stdout, _, err := h.runCLI(t, "project", "add", h.repoPath)
	if err != nil {
		t.Fatalf("project add: %v", err)
	}
	for _, want := range []string{"pins submodules", "agent's worktree", "project verify-mode"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("project add output does not mention %q:\n%s", want, stdout)
		}
	}

	repo, err := h.git.OpenRepository(h.ctx, h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := h.store.GetProjectByPath(h.ctx, repo.Path)
	if err != nil {
		t.Fatalf("GetProjectByPath: %v", err)
	}
	if project.VerificationMode != task.VerificationInPlace {
		t.Errorf("stored mode = %q, want in_place for a repository pinning submodules", project.VerificationMode)
	}
}
