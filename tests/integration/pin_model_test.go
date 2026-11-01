package integration

import (
	"testing"

	"aidev/internal/agent"
	"aidev/internal/config"
	"aidev/internal/logging"
	"aidev/internal/worker"
)

// A task created without a model ran with whatever model the configuration
// named when it ran, not when it was created: TASK-000143 has no model and
// ran on opencode/mimo-v2.6-flash-free, while conf.json names
// opencode-go/deepseek-v4.1-flash today. Editing the configuration between
// creating a task and running it — or between two attempts — silently
// changed the model, and the task row never said which one it was meant for.
//
// CreateTask resolves the model once, as the run would (the task's own, then
// the routing table for its hardness, then the backend's configured model),
// and stores it on the task.

func TestCreateTaskPinsTheConfiguredModel(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.OpenCodeModel = "provider/model-a" })
	h.backend.Work = doTheWork
	created := h.createTask(nil)
	if created.Model != "provider/model-a" {
		t.Fatalf("created.Model = %q, want the configured model pinned at creation", created.Model)
	}

	// The configuration changes before the task runs.
	cfg := h.orchestratorConfig()
	cfg.OpenCodeModel = "provider/model-b"
	later := &agent.Fake{Model: "provider/model-b", Work: doTheWork}
	orch := worker.New(h.store, h.git, later, cfg, logging.Discard())
	if _, err := orch.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	call, ok := later.LastCall()
	if !ok {
		t.Fatal("the backend was never called")
	}
	if call.Model != "provider/model-a" {
		t.Errorf("the run asked for %q, want the model pinned at creation", call.Model)
	}
}

// Routing by hardness is resolved at creation too.
func TestCreateTaskPinsTheRoutedModel(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.OpenCodeModel = "provider/default"
		c.Routing = map[string]string{"HARD": "provider/strong"}
	})
	created := h.createTask(func(in *worker.CreateTaskInput) { in.Hardness = "HARD" })
	if created.Model != "provider/strong" {
		t.Errorf("created.Model = %q, want the routed model", created.Model)
	}
}

// A model named on the task is kept as given.
func TestCreateTaskKeepsAnExplicitModel(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.OpenCodeModel = "provider/default" })
	created := h.createTask(func(in *worker.CreateTaskInput) { in.Model = "provider/chosen" })
	if created.Model != "provider/chosen" {
		t.Errorf("created.Model = %q, want the task's own model", created.Model)
	}
}

// With no model anywhere, nothing is invented: the backend still chooses.
func TestCreateTaskWithNoModelConfiguredPinsNothing(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)
	if created.Model != "" {
		t.Errorf("created.Model = %q, want empty when nothing names a model", created.Model)
	}
}
