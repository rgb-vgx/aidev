package integration

import (
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/task"
	"aidev/internal/worker"
)

// Clean verification checks out exactly what would be committed, so nothing
// git ignores is there — node_modules included. A step that runs node or npm
// in a repository with a lockfile then fails unless a setup step installs the
// dependencies first. Axiom-Office could not move to clean verification on
// 2026-11-01 because tasks such as TASK-000159 ran `node --test` with no
// `npm ci`; in place that worked only because the agent had installed them.
//
// CreateTask still creates such a task — a script may need no package — but
// says so where the planner sees it: in the create result and on
// task.created.

func nodeRepo(t *testing.T, h *harness) {
	t.Helper()
	writeFile(t, filepath.Join(h.repoPath, "package.json"), `{"name":"probe","version":"1.0.0"}`+"\n")
	writeFile(t, filepath.Join(h.repoPath, "package-lock.json"), `{"name":"probe","lockfileVersion":3}`+"\n")
	writeFile(t, filepath.Join(h.repoPath, "check.test.mjs"), "import test from 'node:test';\ntest('ok', () => {});\n")
	gitRun(t, h.repoPath, "add", "-A")
	gitRun(t, h.repoPath, "commit", "-qm", "node project")
}

func createdWarnings(t *testing.T, h *harness, created task.Task) []string {
	t.Helper()
	payload := h.eventPayload(created.ID, "task.created")
	raw, _ := payload["warnings"].([]any)
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		s, _ := w.(string)
		out = append(out, s)
	}
	return out
}

func TestCleanTaskRunningNodeWithoutInstallIsWarned(t *testing.T) {
	h := newHarness(t, nil)
	nodeRepo(t, h)
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "node", Args: []string{"--test", "check.test.mjs"}}}
		in.VerificationMode = "clean"
	})
	warnings := createdWarnings(t, h, created)
	if len(warnings) != 1 {
		t.Fatalf("task.created warnings = %v, want one", warnings)
	}
	for _, want := range []string{"package-lock.json", "node_modules", "npm ci"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning %q does not mention %q", warnings[0], want)
		}
	}
}

func TestNoWarningWhenASetupStepInstalls(t *testing.T) {
	for name, setup := range map[string]task.VerificationStep{
		"npm ci":       {Command: "npm", Args: []string{"ci", "--offline"}},
		"npm install":  {Command: "npm", Args: []string{"install"}},
		"pnpm install": {Command: "pnpm", Args: []string{"install", "--frozen-lockfile"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			nodeRepo(t, h)
			created := h.createTask(func(in *worker.CreateTaskInput) {
				in.SetupSteps = []task.VerificationStep{setup}
				in.Verification = []task.VerificationStep{{Command: "node", Args: []string{"--test", "check.test.mjs"}}}
				in.VerificationMode = "clean"
			})
			if w := createdWarnings(t, h, created); len(w) != 0 {
				t.Errorf("warnings = %v, want none with %s as setup", w, name)
			}
		})
	}
}

// In place the agent's worktree is where the checks run, and an agent that
// ran the tests has usually installed the packages there; no warning.
// Neither is there one without a lockfile, or for a step that runs no node.
func TestNoWarningInPlaceOrWithoutALockfileOrNode(t *testing.T) {
	h := newHarness(t, nil)
	nodeRepo(t, h)
	inPlace := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "node", Args: []string{"--test", "check.test.mjs"}}}
		in.VerificationMode = "in_place"
	})
	if w := createdWarnings(t, h, inPlace); len(w) != 0 {
		t.Errorf("in_place warnings = %v, want none", w)
	}
	noNode := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "test", Args: []string{"-f", "package.json"}}}
		in.VerificationMode = "clean"
	})
	if w := createdWarnings(t, h, noNode); len(w) != 0 {
		t.Errorf("warnings for a step that runs no node = %v, want none", w)
	}

	plain := newHarness(t, nil) // main.go only, no lockfile
	writeFile(t, filepath.Join(plain.repoPath, "check.test.mjs"), "import test from 'node:test';\ntest('ok', () => {});\n")
	gitRun(t, plain.repoPath, "add", "-A")
	gitRun(t, plain.repoPath, "commit", "-qm", "a script, no packages")
	noLock := plain.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "node", Args: []string{"--test", "check.test.mjs"}}}
		in.VerificationMode = "clean"
	})
	if w := createdWarnings(t, plain, noLock); len(w) != 0 {
		t.Errorf("warnings without a lockfile = %v, want none", w)
	}
}

// The planner sees the warning in the create result, not only in the history.
func TestMCPCreateResultCarriesTheWarning(t *testing.T) {
	m := newMCPHarness(t)
	nodeRepo(t, m.harness)
	var created struct {
		Warnings []string `json:"warnings"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":         m.repoPath,
		"title":             "Run the node checks",
		"description":       "Make check.test.mjs pass",
		"verification":      []string{"node --test check.test.mjs"},
		"verification_mode": "clean",
	}, &created)
	if len(created.Warnings) != 1 || !strings.Contains(created.Warnings[0], "npm ci") {
		t.Errorf("create result warnings = %v, want the missing-install warning", created.Warnings)
	}
}
