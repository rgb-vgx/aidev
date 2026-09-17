package verification

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aidev/internal/task"
)

func runner() *Runner { return NewRunner(20*time.Second, 64*1024) }

func request(t *testing.T, steps ...task.VerificationStep) Request {
	t.Helper()
	return Request{AttemptID: uuid.Must(uuid.NewV7()), WorkingDir: t.TempDir(), Steps: steps}
}

func step(command string, args ...string) task.VerificationStep {
	return task.VerificationStep{Command: command, Args: args}
}

func TestAllStepsPass(t *testing.T) {
	req := request(t,
		step("sh", "-c", "echo first"),
		step("sh", "-c", "echo second"),
	)

	report, err := runner().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("Passed = false, want true: %s", report.Summary())
	}
	if report.FailureKind != task.FailureNone {
		t.Errorf("failure kind = %s, want none", report.FailureKind)
	}
	if len(report.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(report.Runs))
	}
	for i, run := range report.Runs {
		if run.Status != task.VerificationPassed {
			t.Errorf("step %d status = %s, want PASSED", i, run.Status)
		}
		if run.ExitCode == nil || *run.ExitCode != 0 {
			t.Errorf("step %d exit code = %v, want 0", i, run.ExitCode)
		}
		if run.StepIndex != i {
			t.Errorf("step %d recorded index %d", i, run.StepIndex)
		}
		if run.AttemptID != req.AttemptID {
			t.Errorf("step %d lost the attempt id", i)
		}
		if run.FinishedAt == nil {
			t.Errorf("step %d has no finish time", i)
		}
	}
	if _, ok := report.FirstFailure(); ok {
		t.Error("FirstFailure reported a failure on a passing report")
	}
	if !strings.Contains(report.Summary(), "2/2") {
		t.Errorf("summary = %q, want it to count the steps", report.Summary())
	}
}

// Exit codes and output are what aidev records; the agent's opinion is not
// consulted anywhere in this package.
func TestOutputAndExitCodeAreCaptured(t *testing.T) {
	req := request(t, step("sh", "-c", "echo to-stdout; echo to-stderr >&2; exit 3"))

	report, err := runner().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("a step exiting 3 was reported as passing")
	}

	run := report.Runs[0]
	if run.ExitCode == nil || *run.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", run.ExitCode)
	}
	if strings.TrimSpace(run.Stdout) != "to-stdout" {
		t.Errorf("stdout = %q", run.Stdout)
	}
	if strings.TrimSpace(run.Stderr) != "to-stderr" {
		t.Errorf("stderr = %q", run.Stderr)
	}
	if report.FailureKind != task.FailureVerification {
		t.Errorf("failure kind = %s, want VERIFICATION", report.FailureKind)
	}
}

// Once a check has failed the task cannot succeed, so later commands are not run.
// They are still recorded as SKIPPED, because a missing row could be mistaken for
// a pass.
func TestFailureStopsLaterStepsButRecordsThem(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "third-ran")

	req := Request{
		AttemptID:  uuid.Must(uuid.NewV7()),
		WorkingDir: dir,
		Steps: []task.VerificationStep{
			step("sh", "-c", "exit 0"),
			step("sh", "-c", "exit 1"),
			step("sh", "-c", "touch "+marker),
		},
	}

	report, err := runner().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("Passed = true despite a failing step")
	}
	if len(report.Runs) != 3 {
		t.Fatalf("got %d runs, want one per declared step", len(report.Runs))
	}

	want := []task.VerificationStatus{task.VerificationPassed, task.VerificationFailed, task.VerificationSkipped}
	for i, w := range want {
		if report.Runs[i].Status != w {
			t.Errorf("step %d status = %s, want %s", i, report.Runs[i].Status, w)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the step after the failure was executed")
	}

	// A skipped step has no exit code, so it can never be read as a pass.
	if report.Runs[2].ExitCode != nil {
		t.Errorf("skipped step exit code = %v, want nil", report.Runs[2].ExitCode)
	}

	failure, ok := report.FirstFailure()
	if !ok {
		t.Fatal("FirstFailure found nothing")
	}
	if failure.StepIndex != 1 {
		t.Errorf("FirstFailure = step %d, want 1", failure.StepIndex)
	}
	if !strings.Contains(report.Summary(), "exited 1") {
		t.Errorf("summary = %q, want it to name the failure", report.Summary())
	}
}

func TestPerStepTimeout(t *testing.T) {
	req := request(t,
		task.VerificationStep{Command: "sh", Args: []string{"-c", "sleep 30"}, TimeoutSeconds: 1},
		step("sh", "-c", "exit 0"),
	)

	start := time.Now()
	report, err := runner().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("took %s; the per-step timeout was not applied", elapsed)
	}
	if report.Runs[0].Status != task.VerificationTimedOut {
		t.Errorf("status = %s, want TIMED_OUT", report.Runs[0].Status)
	}
	if report.FailureKind != task.FailureTimeout {
		t.Errorf("failure kind = %s, want TIMEOUT, which is not the same as a failed check", report.FailureKind)
	}
	if report.Passed {
		t.Error("a timed-out step was reported as passing")
	}
	if !strings.Contains(report.Summary(), "timed out") {
		t.Errorf("summary = %q", report.Summary())
	}
}

