package integration

import (
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/task"
	"aidev/internal/worker"
)

// Verification refuses to run a runner that changed during the attempt: the
// judge the agent wrote is not independent evidence (research 7e). A step
// that runs a file the base does not have can therefore never be judged —
// whatever the agent does, creating that file is the change that stops the
// check. TASK-000159 found out after its agent had worked for six and a half
// minutes: `python3 scripts/generate_ribbon_menus.py --check` named the script
// the task asked for, and the attempt failed with "verification not run".
//
// That outcome is decidable when the task is created, from the base alone,
// so CreateTask refuses such a step and says which file it is.

func missingRunnerInput(h *harness, steps ...task.VerificationStep) worker.CreateTaskInput {
	return worker.CreateTaskInput{
		RepoPath:           h.repoPath,
		Title:              "Write the generator",
		Description:        "Write scripts/gen.py",
		AcceptanceCriteria: "scripts/gen.py --check passes",
		Verification:       steps,
	}
}

func TestCreateRefusesAStepThatRunsAFileTheBaseLacks(t *testing.T) {
	h := newHarness(t, nil)
	for name, step := range map[string]task.VerificationStep{
		"python script":   {Command: "python3", Args: []string{"scripts/gen.py", "--check"}},
		"shell script":    {Command: "sh", Args: []string{"scripts/gen.sh"}},
		"relative binary": {Command: "./scripts/gen.sh"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.orchestrator.CreateTask(h.ctx, missingRunnerInput(h,
				task.VerificationStep{Command: "test", Args: []string{"-f", "main.go"}}, step))
			if err == nil {
				t.Fatalf("CreateTask accepted %q, which runs a file the base does not have", step.String())
			}
			msg := err.Error()
			wantPath := strings.TrimPrefix(firstPathArg(step), "./")
			if !strings.Contains(msg, wantPath) {
				t.Errorf("error %q does not name %s", msg, wantPath)
			}
			if !strings.Contains(msg, "step 2") {
				t.Errorf("error %q does not say which step (step 2)", msg)
			}
		})
	}
}

// A runner the base already has is the ordinary case; so is a test runner
// whose own files exist (go test, node --test on a committed spec).
func TestCreateAcceptsStepsWhoseRunnersExist(t *testing.T) {
	h := newHarness(t, nil)
	writeFile(t, filepath.Join(h.repoPath, "scripts", "gen.py"), "print('ok')\n")
	gitRun(t, h.repoPath, "add", "-A")
	gitRun(t, h.repoPath, "commit", "-qm", "add the generator")

	if _, err := h.orchestrator.CreateTask(h.ctx, missingRunnerInput(h,
		task.VerificationStep{Command: "python3", Args: []string{"scripts/gen.py", "--check"}},
		task.VerificationStep{Command: "go", Args: []string{"test", "./..."}},
		task.VerificationStep{Command: "test", Args: []string{"-f", "marker.txt"}},
	)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

// The runner is looked up at the task's base ref, not in the operator's
// checkout: a script committed on the spec branch counts, one only in the
// working tree does not.
func TestCreateLooksForTheRunnerAtTheBaseRef(t *testing.T) {
	h := newHarness(t, nil)
	gitRun(t, h.repoPath, "switch", "-qc", "spec/gen")
	writeFile(t, filepath.Join(h.repoPath, "scripts", "gen.py"), "print('ok')\n")
	gitRun(t, h.repoPath, "add", "-A")
	gitRun(t, h.repoPath, "commit", "-qm", "spec")
	gitRun(t, h.repoPath, "switch", "-q", "main")

	in := missingRunnerInput(h, task.VerificationStep{Command: "python3", Args: []string{"scripts/gen.py"}})
	in.BaseRef = "spec/gen"
	if _, err := h.orchestrator.CreateTask(h.ctx, in); err != nil {
		t.Fatalf("CreateTask with the script on its base ref: %v", err)
	}
	in.BaseRef = "main"
	if _, err := h.orchestrator.CreateTask(h.ctx, in); err == nil {
		t.Fatal("CreateTask accepted a step whose script exists only on another branch")
	}
}

func firstPathArg(step task.VerificationStep) string {
	if strings.HasPrefix(step.Command, "./") {
		return step.Command
	}
	return step.Args[0]
}
