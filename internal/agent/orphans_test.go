package agent

import (
	"context"
	"testing"

	"aidev/internal/task"
)

// The prompt tells the agent to leave no background process behind, and
// procexec stops any it does leave (TASK-000092), but the record never said
// it happened: procexec.Result.OrphansKilled was computed and read by no one.
// An agent that habitually leaves a dev server running looked exactly like
// one that does not.

func TestOpenCodeRecordsTheProcessesItsAgentLeftBehind(t *testing.T) {
	command, _ := fakeOpenCode(t, "sleep 30 >/dev/null 2>&1 &\n"+emit(fixtureStepStart, fixtureText, fixtureStepFinishStop)+"exit 0")
	res, err := NewOpenCode(command, "").Run(context.Background(), openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerSucceeded {
		t.Errorf("status = %s, want SUCCEEDED: leaving a process behind is recorded, not judged", res.Status)
	}
	if !res.OrphansKilled {
		t.Error("OrphansKilled = false, want true: the agent's background sleep outlived it and was stopped")
	}
}

func TestOpenCodeWithNothingLeftBehindRecordsNoOrphans(t *testing.T) {
	command, _ := fakeOpenCode(t, emit(fixtureStepStart, fixtureText, fixtureStepFinishStop)+"exit 0")
	res, err := NewOpenCode(command, "").Run(context.Background(), openCodeRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OrphansKilled {
		t.Error("OrphansKilled = true for a run that left nothing behind")
	}
}