func TestDefaultTimeoutAppliesWhenStepHasNone(t *testing.T) {
	r := NewRunner(500*time.Millisecond, 4096)
	report, err := r.Run(context.Background(), request(t, step("sh", "-c", "sleep 30")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Runs[0].Status != task.VerificationTimedOut {
		t.Errorf("status = %s, want TIMED_OUT from the runner default", report.Runs[0].Status)
	}
}

func TestCancellationDuringAStep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	report, err := runner().Run(ctx, request(t, step("sh", "-c", "sleep 30"), step("sh", "-c", "exit 0")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Runs[0].Status != task.VerificationCancelled {
		t.Errorf("status = %s, want CANCELLED", report.Runs[0].Status)
	}
	if report.FailureKind != task.FailureCancelled {
		t.Errorf("failure kind = %s, want CANCELLED", report.FailureKind)
	}
	if report.Passed {
		t.Error("a cancelled pass was reported as passing")
	}
}

// A cancellation before a step starts must be recorded as cancelled, not skipped:
// the step was not attempted because the caller stopped, which is a different
// fact from an earlier step having failed.
func TestCancellationBeforeAnyStepRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := runner().Run(ctx, request(t, step("sh", "-c", "exit 0"), step("sh", "-c", "exit 0")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Runs[0].Status != task.VerificationCancelled {
		t.Errorf("first step status = %s, want CANCELLED", report.Runs[0].Status)
	}
	if report.Runs[1].Status != task.VerificationSkipped {
		t.Errorf("second step status = %s, want SKIPPED", report.Runs[1].Status)
	}
	if report.Passed {
		t.Error("Passed = true for a cancelled run")
	}
}

// A command that does not exist is a real verification failure: the task named
// something that cannot run here, and the user needs to see that rather than an
// internal error.
func TestUnrunnableCommandIsAFailedCheck(t *testing.T) {
	report, err := runner().Run(context.Background(), request(t, step("aidev-no-such-command-xyz")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("a nonexistent command was reported as passing")
	}
	if report.Runs[0].Status != task.VerificationFailed {
		t.Errorf("status = %s, want FAILED", report.Runs[0].Status)
	}
	if report.Runs[0].Stderr == "" {
		t.Error("no diagnostic was recorded for an unrunnable command")
	}
}

func TestStepsRunInTheWorktree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "only-here.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := runner().Run(context.Background(), Request{
		AttemptID:  uuid.Must(uuid.NewV7()),
		WorkingDir: dir,
		Steps:      []task.VerificationStep{step("cat", "only-here.txt")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Errorf("a command reading a worktree file failed, so it did not run there: %s", report.Runs[0].Stderr)
	}
}

// Reaching the runner with no steps means a bug: the domain refuses such a task
// and so does the database. Returning a pass for zero checks would be the worst
// possible response.
func TestNoStepsIsAnErrorNotAPass(t *testing.T) {
	report, err := runner().Run(context.Background(), Request{
		AttemptID:  uuid.Must(uuid.NewV7()),
		WorkingDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("an empty step list was accepted")
	}
	if report.Passed {
		t.Fatal("Passed = true with no steps run")
	}
}

func TestMissingWorkingDirIsAnError(t *testing.T) {
	if _, err := runner().Run(context.Background(), Request{Steps: []task.VerificationStep{step("true")}}); err == nil {
		t.Error("a request with no working directory was accepted")
	}
}

func TestOutputIsBounded(t *testing.T) {
	r := NewRunner(20*time.Second, 512)
	report, err := r.Run(context.Background(), request(t, step("sh", "-c", "printf 'x%.0s' $(seq 1 4000)")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Errorf("exceeding the output cap must not fail the check: %s", report.Summary())
	}
	if len(report.Runs[0].Stdout) != 512 || !report.Runs[0].StdoutTruncated {
		t.Errorf("stdout length = %d truncated = %v, want 512 and true",
			len(report.Runs[0].Stdout), report.Runs[0].StdoutTruncated)
	}
}

// The command recorded is the rendered argv, which is what an operator reads in
// the result. It is never handed back to a shell.
func TestRecordedCommandIsReadable(t *testing.T) {
	report, err := runner().Run(context.Background(), request(t, step("sh", "-c", "exit 0")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := report.Runs[0].Command; got != `sh -c 'exit 0'` {
		t.Errorf("command = %q, want the rendered argv", got)
	}
}
