package integration

import (
	"errors"
	"strings"
	"testing"

	"aidev/internal/config"
	"aidev/internal/store"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// Research D3: the MCP server used to register any repository a planner named,
// so a guessed or mistyped path became an agent running in the wrong
// repository. Now it creates tasks only for registered repositories unless
// mcp.auto_register_projects says otherwise, and a person registers one with
// `aidev project add`.

func createTaskArgs(repo string) map[string]any {
	return map[string]any{
		"repo_path":    repo,
		"title":        "Create the marker file",
		"description":  "Create marker.txt containing the word done",
		"verification": []string{"test -f marker.txt"},
	}
}

func TestMCPRefusesAnUnregisteredRepository(t *testing.T) {
	m := newMCPHarness(t)
	other := newTestRepo(t) // never registered

	msg := m.callExpectingError(t, "aidev_create_task", createTaskArgs(other))
	for _, want := range []string{"not registered", "aidev project add", other} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to mention %q", msg, want)
		}
	}

	// Refused before anything was written: no project row, no task.
	if _, err := m.store.GetProjectByPath(m.ctx, other); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetProjectByPath after a refusal = %v, want ErrNotFound: the refusal registered it anyway", err)
	}
	tasks, err := m.store.ListTasks(m.ctx, store.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("%d task(s) exist after a refused create, want 0", len(tasks))
	}
}

// Opting in restores registration on demand.
func TestMCPAutoRegisterProjectsRegistersOnDemand(t *testing.T) {
	m := newMCPHarnessWith(t, func(c *config.Config) { c.MCPAutoRegisterProjects = true })
	other := newTestRepo(t)

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", createTaskArgs(other), &created)
	if created.Task["ref"] == "" {
		t.Fatal("no task was created")
	}
	if _, err := m.store.GetProjectByPath(m.ctx, other); err != nil {
		t.Errorf("the repository was not registered on demand: %v", err)
	}
}

// The CLI keeps registering on demand — a person typed that path — and the
// orchestrator honours the flag for any caller, not only MCP.
func TestCreateTaskRequireRegisteredProjectIsPerCaller(t *testing.T) {
	h := newHarness(t, nil)

	_, err := h.orchestrator.CreateTask(h.ctx, worker.CreateTaskInput{
		RepoPath:                 h.repoPath,
		RequireRegisteredProject: true,
		Title:                    "Create the marker file",
		Description:              "Create marker.txt",
		Verification:             []task.VerificationStep{{Command: "test", Args: []string{"-f", "marker.txt"}}},
	})
	if !errors.Is(err, worker.ErrProjectNotRegistered) {
		t.Fatalf("CreateTask with RequireRegisteredProject on an unknown repository = %v, want ErrProjectNotRegistered", err)
	}

	h.createTask(nil) // the default: registers on demand
	if _, err := h.store.GetProjectByPath(h.ctx, h.repoPath); err != nil {
		t.Errorf("a CLI-style create did not register the repository: %v", err)
	}
}

func TestProjectAddRegistersOnceAndUnlocksMCP(t *testing.T) {
	m := newMCPHarness(t)
	other := newTestRepo(t)

	stdout, stderr, err := m.runCLI(t, "project", "add", other)
	if err != nil {
		t.Fatalf("project add: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "registered "+other) {
		t.Errorf("project add output = %q, want it to confirm the registration", stdout)
	}

	// Idempotent, and says so rather than pretending to register again.
	stdout, stderr, err = m.runCLI(t, "project", "add", other)
	if err != nil {
		t.Fatalf("second project add: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "already registered") {
		t.Errorf("second project add output = %q, want it to say the repository was already known", stdout)
	}

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", createTaskArgs(other), &created)
	if created.Task["ref"] == "" {
		t.Error("MCP still refused the repository after project add")
	}

	stdout, _, err = m.runCLI(t, "project", "list")
	if err != nil || !strings.Contains(stdout, other) {
		t.Errorf("project list = %q (%v), want it to include %s", stdout, err, other)
	}
}

func TestProjectAddRefusesAPathOutsideAnyRepository(t *testing.T) {
	h := newHarness(t, nil)
	if _, stderr, err := h.runCLI(t, "project", "add", t.TempDir()); err == nil {
		t.Errorf("project add accepted a directory that is not a git repository; stderr: %s", stderr)
	}
}
