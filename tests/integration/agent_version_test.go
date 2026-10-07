package integration

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"aidev/internal/task"
	"aidev/internal/worker"
)

// A worker run records which agent version and which model ran it, and every
// surface that shows the run shows both. Without them, the same task passing
// one day and failing the next after an agent upgrade looks like flakiness.

func TestWorkerRunRecordsTheAgentVersionAndModel(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	h.backend.AgentVersion = "9.9.9-test"
	created := h.createTask(func(in *worker.CreateTaskInput) { in.Model = "provider/model-x" })

	out, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v", out.Task.Status, err)
	}
	runs, err := h.store.ListWorkerRuns(h.ctx, out.Attempt.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("worker runs = %d, %v; want 1", len(runs), err)
	}
	if runs[0].AgentVersion != "9.9.9-test" {
		t.Errorf("agent_version = %q, want 9.9.9-test", runs[0].AgentVersion)
	}
	if runs[0].Model != "provider/model-x" {
		t.Errorf("model = %q, want provider/model-x", runs[0].Model)
	}

	text, stderr, err := h.runCLI(t, "task", "result", created.Ref)
	if err != nil {
		t.Fatalf("task result: %v\n%s", err, stderr)
	}
	for _, want := range []string{"9.9.9-test", "provider/model-x"} {
		if !strings.Contains(text, want) {
			t.Errorf("task result does not show %q:\n%s", want, text)
		}
	}

	raw, _, err := h.runCLI(t, "task", "result", created.Ref, "--json")
	if err != nil {
		t.Fatalf("task result --json: %v", err)
	}
	var result struct {
		Worker struct {
			AgentVersion string `json:"agent_version"`
			Model        string `json:"model"`
		} `json:"worker"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if result.Worker.AgentVersion != "9.9.9-test" || result.Worker.Model != "provider/model-x" {
		t.Errorf("worker = %+v, want agent_version 9.9.9-test and model provider/model-x", result.Worker)
	}
}

// A version that cannot be read is recorded as unknown. It never fails a run:
// the version describes the run, it does not decide it.
func TestAnUnreadableAgentVersionDoesNotFailTheRun(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	h.backend.VersionErr = errors.New("opencode --version: exit status 1")
	created := h.createTask(nil)

	out, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v; a version lookup must not fail the run", out.Task.Status, err)
	}
	runs, err := h.store.ListWorkerRuns(h.ctx, out.Attempt.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("worker runs = %d, %v", len(runs), err)
	}
	if runs[0].AgentVersion != "" {
		t.Errorf("agent_version = %q, want empty for an unreadable version", runs[0].AgentVersion)
	}
	raw, _, err := h.runCLI(t, "task", "result", created.Ref, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, `"agent_version"`) {
		t.Errorf("an unknown version is reported as a key; want it omitted:\n%s", raw)
	}
}

// MCP callers read the same evidence.
func TestMCPResultCarriesTheAgentVersionAndModel(t *testing.T) {
	m := newMCPHarness(t)
	m.backend.Work = doTheWork
	m.backend.AgentVersion = "9.9.9-test"
	created := m.createTask(func(in *worker.CreateTaskInput) { in.Model = "provider/model-x" })
	if out, err := m.orchestrator.RunTask(m.ctx, created.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v", out.Task.Status, err)
	}

	var got struct {
		Result struct {
			Worker struct {
				AgentVersion string `json:"agent_version"`
				Model        string `json:"model"`
			} `json:"worker"`
		} `json:"result"`
	}
	m.call(t, "aidev_get_task_result", map[string]any{"task": created.Ref}, &got)
	if got.Result.Worker.AgentVersion != "9.9.9-test" || got.Result.Worker.Model != "provider/model-x" {
		t.Errorf("result.worker = %+v, want agent_version and model", got.Result.Worker)
	}
}
