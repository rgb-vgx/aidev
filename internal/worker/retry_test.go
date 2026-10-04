package worker

import (
	"strings"
	"testing"

	"aidev/internal/task"
	"aidev/internal/verification"
)

// A retry that continues the agent's session sends only what changed: which
// attempt this is, what failed with the end of its output, and the commands
// that will judge it again. The session already holds the task.
func TestRetryPromptForAContinuedSession(t *testing.T) {
	exit := 1
	report := verification.Report{Runs: []task.VerificationRun{
		{Command: "go vet ./...", Status: task.VerificationPassed},
		{Command: "go test ./...", Status: task.VerificationFailed, ExitCode: &exit,
			Stdout: "--- FAIL: TestGreet\n    greet_test.go:9: got \"\", want \"Hello, world\""},
		{Command: "make lint", Status: task.VerificationSkipped},
	}}
	rc := &retryContext{previous: 1, kind: task.FailureVerification, message: "verification did not pass: 1/3",
		sessionID: "ses_1", failed: failedSteps(report)}

	prompt, err := buildRetryPrompt(sampleTask(), 2, rc)
	if err != nil {
		t.Fatalf("buildRetryPrompt: %v", err)
	}
	for _, want := range []string{
		"# Attempt 2 of task TASK-000042",
		"The verification commands ran and at least one of them failed.",
		"`go test ./...` — FAILED, exit code 1",
		`greet_test.go:9: got "", want "Hello, world"`,
		"  - go test ./...",
		"  - go vet ./...",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// Passing and skipped steps are not failures to fix.
	if strings.Contains(prompt, "`go vet ./...` —") || strings.Contains(prompt, "`make lint` —") {
		t.Errorf("prompt lists steps that did not fail:\n%s", prompt)
	}
	if strings.Contains(prompt, "You are implementing one specific change") {
		t.Errorf("a continued session got the whole task again:\n%s", prompt)
	}
}

// Without a session to continue, the agent starts cold, so the task comes
// first and the failure after it.
func TestRetryPromptWithoutASessionCarriesTheTask(t *testing.T) {
	rc := &retryContext{previous: 1, kind: task.FailureAgentError,
		message: `opencode ended with finish reason "length"`}
	prompt, err := buildRetryPrompt(sampleTask(), 2, rc)
	if err != nil {
		t.Fatalf("buildRetryPrompt: %v", err)
	}
	task, retry := strings.Index(prompt, "You are implementing one specific change"), strings.Index(prompt, "# Attempt 2")
	if task < 0 || retry < 0 || task > retry {
		t.Errorf("want the task first and the retry section after it:\n%s", prompt)
	}
	if !strings.Contains(prompt, `finish reason "length"`) {
		t.Errorf("prompt lacks the recorded error:\n%s", prompt)
	}
}

func TestTailBytesKeepsTheEnd(t *testing.T) {
	if got := tailBytes("short", 100); got != "short" {
		t.Errorf("tailBytes of a short string = %q", got)
	}
	long := strings.Repeat("noise line\n", 1000) + "the real failure"
	got := tailBytes(long, 200)
	if !strings.HasSuffix(got, "the real failure") || len(got) > 210 {
		t.Errorf("tailBytes = %d bytes ending %q, want about 200 ending with the failure", len(got), got[len(got)-20:])
	}
	if !strings.HasPrefix(got, "…\nnoise line") {
		t.Errorf("tailBytes = %q..., want it to start at a line boundary after the ellipsis", got[:20])
	}
}
