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
		in.Coverage = true
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
		in.Coverage = true
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
