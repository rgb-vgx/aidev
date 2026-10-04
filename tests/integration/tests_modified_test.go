package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
)

// Research §7b tier 1: verification reports when the attempt changed the files
// that look like the tests judging it. The report must not decide the outcome —
// an honest run that updates a fixture alongside the code stays SUCCEEDED — but
// it must be recorded on every surface a reviewer reads: the event log, the
// in-process outcome, and the result read back from the database.

func TestVerificationReportsEditedTests(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.WorkingDir, "parser_test.go"), []byte("package parser\n"), 0o600)
	}

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	// The classifier only reports. Editing a test must not itself fail the run.
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED: a report must not decide the outcome", outcome.Task.Status)
	}

	if len(outcome.TestsModified) != 1 || outcome.TestsModified[0] != "parser_test.go" {
		t.Errorf("outcome.TestsModified = %v, want [parser_test.go]", outcome.TestsModified)
	}

	payload := h.eventPayload(created.ID, "task.verification_tests_modified")
	paths, _ := payload["paths"].([]any)
	if len(paths) != 1 || paths[0] != "parser_test.go" {
		t.Errorf("event payload paths = %v, want [parser_test.go]", payload["paths"])
	}

	// The report is part of the verification phase: after verification opened,
	// before the verdict it might influence.
	history := h.eventTypes(created.ID)
	a, b := indexOf(history, "task.verification_tests_modified"), indexOf(history, "task.verification_completed")
	if a < 0 || b < 0 || a > b {
		t.Errorf("history = %v, want task.verification_tests_modified recorded before task.verification_completed", history)
	}

	// A later process reads the same report from the database, so the
	// in-process outcome and the stored result cannot drift.
	if outcome.Attempt == nil {
		t.Fatal("outcome.Attempt is nil after a run")
	}
	stored, err := h.store.TestsModifiedPaths(h.ctx, created.ID, outcome.Attempt.ID)
	if err != nil {
		t.Fatalf("TestsModifiedPaths: %v", err)
	}
	if len(stored) != 1 || stored[0] != "parser_test.go" {
		t.Errorf("TestsModifiedPaths = %v, want [parser_test.go]", stored)
	}
}

// Silence is also a report: an attempt that left the tests alone must not
// leave a stale or empty event behind for a reviewer to misread.
func TestNoTestsModifiedEventWhenTestsLeftAlone(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork

	created := h.createTask(nil)
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED", outcome.Task.Status)
	}
	if outcome.TestsModified != nil {
		t.Errorf("outcome.TestsModified = %v, want nil", outcome.TestsModified)
	}
	if contains(h.eventTypes(created.ID), "task.verification_tests_modified") {
		t.Error("an event was recorded although no test path changed")
	}
}

// An attempt that edited tests and then got caught still edited them: the
// report is recorded before the interception check, so the refusal does not
// hide what a reviewer needs to see.
func TestEditedTestsRecordedEvenWhenIntercepted(t *testing.T) {
	h := newHarness(t, nil)
	commitCheckScript(h)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		// Rewrite the runner the verification would load, and add a test file
		// beside it: both facts must survive the interception.
		if err := os.WriteFile(filepath.Join(req.WorkingDir, "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.WorkingDir, "parser_test.go"), []byte("package parser\n"), 0o600)
	}

	created := h.createTask(verifyWith("./check.sh"))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	assertIntercepted(t, h, created, outcome, "./check.sh")

	if len(outcome.TestsModified) != 1 || outcome.TestsModified[0] != "parser_test.go" {
		t.Errorf("outcome.TestsModified = %v, want [parser_test.go]: interception must not hide what was edited", outcome.TestsModified)
	}
	history := h.eventTypes(created.ID)
	a, b := indexOf(history, "task.verification_tests_modified"), indexOf(history, "task.verification_intercepted")
	if a < 0 || b < 0 || a > b {
		t.Errorf("history = %v, want task.verification_tests_modified recorded before task.verification_intercepted", history)
	}
}

// The MCP surface assembles its result from the database, not from the
// in-process outcome, so it needs its own proof that the report arrives there.
func TestMCPResultSurfacesEditedTests(t *testing.T) {
	m := newMCPHarness(t)
	m.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.WorkingDir, "parser_test.go"), []byte("package parser\n"), 0o600)
	}

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Create the marker file",
		"description":  "Create marker.txt containing the word done",
		"verification": []string{"test -f marker.txt"},
	}, &created)
	ref, _ := created.Task["ref"].(string)
	if ref == "" {
		t.Fatal("created task has no ref")
	}

	var run struct {
		Result       map[string]any `json:"result"`
		StillRunning bool           `json:"still_running"`
		Succeeded    bool           `json:"succeeded"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": ref, "wait_seconds": 60}, &run)
	if run.StillRunning || !run.Succeeded {
		t.Fatalf("succeeded = %v, still_running = %v: %v", run.Succeeded, run.StillRunning, run.Result["message"])
	}

	var result struct {
		Result       map[string]any `json:"result"`
		StillRunning bool           `json:"still_running"`
	}
	m.call(t, "aidev_get_task_result", map[string]any{"task": ref}, &result)
	if result.StillRunning {
		t.Error("still_running is true for a finished task")
	}
	paths, _ := result.Result["tests_modified"].([]any)
	if len(paths) != 1 || paths[0] != "parser_test.go" {
		t.Errorf("result.tests_modified = %v, want [parser_test.go]", result.Result["tests_modified"])
	}
}
