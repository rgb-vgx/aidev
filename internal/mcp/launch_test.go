package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aidev/internal/config"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// The child must not inherit this server's channels: stdout is the JSON-RPC
// channel (invariant 3), stdin is the client's side of it, and stderr is the
// one channel that should be captured for diagnosis (research C2).
func TestNewRunChildIsolatesChannels(t *testing.T) {
	logFile, err := os.CreateTemp(t.TempDir(), "run-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	cmd, closeStdout, err := newRunChild("/usr/bin/aidev", "TASK-000001", logFile)
	if err != nil {
		t.Fatalf("newRunChild: %v", err)
	}
	defer closeStdout()

	want := []string{"/usr/bin/aidev", "task", "run", "TASK-000001"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
	if cmd.Stdout == nil {
		t.Fatal("stdout is nil: the child would inherit this server's, which is the protocol channel")
	}
	if cmd.Stdout == os.Stdout {
		t.Error("the child's stdout is this process's stdout: nothing but the protocol may write there (invariant 3)")
	}
	if cmd.Stderr != logFile {
		t.Errorf("stderr = %v, want the run's log file", cmd.Stderr)
	}
	if cmd.Stdin != nil {
		t.Errorf("stdin = %v, want nil so the child reads nothing from the protocol channel", cmd.Stdin)
	}
}

// TestDetachedLaunchSpawnsTheCLI drives the real spawn path with a stand-in
// executable: the child is `aidev task run <ref>`, its stderr lands in a log
// under the workspace root, its stdout does not surface anywhere, and its exit
// status is what wait reports.
func TestDetachedLaunchSpawnsTheCLI(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "fake-aidev")
	script := "#!/bin/sh\n" +
		"echo 'from the child stdout'\n" +
		"echo 'from the child stderr' >&2\n" +
		"exit 7\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	orch := worker.New(nil, nil, nil, config.Config{WorkspaceRoot: workspace}, nil)
	s := &Server{exePath: exe}

	wait, logPath, err := s.detachedLaunch(context.Background(), orch, task.Task{Ref: "TASK-000001"})
	if err != nil {
		t.Fatalf("detachedLaunch: %v", err)
	}

	waitErr := wait()
	if waitErr == nil || !strings.Contains(waitErr.Error(), "exit status 7") {
		t.Errorf("wait error = %v, want the child's exit status", waitErr)
	}

	if filepath.Base(filepath.Dir(logPath)) != runLogDirname {
		t.Errorf("log %q is not in a %q directory under the workspace", logPath, runLogDirname)
	}
	if !strings.HasPrefix(logPath, workspace) {
		t.Errorf("log %q is outside the workspace root %q", logPath, workspace)
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read the run log: %v", err)
	}
	if !strings.Contains(string(content), "from the child stderr") {
		t.Errorf("the run log does not hold the child's stderr: %q", content)
	}
	if strings.Contains(string(content), "from the child stdout") {
		t.Errorf("the child's stdout reached the log instead of the null device: %q", content)
	}
}

// A shutdown stops the watching, not the run: the watcher must return as soon
// as its context is cancelled rather than hold the connection for a process
// that outlives it anyway (research C2).
func TestAwaitOutcomeStopsWatchingOnShutdown(t *testing.T) {
	s := &Server{}
	release := make(chan struct{})
	wait := func() error { <-release; return nil }
	// Unblocked after the test so the waiter goroutine can finish; buffered
	// waitCh means it cannot leak either way.
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- s.awaitOutcome(ctx, nil, uuid.Nil, wait, "") }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitOutcome did not notice the shutdown")
	}
}

func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	full := "0123456789"
	fullPath := filepath.Join(dir, "full")
	if err := os.WriteFile(fullPath, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(dir, "empty")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		path string
		max  int64
		want string
	}{
		"no path":    {"", 10, ""},
		"missing":    {filepath.Join(dir, "gone"), 10, ""},
		"empty file": {emptyPath, 10, ""},
		"file fits":  {fullPath, 100, full},
		"tail only":  {fullPath, 4, "6789"},
	}
	for name, tc := range tests {
		if got := tailFile(tc.path, tc.max); got != tc.want {
			t.Errorf("%s: tailFile(%q, %d) = %q, want %q", name, tc.path, tc.max, got, tc.want)
		}
	}
}
