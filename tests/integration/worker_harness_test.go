package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/git"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// harness is a complete, isolated aidev: a real PostgreSQL, a real git repository,
// a real worktree manager and verification runner, with a scripted agent in place
// of OpenCode. Only the agent is faked, so everything the orchestrator does to the
// database and the filesystem is exercised for real.
type harness struct {
	t            *testing.T
	ctx          context.Context
	store        *store.Store
	git          *git.Manager
	backend      *agent.Fake
	orchestrator *worker.Orchestrator
	repoPath     string
	workspace    string
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	db, ctx := openStore(t)
	repoPath := newTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspaces")

	gm, err := git.NewManager(workspace)
	if err != nil {
		t.Fatalf("git.NewManager: %v", err)
	}

	cfg := config.Config{
		WorkspaceRoot:              workspace,
		DefaultTaskTimeout:         30 * time.Second,
		DefaultVerificationTimeout: 30 * time.Second,
		OpenCodeAgent:              "build",
		MaxOutputBytes:             64 * 1024,
		WorktreeCleanup:            config.CleanupOnSuccess,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	// The real wiring builds the backend from configuration (internal/cli/app.go),
	// and the backend is what knows which model it falls back to, so the harness
	// mirrors that rather than letting the worker guess.
	backend := &agent.Fake{Model: cfg.OpenCodeModel}
	return &harness{
		t:            t,
		ctx:          ctx,
		store:        db,
		git:          gm,
		backend:      backend,
		orchestrator: worker.New(db, gm, backend, cfg, logging.Discard()),
		repoPath:     repoPath,
		workspace:    workspace,
	}
}

// newTestRepo creates a git repository with one commit and a passing test.
func newTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "init")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// createTask makes a task whose verification passes only when the agent has
// created marker.txt, so that "did the work actually happen" is decidable.
func (h *harness) createTask(mutate func(*worker.CreateTaskInput)) task.Task {
	h.t.Helper()

	in := worker.CreateTaskInput{
		RepoPath:           h.repoPath,
		Title:              "Create the marker file",
		Description:        "Create marker.txt containing the word done",
		AcceptanceCriteria: "marker.txt exists and contains done",
		Verification: []task.VerificationStep{
			{Command: "test", Args: []string{"-f", "marker.txt"}},
		},
	}
	if mutate != nil {
		mutate(&in)
	}

	created, err := h.orchestrator.CreateTask(h.ctx, in)
	if err != nil {
		h.t.Fatalf("CreateTask: %v", err)
	}
	return created
}

// doTheWork is the agent behaviour that satisfies the default task.
func doTheWork(_ context.Context, req agent.Request) error {
	return os.WriteFile(filepath.Join(req.WorkingDir, "marker.txt"), []byte("done\n"), 0o600)
}

// eventTypes returns a task's history as a list of type names.
func (h *harness) eventTypes(taskID uuid.UUID) []string {
	h.t.Helper()
	events, err := h.store.ListEvents(h.ctx, store.EventFilter{TaskID: taskID})
	if err != nil {
		h.t.Fatalf("ListEvents: %v", err)
	}
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.Type.String())
	}
	return names
}

// repoIsClean reports whether the repository's own working tree was left alone.
func (h *harness) repoIsClean() bool {
	return strings.TrimSpace(gitRun(h.t, h.repoPath, "status", "--porcelain")) == ""
}

func (h *harness) branchFiles(branch string) string {
	return gitRun(h.t, h.repoPath, "ls-tree", "--name-only", "-r", branch)
}

func (h *harness) branchExists(branch string) bool {
	cmd := exec.Command("git", "-C", h.repoPath, "rev-parse", "--verify", branch)
	return cmd.Run() == nil
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// indexOf reports the position of the first occurrence, or -1.
func indexOf(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}
