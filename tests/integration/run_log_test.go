package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A run started by the MCP server is a detached `aidev task run` process whose
// stderr goes to a log under workspace_root/run-logs/ (research C2). When such
// a run dies before recording an ending, that file is the only evidence of
// why — and until now nothing but the tool's error message named it.
//
// `aidev task run-log <task>` reads it: the newest log for the task, or all of
// them, or just the path.

// writeRunLog puts a log file where the MCP launcher writes one.
func writeRunLog(t *testing.T, h *harness, ref, content string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(h.workspace, "run-logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%s-%d.log", ref, time.Now().Add(-age).UnixNano())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTaskRunLogShowsTheNewestLog(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)
	writeRunLog(t, h, created.Ref, "older run: exit status 1\n", time.Hour)
	writeRunLog(t, h, created.Ref, "newer run: panic: the agent connection dropped\n", 0)

	stdout, stderr, err := h.runCLI(t, "task", "run-log", created.Ref)
	if err != nil {
		t.Fatalf("task run-log: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "panic: the agent connection dropped") {
		t.Errorf("output = %q, want the newest log's contents", stdout)
	}
	if strings.Contains(stdout, "older run") {
		t.Errorf("output = %q, want only the newest log", stdout)
	}
	if !strings.Contains(stdout, created.Ref) {
		t.Errorf("output = %q, want it to name the log file it read", stdout)
	}
}

func TestTaskRunLogAllShowsEveryLogOldestFirst(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)
	writeRunLog(t, h, created.Ref, "first attempt\n", 2*time.Hour)
	writeRunLog(t, h, created.Ref, "second attempt\n", time.Hour)

	stdout, stderr, err := h.runCLI(t, "task", "run-log", created.Ref, "--all")
	if err != nil {
		t.Fatalf("task run-log --all: %v\n%s", err, stderr)
	}
	first, second := strings.Index(stdout, "first attempt"), strings.Index(stdout, "second attempt")
	if first < 0 || second < 0 {
		t.Fatalf("output = %q, want both logs", stdout)
	}
	if first > second {
		t.Errorf("logs are newest first:\n%s", stdout)
	}
}

func TestTaskRunLogPathPrintsThePathOnly(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)
	path := writeRunLog(t, h, created.Ref, "boom\n", 0)

	stdout, stderr, err := h.runCLI(t, "task", "run-log", created.Ref, "--path")
	if err != nil {
		t.Fatalf("task run-log --path: %v\n%s", err, stderr)
	}
	if strings.TrimSpace(stdout) != path {
		t.Errorf("output = %q, want the path %q and nothing else", stdout, path)
	}
	if strings.Contains(stdout, "boom") {
		t.Error("--path printed the log's contents")
	}
}

// Only this task's logs. A log is named <reference>-<unixnanos>.log, so a file
// that merely starts with the reference — another task's, or something a
// person left in the directory — is not one of them.
func TestTaskRunLogShowsOnlyItsOwnLogs(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)
	writeRunLog(t, h, created.Ref, "mine\n", 0)
	writeRunLog(t, h, created.Ref+"-a2", "a file that merely starts with the reference\n", 0)
	writeRunLog(t, h, "TASK-999999", "someone else's\n", 0)

	stdout, stderr, err := h.runCLI(t, "task", "run-log", created.Ref, "--all")
	if err != nil {
		t.Fatalf("task run-log --all: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "mine") {
		t.Errorf("output = %q, want the task's own log", stdout)
	}
	for _, other := range []string{"merely starts with", "someone else's"} {
		if strings.Contains(stdout, other) {
			t.Errorf("output = %q, want no other log", stdout)
		}
	}
}

// A task with no log says so, and says why there may be none: only runs the
// MCP server started write one.
func TestTaskRunLogSaysWhenThereIsNone(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createTask(nil)

	_, _, err := h.runCLI(t, "task", "run-log", created.Ref)
	if err == nil {
		t.Fatal("task run-log succeeded although no log exists")
	}
	for _, want := range []string{created.Ref, "no run log", "aidev task run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}
