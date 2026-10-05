package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// A repository with no commits yet has nothing to branch from: a git worktree
// needs a commit, and `git init` alone does not make one. Creating a task
// there used to fail with the ref resolution's "revision does not exist: main",
// which names a branch the person has not created and does not say what to do.
//
// The message must say what is wrong and what to do, on both surfaces, and
// nothing may be written for the task.

// unbornRepo is a real `git init` with no commits.
func unbornRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := filepath.Join(t.TempDir(), "unborn")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

func TestCreateTaskRefusesARepositoryWithNoCommits(t *testing.T) {
	h := newHarness(t, nil)
	repo := unbornRepo(t)

	_, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
		RepoPath: repo,
		Title:    "The first change",
		Verification: []task.VerificationStep{
			{Command: "true"},
		},
	})
	if err == nil {
		t.Fatal("CreateTask succeeded on a repository with no commits")
	}
	message := err.Error()
	for _, want := range []string{repo, "no commits", "commit"} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to mention %q", message, want)
		}
	}
	if strings.Contains(message, "revision does not exist") {
		t.Errorf("error = %q, want the no-commits reason rather than a ref resolution failure", message)
	}

	// Nothing was written for it: no task, and no project row either — a task
	// that cannot exist should not register the repository.
	if tasks, err := h.store.ListTasks(h.ctx, store.TaskFilter{}); err != nil {
		t.Fatal(err)
	} else if len(tasks) != 0 {
		t.Errorf("%d tasks exist after the refusal, want none", len(tasks))
	}
	if _, err := h.store.GetProjectByPath(h.ctx, repo); err == nil {
		t.Error("the repository was registered as a project although no task could be created")
	}
}

// The same refusal, worded for a person at the CLI, and the same for MCP: the
// message comes from one place, so both surfaces say it.
func TestUnbornRepositoryMessageAtTheCLIAndMCP(t *testing.T) {
	h := newHarness(t, nil)
	repo := unbornRepo(t)

	_, _, err := h.runCLI(t, "task", "create", "--repo", repo, "--title", "The first change", "--verify", "true")
	if err == nil {
		t.Fatal("aidev task create succeeded on a repository with no commits")
	}
	if !strings.Contains(err.Error(), "no commits") {
		t.Errorf("CLI error = %q, want the no-commits reason", err)
	}

	m := newMCPHarness(t)
	// Registered first: the registration gate (research D3) answers before
	// anything else, and would mask the reason this test is about.
	m.registerProject(repo)
	msg := m.callExpectingError(t, "aidev_create_task", map[string]any{
		"repo_path":    repo,
		"title":        "The first change",
		"verification": []string{"true"},
	})
	if !strings.Contains(msg, "no commits") {
		t.Errorf("MCP error = %q, want the no-commits reason", msg)
	}
}

// An explicit base_ref that does not resolve is a different mistake and keeps
// its own message: the repository has commits, the ref is a typo.
func TestAMissingBaseRefStillSaysTheRevisionDoesNotExist(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
		RepoPath:     h.repoPath,
		BaseRef:      "no-such-branch",
		Title:        "A change",
		Verification: []task.VerificationStep{{Command: "true"}},
	})
	if err == nil {
		t.Fatal("CreateTask succeeded with a base_ref that does not exist")
	}
	if !strings.Contains(err.Error(), "no-such-branch") {
		t.Errorf("error = %q, want it to name the ref", err)
	}
}
