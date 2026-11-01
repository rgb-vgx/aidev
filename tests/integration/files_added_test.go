package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// The snapshot takes every file git does not ignore, so whatever appears in
// the worktree while the agent runs is delivered with its work. TASK-000143
// delivered data/research.db — 49 KB of SQLite an MCP server created in its
// working directory — and the record said only "changed_files: 2"; the
// agent's own summary said one file had changed. Nothing in the history named
// the file until someone read the commit.
//
// task.files_added names the files the delivered tree adds to the base, with
// their size and whether git reads them as binary, so the review sees them
// without opening the commit. It is a report, like
// task.verification_tests_modified: the run is judged as usual.

func addedEntries(t *testing.T, payload map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := payload["paths"].([]any)
	if !ok {
		t.Fatalf("task.files_added has no paths list: %v", payload)
	}
	out := map[string]map[string]any{}
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("task.files_added entry is %T, want an object", item)
		}
		path, _ := entry["path"].(string)
		out[path] = entry
	}
	return out
}

func TestFilesTheAttemptAddsAreNamed(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if err := doTheWork(ctx, req); err != nil {
			return err
		}
		// A file nobody asked for, as an MCP server writing into its working
		// directory leaves one: binary content, a directory of its own.
		path := filepath.Join(req.WorkingDir, "data", "research.db")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("SQLite format 3\x00\x10\x00\x01\x01"), 0o600)
	}
	created := h.createTask(nil)

	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED (the report never decides): %s", outcome.Task.Status, outcome.Message)
	}

	entries := addedEntries(t, h.eventPayload(created.ID, "task.files_added"))
	if len(entries) != 2 {
		t.Fatalf("files_added names %d paths, want marker.txt and data/research.db: %v", len(entries), entries)
	}
	db, ok := entries["data/research.db"]
	if !ok {
		t.Fatalf("data/research.db is not named: %v", entries)
	}
	if db["binary"] != true {
		t.Errorf("data/research.db binary = %v, want true", db["binary"])
	}
	if db["bytes"] != float64(20) {
		t.Errorf("data/research.db bytes = %v, want 20", db["bytes"])
	}
	marker := entries["marker.txt"]
	if marker["binary"] != false {
		t.Errorf("marker.txt binary = %v, want false", marker["binary"])
	}
	if marker["bytes"] != float64(5) {
		t.Errorf("marker.txt bytes = %v, want 5", marker["bytes"])
	}

	// Before verification starts, so it is there however the checks end.
	types := h.eventTypes(created.ID)
	if at, verify := indexOf(types, "task.files_added"), indexOf(types, "task.verification_started"); at < 0 || at > verify {
		t.Errorf("task.files_added at %d, verification_started at %d; want the report before verification: %v", at, verify, types)
	}
}

// An attempt that only edits files the base already has adds nothing, and
// the history says nothing about it.
func TestNoFilesAddedNoReport(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = func(_ context.Context, req agent.Request) error {
		return os.WriteFile(filepath.Join(req.WorkingDir, "main.go"), []byte("package main\n\nfunc main() { _ = 1 }\n"), 0o600)
	}
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "true"}}
	})
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if contains(h.eventTypes(created.ID), "task.files_added") {
		t.Errorf("task.files_added was recorded for an attempt that added no file: %v", h.eventTypes(created.ID))
	}
}
