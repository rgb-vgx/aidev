package integration

import (
	"testing"
)

// What the backend reports about processes its agent left behind reaches the
// history: task.worker_completed carries orphans_killed when it happened, and
// leaves the key out when it did not (an always-false field in every event of
// every clean run would be noise).

func TestWorkerCompletedRecordsOrphansKilled(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	h.backend.OrphansKilled = true
	created := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	payload := h.eventPayload(created.ID, "task.worker_completed")
	if payload["orphans_killed"] != true {
		t.Errorf("worker_completed payload = %v, want orphans_killed: true", payload)
	}
}

func TestWorkerCompletedOmitsOrphansWhenNoneWereLeft(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	created := h.createTask(nil)
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if _, present := h.eventPayload(created.ID, "task.worker_completed")["orphans_killed"]; present {
		t.Error("worker_completed carries orphans_killed for a run that left nothing behind")
	}
}
