package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// `--coverage` asks aidev, once verification has passed, which of the Go
// lines the agent changed the tests actually ran. Every dogfood round found
// defects in code no specification exercised; this makes such code visible
// in the result. It reports only: the task's outcome never depends on it.

// commitGoModule puts a small module in the test repository: Add, tested.
func commitGoModule(t *testing.T, h *harness) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	writeUnder(t, h.repoPath, "go.mod", "module example.com/demo\n\ngo 1.21\n")
	writeUnder(t, h.repoPath, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	writeUnder(t, h.repoPath, "calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n")
	gitRun(t, h.repoPath, "add", "go.mod", "calc")
	gitRun(t, h.repoPath, "commit", "-qm", "a tested Add")
}

// addUntestedSub is an agent that adds Sub with no test for it.
func addUntestedSub(_ context.Context, req agent.Request) error {
	p := filepath.Join(req.WorkingDir, "calc", "calc.go")
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(raw, []byte("\nfunc Sub(a, b int) int {\n\treturn a - b\n}\n")...), 0o644)
}

func goTest() []task.VerificationStep {
	return []task.VerificationStep{{Command: "go", Args: []string{"test", "./..."}}}
}

func TestCoverageNamesChangedLinesNoTestRan(t *testing.T) {
	h := newHarness(t, nil)
	commitGoModule(t, h)
	h.backend.Work = addUntestedSub

	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = goTest()
		in.Coverage = boolPtr(true)
	})
	out, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	// It reports; it does not decide.
	if out.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s (%s), want SUCCEEDED: coverage never fails a task", out.Task.Status, out.Message)
	}
	if !contains(h.eventTypes(created.ID), "task.coverage_reported") {
		t.Fatalf("history = %v, want task.coverage_reported", h.eventTypes(created.ID))
	}
	payload := h.eventPayload(created.ID, "task.coverage_reported")
	if payload["not_run"].(float64) < 1 {
		t.Errorf("coverage = %v, want Sub's lines reported as not run", payload)
	}

	text, _, err := h.runCLI(t, "task", "result", created.Ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"not run by the tests", "calc/calc.go"} {
		if !strings.Contains(text, want) {
			t.Errorf("task result lacks %q:\n%s", want, text)
		}
	}
	raw, _, err := h.runCLI(t, "task", "result", created.Ref, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Coverage *struct {
			Changed int `json:"changed"`
			NotRun  int `json:"not_run"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Coverage == nil || result.Coverage.NotRun < 1 || result.Coverage.Changed <= result.Coverage.NotRun-1 {
		t.Errorf("result.coverage = %+v, want the lines reported", result.Coverage)
	}
	got, _ := h.store.GetTask(h.ctx, created.ID)
	if !got.Coverage {
		t.Error("the task did not keep its coverage setting")
	}
}

// Off by default: no extra test run, nothing in the history.
func TestCoverageIsOffByDefault(t *testing.T) {
	h := newHarness(t, nil)
	commitGoModule(t, h)
	h.backend.Work = addUntestedSub
	created := h.createTask(func(in *worker.CreateTaskInput) { in.Verification = goTest() })
	if out, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v", out.Task.Status, err)
	}
	if contains(h.eventTypes(created.ID), "task.coverage_reported") {
		t.Error("coverage was measured on a task that did not ask for it")
	}
}

// A measurement that cannot be made is a note, never a failure.
func TestCoverageThatCannotBeMeasuredIsANote(t *testing.T) {
	h := newHarness(t, nil)
	commitGoModule(t, h)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := addUntestedSub(ctx, req); err != nil {
			return err
		}
		// Verification here does not run go at all, so a test file that does
		// not compile passes it and still breaks coverage's own go test.
		return os.WriteFile(filepath.Join(req.WorkingDir, "calc", "broken_test.go"), []byte("package calc\n\nfunc broken( {\n"), 0o644)
	}
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "true"}}
		in.Coverage = boolPtr(true)
	})
	out, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v (%s); a failed measurement must not fail the task", out.Task.Status, err, out.Message)
	}
	payload := h.eventPayload(created.ID, "task.coverage_reported")
	if s, _ := payload["error"].(string); s == "" {
		t.Errorf("coverage = %v, want the reason it could not be measured", payload)
	}
}

// A planner asks for it through MCP, and reads it from the result.
func TestMCPCoverage(t *testing.T) {
	m := newMCPHarness(t)
	commitGoModule(t, m.harness)
	m.backend.Work = addUntestedSub
	var created struct {
		Task struct {
			Ref      string `json:"ref"`
			Coverage bool   `json:"coverage"`
		} `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Add Sub",
		"verification": []string{"go test ./..."},
		"coverage":     true,
	}, &created)
	if !created.Task.Coverage {
		t.Fatalf("created task = %+v, want coverage on", created.Task)
	}
	if out, err := m.orchestrator.RunTask(m.ctx, created.Task.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v", out.Task.Status, err)
	}
	var result struct {
		Result struct {
			Coverage *struct {
				NotRun int `json:"not_run"`
				Files  []struct {
					Path string `json:"path"`
				} `json:"files"`
			} `json:"coverage"`
		} `json:"result"`
	}
	m.call(t, "aidev_get_task_result", map[string]any{"task": created.Task.Ref}, &result)
	c := result.Result.Coverage
	if c == nil || c.NotRun < 1 || len(c.Files) != 1 || c.Files[0].Path != "calc/calc.go" {
		t.Errorf("result.coverage = %+v, want calc/calc.go with lines not run", c)
	}
}

