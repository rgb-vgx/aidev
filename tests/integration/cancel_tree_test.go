package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `aidev task cancel` stops a run in another process: the detached runner the
// MCP server starts, or an `aidev task run` in another terminal. TASK-000092
// (Axiom-Office, 2026-10-08) was cancelled, reported "cancelled (was
// RUNNING)", and WPS — started by the agent's test command with a new session,
// out of the agent's process group — kept running until it was killed by
// hand. The cancel had returned the moment the status changed, saying nothing
// about whether anything had stopped.
//
// The fake agent here does what that test command did: it starts a process
// in a new session (setsid), then works for a long time.

type cancelRig struct {
	*harness
	conf, dir string
}

func newCancelRig(t *testing.T) *cancelRig {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	h := newHarness(t, nil)
	dir := t.TempDir()
	opencode := filepath.Join(dir, "opencode")
	stub := fmt.Sprintf(`#!/bin/sh
if [ "$1" = agent ] && [ "$2" = list ]; then printf 'build (primary)\n'; exit 0; fi
if [ "$1" = --version ]; then echo 9.9.9; exit 0; fi
echo $$ > %[1]q/agent.pid
setsid sh -c 'echo $$ > %[1]q/escaped.pid; exec sleep 300' </dev/null >/dev/null 2>&1 &
sleep 300
`, dir)
	if err := os.WriteFile(opencode, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"database":       map[string]any{"url": os.Getenv(envDatabaseURL)},
		"workspace_root": h.workspace,
		"log_level":      "error",
		"agent":          map[string]any{"opencode": map[string]any{"command": opencode}},
	})
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "conf.json")
	if err := os.WriteFile(conf, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &cancelRig{harness: h, conf: conf, dir: dir}
	t.Cleanup(func() {
		for _, name := range []string{"agent.pid", "escaped.pid"} {
			if pid := r.pid(name); pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return r
}

func (r *cancelRig) pid(name string) int {
	raw, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

// startRunner starts `aidev task run` and returns a channel closed when it
// exits; the test is its parent, so it has to reap it to see it gone.
func (r *cancelRig) startRunner(t *testing.T, ref string) (*exec.Cmd, chan struct{}) {
	t.Helper()
	cmd := exec.Command(aidevBinary(t), "task", "run", ref)
	cmd.Env = append(os.Environ(), "AIDEV_CONFIG="+r.conf)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start aidev task run: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, done
}

func (r *cancelRig) cancel(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(aidevBinary(t), append([]string{"task", "cancel"}, args...)...)
	cmd.Env = append(os.Environ(), "AIDEV_CONFIG="+r.conf)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func alive(pid int) bool {
	return pid > 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestCancelStopsTheRunnerAndEveryProcessItsAgentStarted(t *testing.T) {
	r := newCancelRig(t)
	created := r.createTask(nil)
	_, runnerDone := r.startRunner(t, created.Ref)
	waitFor(t, "the agent to start a process in its own session", func() bool { return r.pid("escaped.pid") > 0 })
	escaped, agentPID := r.pid("escaped.pid"), r.pid("agent.pid")

	out, err := r.cancel(t, created.Ref)
	if err != nil {
		t.Fatalf("aidev task cancel: %v\n%s", err, out)
	}

	// Cancel returns once the runner has stopped, not before.
	select {
	case <-runnerDone:
	case <-time.After(time.Second):
		t.Fatalf("aidev task cancel returned while the runner was still running:\n%s", out)
	}
	if alive(agentPID) {
		t.Errorf("the agent (pid %d) outlived the cancel", agentPID)
	}
	if alive(escaped) {
		t.Errorf("the process the agent started in its own session (pid %d) outlived the cancel", escaped)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("cancel output does not say the run stopped:\n%s", out)
	}
	if !contains(r.eventTypes(created.ID), "task.run_stopped") {
		t.Fatalf("no task.run_stopped event: %v", r.eventTypes(created.ID))
	}
	if payload := r.eventPayload(created.ID, "task.run_stopped"); payload["stopped"] != true {
		t.Errorf("task.run_stopped = %v, want stopped true", payload)
	}
}

// A runner that does not stop — hung, or an aidev too old to stop its agent's
// whole tree — is named, not passed over: the cancel says which process is
// still running and how to stop it, and exits non-zero.
func TestCancelSaysWhenTheRunnerDoesNotStop(t *testing.T) {
	r := newCancelRig(t)
	created := r.createTask(nil)
	runner, _ := r.startRunner(t, created.Ref)
	waitFor(t, "the agent to start", func() bool { return r.pid("escaped.pid") > 0 })

	// Frozen, the runner can neither poll nor react: the stand-in for one
	// that never will.
	if err := runner.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Process.Signal(syscall.SIGCONT) })

	start := time.Now()
	out, err := r.cancel(t, created.Ref, "--wait", "2s")
	if err == nil {
		t.Errorf("cancel exited 0 although the runner is still running:\n%s", out)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("cancel took %s with --wait 2s", took)
	}
	for _, want := range []string{"still running", strconv.Itoa(runner.Process.Pid), "kill"} {
		if !strings.Contains(out, want) {
			t.Errorf("cancel output lacks %q:\n%s", want, out)
		}
	}
	// The task is cancelled either way: the state change does not depend on
	// the runner agreeing.
	if got, err := r.store.GetTask(r.ctx, created.ID); err != nil || got.Status.String() != "CANCELLED" {
		t.Errorf("status = %v, %v; want CANCELLED", got.Status, err)
	}
	if payload := r.eventPayload(created.ID, "task.run_stopped"); payload["stopped"] != false {
		t.Errorf("task.run_stopped = %v, want stopped false", payload)
	}
}
