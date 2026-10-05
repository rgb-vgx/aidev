package integration

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
)

// After an automatic retry, `aidev task result` must show what the earlier
// attempts ran into: a reviewer reading a success on attempt 2 needs to know
// why attempt 1 failed, without digging through `aidev task events`.

func retriedTask(t *testing.T, h *harness) task.Task {
	t.Helper()
	var calls atomic.Int32
	h.backend.Work = func(ctx context.Context, req agent.Request) error {
		if calls.Add(1) == 1 {
			return nil // the first attempt leaves no marker, so its check fails
		}
		return doTheWork(ctx, req)
	}
	created := h.createTask(withRetries(1))
	outcome, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || outcome.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v; want SUCCEEDED on the retry", outcome.Task.Status, err)
	}
	return created
}

func TestTaskResultListsEarlierAttempts(t *testing.T) {
	h := newHarness(t, nil)
	created := retriedTask(t, h)

	stdout, stderr, err := h.runCLI(t, "task", "result", created.Ref)
	if err != nil {
		t.Fatalf("task result: %v\n%s", err, stderr)
	}
	for _, want := range []string{
		"attempt 2  SUCCEEDED",             // the latest attempt, as before
		"earlier attempts",                 // a section for the others
		"attempt 1  FAILED (VERIFICATION)", // each with its status and kind
		"verification did not pass",        // and the start of its error
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("task result output lacks %q:\n%s", want, stdout)
		}
	}
	// The section comes after the latest attempt, not before it.
	if strings.Index(stdout, "earlier attempts") < strings.Index(stdout, "attempt 2  SUCCEEDED") {
		t.Errorf("earlier attempts are listed before the latest one:\n%s", stdout)
	}
}

func TestTaskResultJSONListsEarlierAttempts(t *testing.T) {
	h := newHarness(t, nil)
	created := retriedTask(t, h)

	stdout, stderr, err := h.runCLI(t, "task", "result", created.Ref, "--json")
	if err != nil {
		t.Fatalf("task result --json: %v\n%s", err, stderr)
	}
	var result struct {
		Attempt struct {
			Number int `json:"number"`
		} `json:"attempt"`
		EarlierAttempts []struct {
			Number      int    `json:"number"`
			Status      string `json:"status"`
			FailureKind string `json:"failure_kind"`
			Error       string `json:"error"`
		} `json:"earlier_attempts"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if result.Attempt.Number != 2 {
		t.Errorf("attempt.number = %d, want 2", result.Attempt.Number)
	}
	if len(result.EarlierAttempts) != 1 {
		t.Fatalf("earlier_attempts = %+v, want exactly attempt 1", result.EarlierAttempts)
	}
	e := result.EarlierAttempts[0]
	if e.Number != 1 || e.Status != "FAILED" || e.FailureKind != "VERIFICATION" || !strings.Contains(e.Error, "verification did not pass") {
		t.Errorf("earlier_attempts[0] = %+v, want attempt 1 FAILED/VERIFICATION with its error", e)
	}
}

// One attempt means nothing earlier: no empty section, no empty JSON list.
func TestTaskResultWithOneAttemptHasNoEarlierAttempts(t *testing.T) {
	h := newHarness(t, nil)
	created := succeededTask(t, h)

	stdout, _, err := h.runCLI(t, "task", "result", created.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "earlier attempts") {
		t.Errorf("a single-attempt result shows an earlier-attempts section:\n%s", stdout)
	}
	jsonOut, _, err := h.runCLI(t, "task", "result", created.Ref, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonOut, "earlier_attempts") {
		t.Errorf("a single-attempt JSON result carries earlier_attempts:\n%s", jsonOut)
	}
}