// boolPtr boxes a coverage choice: a nil *bool takes the project's default,
// a non-nil one freezes that value into the task.
func boolPtr(v bool) *bool { return &v }

// The project row is a template for tasks created after it changes, never a
// leash on tasks that already exist: each task freezes its own coverage
// setting at creation, so the goalposts cannot move under a task, and an
// explicit off at creation beats a project that has it on.
func TestProjectCoverageIsFrozenIntoEachTask(t *testing.T) {
	h := newHarness(t, nil)

	// The first task registers the project with coverage off by default.
	first := h.createTask(nil)
	if first.Coverage {
		t.Fatal("first task coverage = true, want the off default")
	}

	repo, err := h.git.OpenRepository(h.ctx, h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := h.store.GetProjectByPath(h.ctx, repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.SetProjectCoverage(h.ctx, project.ID, true); err != nil {
		t.Fatalf("SetProjectCoverage: %v", err)
	}

	// A task created after the switch inherits it…
	second := h.createTask(nil)
	if !second.Coverage {
		t.Error("inherited coverage = false, want true from the project template")
	}
	// …the first keeps what it was created with, reloaded from the store so
	// the assertion is about the row, not the value returned earlier.
	reloaded, err := h.store.GetTask(h.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Coverage {
		t.Error("first task coverage after the project switched = true, want it frozen at false")
	}

	// An explicit off at creation beats the template.
	explicit := h.createTask(func(in *worker.CreateTaskInput) { in.Coverage = boolPtr(false) })
	if explicit.Coverage {
		t.Error("explicit coverage = true, want the caller's off to win over the template")
	}

	// Switching back does not rewrite what already exists.
	if _, err := h.store.SetProjectCoverage(h.ctx, project.ID, false); err != nil {
		t.Fatal(err)
	}
	reloaded, err = h.store.GetTask(h.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Coverage {
		t.Error("second task coverage after switching back = false, want it frozen at true")
	}

	// The audit log records the setting with the task.
	if cov := h.eventPayload(second.ID, "task.created")["coverage"]; cov != true {
		t.Errorf("task.created coverage = %v, want true", cov)
	}
}

// `aidev project coverage` is where the template for new tasks is set. Like
// approval it prints the current state with no argument — the common
// question is "do this project's tasks report coverage?" — and setting it
// must survive to the project list, which is where an operator sees every
// repository at once.
func TestProjectCoverageShowSetAndList(t *testing.T) {
	h := newHarness(t, nil)

	// The project does not exist until its first task registers it.
	if _, stderr, err := h.runCLI(t, "project", "coverage", "--repo", h.repoPath); err == nil {
		t.Errorf("coverage on an unknown repository succeeded; stderr: %s", stderr)
	}

	h.createTask(nil)

	stdout, stderr, err := h.runCLI(t, "project", "coverage", "--repo", h.repoPath)
	if err != nil {
		t.Fatalf("project coverage: %v\nstderr: %s", err, stderr)
	}
	if strings.TrimSpace(stdout) != "off" {
		t.Errorf("initial coverage = %q, want off", strings.TrimSpace(stdout))
	}

	stdout, stderr, err = h.runCLI(t, "project", "coverage", "--repo", h.repoPath, "on")
	if err != nil {
		t.Fatalf("project coverage on: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "coverage on") {
		t.Errorf("set output = %q, want it to confirm the new setting", stdout)
	}
	// The output must say the change applies to new tasks only: a reader
	// who believes existing tasks moved would misread their results.
	if !strings.Contains(stdout, "already created") {
		t.Errorf("set output does not say existing tasks keep their setting:\n%s", stdout)
	}

	stdout, stderr, err = h.runCLI(t, "project", "list")
	if err != nil {
		t.Fatalf("project list: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "coverage on") {
		t.Errorf("project list does not show the coverage setting:\n%s", stdout)
	}

	// An unknown word is refused without changing anything.
	if _, _, err := h.runCLI(t, "project", "coverage", "--repo", h.repoPath, "maybe"); err == nil {
		t.Error("an unknown word was accepted as a coverage setting")
	}
	stdout, _, err = h.runCLI(t, "project", "coverage", "--repo", h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "on" {
		t.Errorf("coverage after a rejected value = %q, want on to be unchanged", strings.TrimSpace(stdout))
	}
}

// The create flags must make the tri-state obvious: --coverage turns the
// report on for the task, --no-coverage turns it off against a project that
// has it on, and neither takes the project's default.
func TestTaskCreateCoverageFlags(t *testing.T) {
	h := newHarness(t, nil)

	_, stderr, _ := h.runCLI(t, "task", "create", "-h")
	for _, want := range []string{"-coverage", "-no-coverage"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("task create help does not mention %s:\n%s", want, stderr)
		}
	}

	// Register the project through a first task, then turn its default on.
	h.createTask(nil)
	repo, err := h.git.OpenRepository(h.ctx, h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := h.store.GetProjectByPath(h.ctx, repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.SetProjectCoverage(h.ctx, project.ID, true); err != nil {
		t.Fatalf("SetProjectCoverage: %v", err)
	}

	stdout, stderr, err := h.runCLI(t, "task", "create",
		"--repo", h.repoPath,
		"--title", "Create the marker file",
		"--verify", "test -f marker.txt",
		"--no-coverage",
		"--json")
	if err != nil {
		t.Fatalf("task create --no-coverage: %v\nstderr: %s", err, stderr)
	}
	var off struct {
		Coverage bool `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(stdout), &off); err != nil {
		t.Fatalf("the created task is not JSON: %v\n%s", err, stdout)
	}
	if off.Coverage {
		t.Errorf("coverage = true, want --no-coverage to beat the project's on default")
	}

	if _, _, err := h.runCLI(t, "task", "create",
		"--repo", h.repoPath,
		"--title", "Create the marker file",
		"--verify", "test -f marker.txt",
		"--coverage", "--no-coverage"); err == nil {
		t.Error("--coverage with --no-coverage was accepted; the two contradict each other")
	}
}
