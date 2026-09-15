package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// The CLI is where a person routes a task to a model: "this one is hard, run it on
// the strong model", or "muse is down today, run this on mimo". Without the flag the
// only way to change the model is editing conf.json, which changes it for everything.
func TestTaskCreateTakesAModel(t *testing.T) {
	h := newHarness(t, nil)

	stdout, stderr, err := h.runCLI(t, "task", "create",
		"--repo", h.repoPath,
		"--title", "Create the marker file",
		"--verify", "test -f marker.txt",
		"--model", "opencode/mimo-v2.5-free",
		"--json")
	if err != nil {
		t.Fatalf("task create --model: %v\nstderr: %s", err, stderr)
	}

	var created struct {
		Ref   string `json:"ref"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("the created task is not JSON: %v\n%s", err, stdout)
	}
	if created.Model != "opencode/mimo-v2.5-free" {
		t.Errorf("model = %q, want the one asked for on the command line", created.Model)
	}

	shown, _, err := h.runCLI(t, "task", "get", created.Ref)
	if err != nil {
		t.Fatalf("task get: %v", err)
	}
	if !strings.Contains(shown, "opencode/mimo-v2.5-free") {
		t.Errorf("task get does not show which model the task will run on:\n%s", shown)
	}
}

// The help has to name the setting the default comes from, the way the other
// configuration-backed flags do, or nobody can find where to change it.
func TestTaskCreateHelpNamesTheModelSetting(t *testing.T) {
	h := newHarness(t, nil)
	_, stderr, _ := h.runCLI(t, "task", "create", "-h")
	if !strings.Contains(stderr, "--model") && !strings.Contains(stderr, "-model") {
		t.Errorf("task create has no --model flag:\n%s", stderr)
	}
	if !strings.Contains(stderr, "agent.opencode.model") {
		t.Errorf("the help does not name the setting the default comes from:\n%s", stderr)
	}
}

// Hardness is stated by the person creating the task, so it lives on the command line
// next to --model, and `task get` shows it: a routed model that nobody can see the
// reason for is worse than no routing.
func TestTaskCreateTakesAHardness(t *testing.T) {
	h := newHarness(t, nil)

	stdout, stderr, err := h.runCLI(t, "task", "create",
		"--repo", h.repoPath,
		"--title", "Create the marker file",
		"--verify", "test -f marker.txt",
		"--hardness", "hard",
		"--json")
	if err != nil {
		t.Fatalf("task create --hardness: %v\nstderr: %s", err, stderr)
	}
	var created struct {
		Ref      string `json:"ref"`
		Hardness string `json:"hardness"`
	}
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("the created task is not JSON: %v\n%s", err, stdout)
	}
	if created.Hardness != "HARD" {
		t.Errorf("hardness = %q, want the parsed level", created.Hardness)
	}

	shown, _, err := h.runCLI(t, "task", "get", created.Ref)
	if err != nil {
		t.Fatalf("task get: %v", err)
	}
	if !strings.Contains(shown, "HARD") {
		t.Errorf("task get does not show the hardness:\n%s", shown)
	}

	_, stderr, err = h.runCLI(t, "task", "create",
		"--repo", h.repoPath, "--title", "x", "--verify", "test -f marker.txt",
		"--hardness", "impossible")
	if err == nil {
		t.Error("an invented hardness was accepted")
	} else if !strings.Contains(err.Error()+stderr, "impossible") {
		t.Errorf("error = %v, want it to quote what was given", err)
	}
}
