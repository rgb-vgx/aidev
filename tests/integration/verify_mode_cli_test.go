package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// B2 on the command line: --setup prepares the checkout, --verify-mode
// chooses where the checks run, and `task get` shows both — a setting that
// only exists at creation is invisible to whoever reviews the task later.
func TestTaskCreateTakesSetupAndVerifyMode(t *testing.T) {
	h := newHarness(t, nil)

	stdout, stderr, err := h.runCLI(t, "task", "create",
		"--repo", h.repoPath,
		"--title", "Create the marker file",
		"--verify", "test -f marker.txt",
		"--setup", "sh -c echo prepared",
		"--verify-mode", "clean",
		"--json")
	if err != nil {
		t.Fatalf("task create --setup --verify-mode: %v\nstderr: %s", err, stderr)
	}

	var created struct {
		Ref              string   `json:"ref"`
		SetupSteps       []string `json:"setup_steps"`
		VerificationMode string   `json:"verification_mode"`
	}
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("the created task is not JSON: %v\n%s", err, stdout)
	}
	if len(created.SetupSteps) != 1 || created.SetupSteps[0] != "sh -c echo prepared" {
		t.Errorf("setup_steps = %v, want the command from the flag", created.SetupSteps)
	}
	if created.VerificationMode != "clean" {
		t.Errorf("verification_mode = %q, want clean from the flag", created.VerificationMode)
	}

	shown, _, err := h.runCLI(t, "task", "get", created.Ref)
	if err != nil {
		t.Fatalf("task get: %v", err)
	}
	if !strings.Contains(shown, "clean") {
		t.Errorf("task get does not show where verification runs:\n%s", shown)
	}
}

// The help must name both flags, or --setup in particular is undiscoverable:
// nothing else in task create prepares the checkout.
func TestTaskCreateHelpNamesSetupAndVerifyMode(t *testing.T) {
	h := newHarness(t, nil)
	_, stderr, _ := h.runCLI(t, "task", "create", "-h")
	// Go's flag package prints a single dash; accept either spelling so the
	// assertion is about the flag existing, not about its typography.
	for _, want := range []string{"-setup", "-verify-mode"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("task create help does not mention %s:\n%s", want, stderr)
		}
	}
}

// `project verify-mode` is where the template for new tasks is set. Like
// approval it prints the current state with no argument — the common
// question is "where do this project's tasks verify?" — and setting it must
// survive to the project list, which is where an operator sees every
// repository at once.
func TestProjectVerifyModeShowSetAndList(t *testing.T) {
	h := newHarness(t, nil)

	// The project does not exist until its first task registers it.
	if _, stderr, err := h.runCLI(t, "project", "verify-mode", "--repo", h.repoPath); err == nil {
		t.Errorf("verify-mode on an unknown repository succeeded; stderr: %s", stderr)
	}

	h.createTask(nil)

	stdout, stderr, err := h.runCLI(t, "project", "verify-mode", "--repo", h.repoPath)
	if err != nil {
		t.Fatalf("project verify-mode: %v\nstderr: %s", err, stderr)
	}
	if strings.TrimSpace(stdout) != "in_place" {
		t.Errorf("initial verify-mode = %q, want in_place", strings.TrimSpace(stdout))
	}

	stdout, stderr, err = h.runCLI(t, "project", "verify-mode", "--repo", h.repoPath, "clean")
	if err != nil {
		t.Fatalf("project verify-mode clean: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "verify-mode clean") {
		t.Errorf("set output = %q, want it to confirm the new mode", stdout)
	}
	// The output must say the change applies to new tasks only: a reader
	// who believes existing tasks moved would misread their results.
	if !strings.Contains(stdout, "already created") {
		t.Errorf("set output does not say existing tasks keep their mode:\n%s", stdout)
	}

	stdout, stderr, err = h.runCLI(t, "project", "list")
	if err != nil {
		t.Fatalf("project list: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "verify clean") {
		t.Errorf("project list does not show the verify mode:\n%s", stdout)
	}

	// An unknown mode is refused without changing anything.
	if _, _, err := h.runCLI(t, "project", "verify-mode", "--repo", h.repoPath, "dirty"); err == nil {
		t.Error("an unknown verify mode was accepted")
	}
	stdout, _, err = h.runCLI(t, "project", "verify-mode", "--repo", h.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "clean" {
		t.Errorf("verify-mode after a rejected value = %q, want clean to be unchanged", strings.TrimSpace(stdout))
	}
}
