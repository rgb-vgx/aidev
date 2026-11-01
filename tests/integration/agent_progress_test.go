package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aidev/internal/agent"
	"aidev/internal/store"
	"aidev/internal/task"
)

// The backend reports a running agent's progress through Request.OnProgress
// (TASK-000165); until the worker records it, nothing reaches the history
// between task.worker_started and task.worker_completed — TASK-000143 had 20
// minutes of nothing, and aidev_get_task_events could not say whether its
// agent was working or stuck.
//
// Every report becomes a task.agent_progress event on the attempt, while the
// agent runs, and the request bounds how often a backend reports.

func TestAgentProgressIsRecordedWhileTheAgentRuns(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if req.OnProgress == nil {
			t.Error("the request carries no progress callback")
			return doTheWork(ctx, req)
		}
		req.OnProgress(agent.Progress{ToolCalls: 1, Steps: 0, LastTool: "read", LastPath: "main.go", Elapsed: 2 * time.Second})
		req.OnProgress(agent.Progress{ToolCalls: 2, Steps: 1, LastTool: "write", LastPath: "marker.txt", Elapsed: 3500 * time.Millisecond})
		return doTheWork(ctx, req)
	}
	created := h.createTask(nil)

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s: %s", outcome.Task.Status, outcome.Message)
	}

	call, _ := h.backend.LastCall()
	if call.ProgressInterval <= 0 || call.ProgressInterval > time.Minute {
		t.Errorf("ProgressInterval = %s, want a bound between 0 and a minute so a busy agent does not flood the history", call.ProgressInterval)
	}

	events, err := h.store.ListEvents(h.ctx, store.EventFilter{TaskID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	var progress []map[string]any
	started, completed := -1, -1
	for i, e := range events {
		switch e.Type.String() {
		case "task.worker_started":
			started = i
		case "task.worker_completed":
			completed = i
		case "task.agent_progress":
			if e.AttemptID == nil {
				t.Error("task.agent_progress is not attached to the attempt")
			}
			if i < started || (completed >= 0 && i > completed) {
				t.Errorf("task.agent_progress at %d is outside worker_started (%d) .. worker_completed", i, started)
			}
			var payload map[string]any
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("payload: %v", err)
			}
			progress = append(progress, payload)
		}
	}
	if len(progress) != 2 {
		t.Fatalf("recorded %d task.agent_progress events, want 2", len(progress))
	}
	last := progress[1]
	want := map[string]any{"tool_calls": float64(2), "steps": float64(1), "last_tool": "write", "last_path": "marker.txt", "elapsed_ms": float64(3500)}
	for k, v := range want {
		if last[k] != v {
			t.Errorf("payload[%s] = %v, want %v (payload %v)", k, last[k], v, last)
		}
	}
}
