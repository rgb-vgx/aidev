// Package e2e exercises the whole orchestration pipeline against the real
// OpenCode binary and a real PostgreSQL.
//
// It is opt-in. `make check` must never depend on an agent, a model, or a
// network, so these tests skip unless both TEST_DATABASE_URL and
// AIDEV_TEST_OPENCODE are set. The deterministic coverage of the same pipeline
// lives in tests/integration with a scripted agent; this exists to prove the
// integration with the actual tool, which no fake can do.
package e2e

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/git"
	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/worker"
	"aidev/migrations"
)

// freeModel needs no credentials and reports zero cost, so this test can run on a
// developer machine without an API key (docs/research.md §2.10).
const freeModel = "opencode/nemotron-3.5-lightning-free"

// e2eTimeout is generous because OpenCode's first run against a repository it has
// not seen can take minutes before producing any output (docs/research.md §2.9).
const e2eTimeout = 10 * time.Minute

func setup(t *testing.T) (*worker.Orchestrator, *store.Store, context.Context, string) {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("AIDEV_TEST_OPENCODE") == "" {
		t.Skip("set AIDEV_TEST_OPENCODE=1 to run the end-to-end test against the real opencode")
	}
	if _, err := exec.LookPath(opencodeCommand()); err != nil {
		t.Skipf("opencode is not installed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	t.Cleanup(cancel)

	db, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)

	loaded, err := store.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := db.Migrate(ctx, loaded); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	workspace := filepath.Join(t.TempDir(), "workspaces")
	gm, err := git.NewManager(workspace)
	if err != nil {
		t.Fatalf("git.NewManager: %v", err)
	}

	model := os.Getenv("OPENCODE_MODEL")
	if model == "" {
		model = freeModel
	}
	cfg := config.Config{
		WorkspaceRoot:              workspace,
		DefaultTaskTimeout:         8 * time.Minute,
		DefaultVerificationTimeout: 2 * time.Minute,
		OpenCodeCommand:            opencodeCommand(),
		OpenCodeModel:              model,
		OpenCodeAgent:              "build",
		MaxOutputBytes:             1 << 20,
		WorktreeCleanup:            config.CleanupOnSuccess,
	}

	backend := agent.NewOpenCode(cfg.OpenCodeCommand, cfg.OpenCodeModel)
	logger := logging.Discard()
	if testing.Verbose() {
		logger = logging.NewTo(os.Stderr, slog.LevelInfo)
	}

	return worker.New(db, gm, backend, cfg, logger), db, ctx, newRepo(t)
}

func opencodeCommand() string {
	if c := strings.TrimSpace(os.Getenv("OPENCODE_COMMAND")); c != "" {
		return c
	}
	return "opencode"
}

// newRepo builds a small Go module whose test fails until the task is done, so
// verification genuinely depends on the agent's work.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	write(t, filepath.Join(dir, "go.mod"), "module e2eprobe\n\ngo 1.25\n")
	write(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	// The test references Greet, which does not exist yet: `go test ./...` fails
	// to compile until the agent creates it.
	write(t, filepath.Join(dir, "greet_test.go"), `package main

import "testing"

func TestGreet(t *testing.T) {
	if got := Greet("world"); got != "Hello, world" {
		t.Errorf("Greet(\"world\") = %q, want %q", got, "Hello, world")
	}
}
`)

	run(t, dir, "git", "init", "-q", "-b", "main")
	run(t, dir, "git", "config", "user.email", "e2e@example.com")
	run(t, dir, "git", "config", "user.name", "E2E")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-qm", "initial commit with a failing test")
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// TestPipelineAgainstRealOpenCode is the scenario the MVP exists to support:
// create a task, let the real agent implement it in an isolated worktree, verify
// independently, and read the persisted result back.
func TestPipelineAgainstRealOpenCode(t *testing.T) {
	orchestrator, db, ctx, repoPath := setup(t)

	created, err := orchestrator.CreateTask(ctx, worker.CreateTaskInput{
		RepoPath:           repoPath,
		Title:              "Add a Greet function",
		Description:        "Create a file greet.go in the repository root, package main, with a function Greet(name string) string that returns \"Hello, \" followed by name. There is already a test for it in greet_test.go.",
		AcceptanceCriteria: "go test ./... passes",
		Verification: []task.VerificationStep{
			{Command: "go", Args: []string{"test", "./..."}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	t.Logf("created %s", created.Ref)

	// The repository's own test must be failing to begin with, or the task would
	// prove nothing.
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = repoPath
	if err := cmd.Run(); err == nil {
		t.Fatal("the repository's test already passes; the task would prove nothing")
	}

	outcome, err := orchestrator.RunTask(ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	t.Logf("outcome: %s", outcome.Message)

	if outcome.WorkerRun != nil {
		t.Logf("agent: status=%s tools=%d session=%s summary=%q",
			outcome.WorkerRun.Status, outcome.WorkerRun.ChangedFiles,
			outcome.WorkerRun.SessionID, truncate(outcome.WorkerRun.Summary, 120))
	}
	if outcome.Verification != nil {
		t.Logf("verification: %s", outcome.Verification.Summary())
		for _, vr := range outcome.Verification.Runs {
			t.Logf("  %s -> %s", vr.Command, vr.Status)
		}
	}

	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED. %s", outcome.Task.Status, outcome.Message)
	}

	// The result is durable in git, on the task's own branch, and the main
	// working tree was never touched.
	branch := "aidev/" + created.Ref
	files := run(t, repoPath, "git", "ls-tree", "--name-only", "-r", branch)
	if !strings.Contains(files, "greet.go") {
		t.Errorf("branch %s does not contain greet.go:\n%s", branch, files)
	}
	if status := strings.TrimSpace(run(t, repoPath, "git", "status", "--porcelain")); status != "" {
		t.Errorf("the repository's working tree was modified:\n%s", status)
	}
	if _, err := os.Stat(filepath.Join(repoPath, "greet.go")); !os.IsNotExist(err) {
		t.Error("the agent's file appeared in the main working tree")
	}

	// And the result is readable back out of the database, which is the part a
	// planner depends on.
	reloaded, err := db.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if reloaded.Status != task.StatusSucceeded {
		t.Errorf("persisted status = %s, want SUCCEEDED", reloaded.Status)
	}

	runs, err := db.ListVerificationRuns(ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatalf("ListVerificationRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != task.VerificationPassed {
		t.Fatalf("verification rows = %+v, want one PASSED", runs)
	}
	if runs[0].ExitCode == nil || *runs[0].ExitCode != 0 {
		t.Errorf("verification exit code = %v, want 0 from the real go test", runs[0].ExitCode)
	}
	if !strings.Contains(runs[0].Stdout, "ok") && !strings.Contains(runs[0].Stdout, "PASS") {
		t.Errorf("verification stdout does not look like go test output:\n%s", runs[0].Stdout)
	}

	workerRuns, err := db.ListWorkerRuns(ctx, outcome.Attempt.ID)
	if err != nil {
		t.Fatalf("ListWorkerRuns: %v", err)
	}
	if len(workerRuns) != 1 {
		t.Fatalf("got %d worker runs, want 1", len(workerRuns))
	}
	wr := workerRuns[0]
	if wr.SessionID == "" {
		t.Error("no OpenCode session id was recorded")
	}
	if wr.ChangedFiles == 0 {
		t.Error("no changed files were recorded, so the diff was not collected")
	}
	if !strings.Contains(wr.Diff, "greet.go") {
		t.Errorf("the collected diff does not mention greet.go:\n%s", truncate(wr.Diff, 400))
	}

	events, err := db.ListEvents(ctx, store.EventFilter{TaskID: created.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var names []string
	for _, e := range events {
		names = append(names, e.Type.String())
	}
	for _, want := range []string{
		"task.created", "task.started", "task.worktree_created",
		"task.worker_started", "task.worker_completed",
		"task.verification_started", "task.verification_completed", "task.succeeded",
	} {
		if !strings.Contains(strings.Join(names, " "), want) {
			t.Errorf("history is missing %s; got %v", want, names)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
