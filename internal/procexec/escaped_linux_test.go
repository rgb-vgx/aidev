//go:build linux

package procexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A process that leaves the child's process group — setsid, as a test runner
// does when it starts an application under xvfb-run — is out of reach of the
// group signals. TASK-000092 (2026-10-08) left WPS running that way after its
// run was cancelled. Run also finds a run's descendants by an environment
// marker it gave the child, which every descendant inherits whatever session
// it moved to, and stops them with the rest.

func escapedChild(t *testing.T) (spec Spec, pidfile string) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	dir := t.TempDir()
	pidfile = filepath.Join(dir, "escaped.pid")
	spec = Spec{
		Command:        "sh",
		Args:           []string{"-c", "setsid sh -c 'echo $$ > " + pidfile + "; exec sleep 300' </dev/null >/dev/null 2>&1 & sleep 300"},
		Dir:            dir,
		Timeout:        time.Minute,
		MaxOutputBytes: 1 << 16,
		FollowSessions: true,
	}
	return spec, pidfile
}

func waitForFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pid := tryReadPid(path); pid > 0 {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return 0
}

func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestCancellingARunStopsADescendantInAnotherSession(t *testing.T) {
	shortenKillGrace(t, 500*time.Millisecond)
	s, pidfile := escapedChild(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = Run(ctx, s); close(done) }()

	escaped := waitForFile(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if processAlive(escaped) {
		t.Errorf("the descendant in its own session (pid %d) outlived the cancelled run", escaped)
	}
}

// Invariant 6 has no exception for commands that succeeded, and none for
// helpers that changed session either.
func TestAFinishedRunLeavesNoDescendantInAnotherSession(t *testing.T) {
	shortenKillGrace(t, 500*time.Millisecond)
	s, pidfile := escapedChild(t)
	s.Args = []string{"-c", "setsid sh -c 'echo $$ > " + pidfile + "; exec sleep 300' </dev/null >/dev/null 2>&1 & while [ ! -s " + pidfile + " ]; do sleep 0.05; done"}

	if _, err := Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	escaped := tryReadPid(pidfile)
	if escaped <= 0 {
		t.Fatal("the descendant never started")
	}
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })
	if processAlive(escaped) {
		t.Errorf("the descendant in its own session (pid %d) outlived a run that exited 0", escaped)
	}
}

func tryReadPid(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

// Without FollowSessions — git plumbing, which never starts a session — the
// run costs no /proc scan, and a descendant in another session is out of
// reach as it always was. The test pins that the cost is opt-in.
func TestFollowingSessionsIsOptIn(t *testing.T) {
	shortenKillGrace(t, 300*time.Millisecond)
	s, pidfile := escapedChild(t)
	s.FollowSessions = false
	s.Args = []string{"-c", "setsid sh -c 'echo $$ > " + pidfile + "; exec sleep 300' </dev/null >/dev/null 2>&1 & while [ ! -s " + pidfile + " ]; do sleep 0.05; done"}
	if _, err := Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	escaped := tryReadPid(pidfile)
	t.Cleanup(func() { _ = syscall.Kill(escaped, syscall.SIGKILL) })
	if !processAlive(escaped) {
		t.Errorf("a run without FollowSessions reached a process in another session (pid %d)", escaped)
	}
}
