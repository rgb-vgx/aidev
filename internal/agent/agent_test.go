package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidev/internal/task"
)

func validRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		TaskRef:        "TASK-000001",
		Prompt:         "Create greet.go",
		WorkingDir:     t.TempDir(),
		Timeout:        10 * time.Second,
		MaxOutputBytes: 4096,
	}
}

func TestRequestValidation(t *testing.T) {
	if err := validRequest(t).Validate(); err != nil {
		t.Fatalf("a valid request was rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Request)
		want   string
	}{
		{"no prompt", func(r *Request) { r.Prompt = "  " }, "prompt is empty"},
		{"no working dir", func(r *Request) { r.WorkingDir = "" }, "working directory is required"},
		{"relative working dir", func(r *Request) { r.WorkingDir = "rel" }, "must be absolute"},
		{"no timeout", func(r *Request) { r.Timeout = 0 }, "timeout must be positive"},
		{"no output cap", func(r *Request) { r.MaxOutputBytes = 0 }, "max output bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest(t)
			tc.mutate(&req)
			err := req.Validate()
			if err == nil {
				t.Fatalf("expected rejection mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestFakeDefaultsToSuccess(t *testing.T) {
	f := &Fake{}
	res, err := f.Run(context.Background(), validRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Succeeded() {
		t.Errorf("status = %s, want SUCCEEDED", res.Status)
	}
	if res.FailureKind != task.FailureNone {
		t.Errorf("failure kind = %s, want none on success", res.FailureKind)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if res.Backend != "fake" {
		t.Errorf("backend = %q, want fake", res.Backend)
	}
	if res.Duration < 0 || res.FinishedAt.Before(res.StartedAt) {
		t.Errorf("timing is not coherent: %+v", res)
	}
}

// The Work hook writes through req.WorkingDir, which is how a test proves the
// orchestrator handed the agent the worktree rather than somewhere else.
func TestFakeWorkRunsInTheRequestedDirectory(t *testing.T) {
	f := &Fake{
		Work: func(_ context.Context, req Request) error {
			return os.WriteFile(filepath.Join(req.WorkingDir, "greet.go"), []byte("package main\n"), 0o600)
		},
	}
	req := validRequest(t)

	if _, err := f.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(req.WorkingDir, "greet.go")); err != nil {
		t.Errorf("the work did not happen in the requested directory: %v", err)
	}

	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if last, ok := f.LastCall(); !ok || last.WorkingDir != req.WorkingDir {
		t.Errorf("LastCall = %+v, want the request just made", last)
	}
}

func TestFakeReportsConfiguredFailure(t *testing.T) {
	f := &Fake{Status: task.WorkerFailed, FailureKind: task.FailureAgentError, Stderr: "model exploded"}
	res, err := f.Run(context.Background(), validRequest(t))
	if err != nil {
		t.Fatalf("a failing agent must not be an error from Run: %v", err)
	}
	if res.Status != task.WorkerFailed || res.FailureKind != task.FailureAgentError {
		t.Errorf("result = %s/%s, want FAILED/AGENT_ERROR", res.Status, res.FailureKind)
	}
	if res.Stderr != "model exploded" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestFakeWorkErrorIsAnAgentFailure(t *testing.T) {
	f := &Fake{Work: func(context.Context, Request) error { return errors.New("could not write") }}
	res, err := f.Run(context.Background(), validRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerFailed || res.FailureKind != task.FailureAgentError {
		t.Errorf("result = %s/%s, want FAILED/AGENT_ERROR", res.Status, res.FailureKind)
	}
}

func TestFakeErrIsAnAidevLevelFailure(t *testing.T) {
	f := &Fake{Err: errors.New("executable not found")}
	res, err := f.Run(context.Background(), validRequest(t))
	if err == nil {
		t.Fatal("a backend that cannot run at all must return an error")
	}
	if res.FailureKind != task.FailureStartup {
		t.Errorf("failure kind = %s, want STARTUP", res.FailureKind)
	}
}

func TestFakeHonoursTimeout(t *testing.T) {
	f := &Fake{Delay: time.Hour}
	req := validRequest(t)
	req.Timeout = 50 * time.Millisecond

	res, err := f.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerTimedOut || res.FailureKind != task.FailureTimeout {
		t.Errorf("result = %s/%s, want TIMED_OUT/TIMEOUT", res.Status, res.FailureKind)
	}
}

func TestFakeHonoursCancellation(t *testing.T) {
	f := &Fake{Delay: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	res, err := f.Run(ctx, validRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerCancelled || res.FailureKind != task.FailureCancelled {
		t.Errorf("result = %s/%s, want CANCELLED/CANCELLED", res.Status, res.FailureKind)
	}
}

// A run cancelled after the work finished must still report cancelled: treating
// it as success would let a stopped task reach verification.
func TestFakeReportsCancellationDetectedAfterTheWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Fake{Work: func(context.Context, Request) error {
		cancel()
		return nil
	}}

	res, err := f.Run(ctx, validRequest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != task.WorkerCancelled {
		t.Errorf("status = %s, want CANCELLED", res.Status)
	}
}

func TestFakeRejectsInvalidRequest(t *testing.T) {
	f := &Fake{}
	res, err := f.Run(context.Background(), Request{})
	if err == nil {
		t.Fatal("an invalid request was accepted")
	}
	if res.FailureKind != task.FailureStartup {
		t.Errorf("failure kind = %s, want STARTUP", res.FailureKind)
	}
}

func TestFakeSatisfiesBackendAndValidator(t *testing.T) {
	var b Backend = &Fake{}
	if b.Name() == "" {
		t.Error("Name returned empty")
	}
	v, ok := b.(Validator)
	if !ok {
		t.Fatal("Fake does not implement Validator")
	}
	if err := v.ValidateAgentName(context.Background(), "build"); err != nil {
		t.Errorf("ValidateAgentName: %v", err)
	}
	if err := v.ValidateAgentName(context.Background(), ""); err == nil {
		t.Error("an empty agent name was accepted")
	}
}

// Succeeded describes the agent's own run, not the correctness of its work. The
// lifecycle enforces the distinction; this test records the intent.
func TestSucceededIsNotAJudgementOfCorrectness(t *testing.T) {
	res := Result{Status: task.WorkerSucceeded}
	if !res.Succeeded() {
		t.Fatal("Succeeded() = false for WorkerSucceeded")
	}
	if task.StatusRunning.CanTransitionTo(task.StatusSucceeded) {
		t.Error("a successful agent run can move a task straight to SUCCEEDED; verification would be bypassable")
	}
}
