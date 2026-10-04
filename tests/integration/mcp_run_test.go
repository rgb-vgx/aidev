package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidev/internal/agent"
	aidevmcp "aidev/internal/mcp"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// A run that dies before it records an ending must be reported as the crash it
// was — with the tail of its stderr, which is the only place the reason still
// lives. The database cannot say what happened, because the run never got far
// enough to write anything (research C2).
func TestMCPRunReportsADeadChild(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "dead-child.log")
	if err := os.WriteFile(logPath, []byte("panic: the agent connection dropped mid-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := func(context.Context, *worker.Orchestrator, task.Task) (func() error, string, error) {
		wait := func() error { return errors.New("exit status 7") }
		return wait, logPath, nil
	}
	m := newMCPHarnessWith(t, nil, aidevmcp.WithLauncher(launcher))

	tk := m.createTask(nil)

	msg := m.callExpectingError(t, "aidev_run_task", map[string]any{"task": tk.Ref, "wait_seconds": 5})
	for _, want := range []string{"exit status 7", "still", "panic: the agent connection dropped mid-run"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not contain %q:\n%s", want, msg)
		}
	}
}

// The server ending is not an interruption of the work: a run that has started
// must reach its own ending even when the session that asked for it goes away.
// Only the watcher stops with the server — if the run itself were bound to the
// server's context, the shutdown would record a cancellation here (research C2).
func TestMCPShutdownLeavesTheRunGoing(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	// Set after construction: the launcher runs a task the harness created,
	// and it deliberately ignores the context it is handed — a detached run
	// does not die with the session that started it.
	var runCtx context.Context
	launcher := func(_ context.Context, orch *worker.Orchestrator, tk task.Task) (func() error, string, error) {
		wait := func() error {
			_, err := orch.RunTask(runCtx, tk.ID.String())
			return err
		}
		return wait, "", nil
	}
	m := newMCPHarnessWith(t, nil, aidevmcp.WithLauncher(launcher))
	runCtx = m.ctx

	// The agent blocks until released: the run is guaranteed to be in flight
	// when the server stops, and to finish promptly afterwards.
	m.backend.Work = func(ctx context.Context, req agent.Request) error {
		select {
		case <-release:
		case <-ctx.Done():
			// A run bound to the server's context would take this path on
			// shutdown and record CANCELLED — the regression this test guards.
			return ctx.Err()
		}
		return doTheWork(ctx, req)
	}

	tk := m.createTask(nil)

	var run struct {
		StillRunning bool `json:"still_running"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": tk.Ref, "wait_seconds": 1}, &run)
	if !run.StillRunning {
		t.Fatal("the run finished before the server was stopped: the agent was supposed to block")
	}

	m.stopServe()
	close(release)
	// The cleanup above reads before closing, so a channel already closed here
	// is left alone.

	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := m.store.GetTask(m.ctx, tk.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status.Terminal() {
			if got.Status == task.StatusCancelled {
				t.Error("the server's shutdown cancelled the run: only the watching stops, not the work")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never finished after the server stopped: still %s", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The check before a spawn: a finished task must be refused with the same
// words the run itself would use, before any child process exists (research C2).
func TestMCPRefusesToRunAFinishedTask(t *testing.T) {
	m := newMCPHarness(t)
	m.backend.Work = doTheWork

	tk := m.createTask(nil)
	var run struct {
		Succeeded bool `json:"succeeded"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": tk.Ref, "wait_seconds": 60}, &run)
	if !run.Succeeded {
		t.Fatal("the first run must succeed for this test to have a finished task")
	}

	msg := m.callExpectingError(t, "aidev_run_task", map[string]any{"task": tk.Ref, "wait_seconds": 1})
	if !strings.Contains(msg, "already") || !strings.Contains(msg, "SUCCEEDED") {
		t.Errorf("error = %q, want it to say the task is already SUCCEEDED", msg)
	}
}
