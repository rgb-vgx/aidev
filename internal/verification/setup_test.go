package verification

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"aidev/internal/task"
)

// Setup and verification are one pass: setup first, a single step_index
// sequence across both phases, and the phase recorded so a reader can tell
// which half a step belonged to without re-parsing the command.
func TestSetupRunsBeforeVerificationInOneIndexSequence(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "prepared")

	req := Request{
		AttemptID:  uuid.Must(uuid.NewV7()),
		WorkingDir: dir,
		SetupSteps: []task.VerificationStep{
			step("sh", "-c", "echo prepared > "+marker),
		},
		Steps: []task.VerificationStep{
			step("sh", "-c", "test -f "+marker),
			step("sh", "-c", "exit 0"),
		},
	}

	report, err := runner().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("Passed = false, want true: %s", report.Summary())
	}
	if len(report.Runs) != 3 {
		t.Fatalf("got %d runs, want setup + two verify steps", len(report.Runs))
	}

	wantPhase := []task.VerificationPhase{task.PhaseSetup, task.PhaseVerify, task.PhaseVerify}
	for i, run := range report.Runs {
		if run.StepIndex != i {
			t.Errorf("step %d recorded index %d, want one sequence across both phases", i, run.StepIndex)
		}
		if run.Phase != wantPhase[i] {
			t.Errorf("step %d phase = %q, want %q", i, run.Phase, wantPhase[i])
		}
		if run.Status != task.VerificationPassed {
			t.Errorf("step %d status = %s, want PASSED", i, run.Status)
		}
	}

	// The verify step really did see what setup produced: if setup ran after,
	// or not at all, test -f would have failed the pass.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("setup did not run in the working directory: %v", err)
	}
}

// A failing setup stops the pass the same way a failing check does: the
// steps after it are recorded, never executed — running checks against an
// unprepared tree would judge something nobody meant — and they keep their
// own phase so the record still reads correctly.
func TestFailingSetupStopsVerificationButRecordsIt(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "verified")

	req := Request{
		AttemptID:  uuid.Must(uuid.NewV7()),
		WorkingDir: dir,
		SetupSteps: []task.VerificationStep{
			step("sh", "-c", "exit 1"),
		},
		Steps: []task.VerificationStep{
			step("sh", "-c", "touch "+marker),
		},
	}

	report, err := runner().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("Passed = true despite a failing setup step")
	}
	if len(report.Runs) != 2 {
		t.Fatalf("got %d runs, want both declared steps accounted for", len(report.Runs))
	}

	if report.Runs[0].Status != task.VerificationFailed {
		t.Errorf("setup status = %s, want FAILED", report.Runs[0].Status)
	}
	if report.Runs[0].Phase != task.PhaseSetup {
		t.Errorf("setup phase = %q, want %q", report.Runs[0].Phase, task.PhaseSetup)
	}

	if report.Runs[1].Status != task.VerificationSkipped {
		t.Errorf("verify status = %s, want SKIPPED", report.Runs[1].Status)
	}
	if report.Runs[1].Phase != task.PhaseVerify {
		t.Errorf("skipped verify phase = %q, want %q", report.Runs[1].Phase, task.PhaseVerify)
	}
	if report.Runs[1].ExitCode != nil {
		t.Errorf("skipped step exit code = %v, want nil: a skip must never read as a pass", report.Runs[1].ExitCode)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Error("verification ran after the setup failed")
	}

	failure, ok := report.FirstFailure()
	if !ok {
		t.Fatal("FirstFailure found nothing on a failing pass")
	}
	if failure.StepIndex != 0 || failure.Phase != task.PhaseSetup {
		t.Errorf("FirstFailure = step %d phase %q, want the failing setup step", failure.StepIndex, failure.Phase)
	}
	if report.FailureKind != task.FailureVerification {
		t.Errorf("failure kind = %s, want VERIFICATION", report.FailureKind)
	}
}
