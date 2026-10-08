package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `aidev project read-dirs` names directories outside the repository that the
// agent may read: an installed program's files, say, which a task needs to
// look at (TASK-000091 needed /opt/kingsoft). It is the operator's decision,
// like approval and verify-mode, so it exists only at the CLI. Everything
// else outside the worktree stays denied — and a denial no longer ends the
// agent's session (research §7m).
//
// The name says read, not read-only: OpenCode's edit rule did not stop a
// write in a measured run, so what keeps such a directory unwritten is the
// filesystem. aidev warns when the person can write to it, because then the
// agent can too.

func TestProjectReadDirsCanBeSetShownAndCleared(t *testing.T) {
	h := newHarness(t, nil)
	h.createTask(nil) // registers the repository
	dir := t.TempDir()

	out, stderr, err := h.runCLI(t, "project", "read-dirs", dir, "--repo", h.repoPath)
	if err != nil {
		t.Fatalf("project read-dirs: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("output does not name the directory:\n%s", out)
	}
	// A directory this user can write to is accepted with a warning.
	if !strings.Contains(out+stderr, "can write") {
		t.Errorf("no warning that the agent can write to %s:\n%s%s", dir, out, stderr)
	}

	project, err := h.store.GetProjectByPath(h.ctx, h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.ReadDirs) != 1 || project.ReadDirs[0] != dir {
		t.Errorf("stored read dirs = %q, want [%s]", project.ReadDirs, dir)
	}

	if out, _, err = h.runCLI(t, "project", "read-dirs", "--repo", h.repoPath); err != nil || !strings.Contains(out, dir) {
		t.Errorf("showing read dirs = %q, %v; want it listed", out, err)
	}
	if _, _, err = h.runCLI(t, "project", "read-dirs", "--clear", "--repo", h.repoPath); err != nil {
		t.Fatalf("--clear: %v", err)
	}
	project, _ = h.store.GetProjectByPath(h.ctx, h.repoPath)
	if len(project.ReadDirs) != 0 {
		t.Errorf("read dirs after --clear = %q, want none", project.ReadDirs)
	}
}

// Directories that would undo the worktree's purpose are refused: the root,
// the repository's own checkout (the agent would reach your working tree),
// and paths that are not directories.
func TestProjectReadDirsRefusesWhatWouldDefeatTheWorktree(t *testing.T) {
	h := newHarness(t, nil)
	h.createTask(nil)
	if err := os.MkdirAll(filepath.Join(h.repoPath, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"/":                              "root",
		h.repoPath:                       "repository",
		filepath.Dir(h.repoPath):         "repository",
		filepath.Join(h.repoPath, "sub"): "repository",
		file:                             "not a directory",
		"/no/such/dir":                   "not a directory",
	}
	// The directory holding the configuration is refused too; the harness
	// writes its configuration where a test cannot name it, so that case is
	// in internal/cli's TestCheckReadDir.
	for dir, why := range cases {
		_, stderr, err := h.runCLI(t, "project", "read-dirs", dir, "--repo", h.repoPath)
		if err == nil {
			t.Errorf("%s (%s) was accepted", dir, why)
			continue
		}
		if !strings.Contains(err.Error()+stderr, why) {
			t.Errorf("%s: error %q does not say %q", dir, err, why)
		}
	}
	project, _ := h.store.GetProjectByPath(h.ctx, h.repoPath)
	if len(project.ReadDirs) != 0 {
		t.Errorf("a refused directory was stored: %q", project.ReadDirs)
	}
}

// What the project allows reaches the agent, and the history says so.
func TestTheAgentIsGivenTheProjectsReadDirs(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	created := h.createTask(nil)
	dir := t.TempDir()
	if _, stderr, err := h.runCLI(t, "project", "read-dirs", dir, "--repo", h.repoPath); err != nil {
		t.Fatalf("project read-dirs: %v\n%s", err, stderr)
	}

	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	calls := h.backend.Calls()
	if len(calls) != 1 || len(calls[0].ReadDirs) != 1 || calls[0].ReadDirs[0] != dir {
		t.Fatalf("agent requests = %+v, want one with ReadDirs [%s]", calls, dir)
	}
	payload := h.eventPayload(created.ID, "task.worker_started")
	dirs, _ := payload["read_dirs"].([]any)
	if len(dirs) != 1 || dirs[0] != dir {
		t.Errorf("worker_started read_dirs = %v, want [%s]", payload["read_dirs"], dir)
	}
}
