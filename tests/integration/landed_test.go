package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/agent"
	"aidev/internal/task"
	"aidev/internal/worker"
)

// A planner often takes a task's work with plain git — a cherry-pick, a
// rebase, a merge — rather than with `aidev task apply`. aidev's apply state
// came only from apply events, so such a task stayed "waiting to be applied"
// forever: in Axiom-Office on 2026-10-11, four of the six tasks `task list
// --unapplied` named were already on main. A SUCCEEDED task with no apply
// event whose every commit already has an equivalent on the repository's
// default branch (or is an ancestor of it) is now "landed".

// succeededWriting runs a task to success whose agent writes one file, so two
// tasks in one test change different things.
func succeededWriting(t *testing.T, h *harness, file string) task.Task {
	t.Helper()
	check := "#!/bin/sh\ntest -f " + file + "\n"
	created := h.createTask(func(in *worker.CreateTaskInput) {
		in.Verification = []task.VerificationStep{{Command: "sh", Args: []string{"-c", strings.TrimPrefix(check, "#!/bin/sh\n")}}}
	})
	h.backend.Work = func(_ context.Context, req agent.Request) error {
		return os.WriteFile(filepath.Join(req.WorkingDir, file), []byte(file+"\n"), 0o644)
	}
	out, err := h.orchestrator.RunTask(h.ctx, created.Ref)
	if err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask = %v, %v (%s); want SUCCEEDED", out.Task.Status, err, out.Message)
	}
	return created
}

func listJSON(t *testing.T, h *harness, args ...string) map[string]map[string]any {
	t.Helper()
	out, stderr, err := h.runCLI(t, append([]string{"task", "list", "--json"}, args...)...)
	if err != nil {
		t.Fatalf("task list --json: %v\n%s", err, stderr)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	byRef := map[string]map[string]any{}
	for _, it := range items {
		byRef[it["ref"].(string)] = it
	}
	return byRef
}

func TestACherryPickedTaskIsLanded(t *testing.T) {
	h := newHarness(t, nil)
	picked := succeededWriting(t, h, "picked.txt")
	waiting := succeededWriting(t, h, "waiting.txt")
	gitRun(t, h.repoPath, "cherry-pick", "aidev/"+picked.Ref)

	items := listJSON(t, h)
	if got := items[picked.Ref]; got["apply_state"] != "landed" || got["apply_into"] != "main" {
		t.Errorf("cherry-picked task = %v/%v, want landed into main", got["apply_state"], got["apply_into"])
	}
	if got := items[waiting.Ref]; got["apply_state"] != nil {
		t.Errorf("a task nobody took = %v, want no apply state", got["apply_state"])
	}

	// --unapplied answers "what is still waiting": the landed one is not.
	waitingOnly := listJSON(t, h, "--unapplied")
	if _, ok := waitingOnly[picked.Ref]; ok {
		t.Errorf("--unapplied lists %s, which is already on main", picked.Ref)
	}
	if _, ok := waitingOnly[waiting.Ref]; !ok {
		t.Errorf("--unapplied lost %s, which nobody took", waiting.Ref)
	}

	text, _, err := h.runCLI(t, "task", "list")
	if err != nil || !strings.Contains(text, "[landed in main]") {
		t.Errorf("task list text = %q, %v; want [landed in main]", text, err)
	}
	result, _, err := h.runCLI(t, "task", "result", picked.Ref)
	if err != nil || !strings.Contains(result, "landed in main") {
		t.Errorf("task result = %q, %v; want it to say the work landed in main", result, err)
	}
}

// A real merge by hand puts the task's commit in main's history.
func TestAMergedByHandTaskIsLanded(t *testing.T) {
	h := newHarness(t, nil)
	merged := succeededWriting(t, h, "merged.txt")
	gitRun(t, h.repoPath, "merge", "--no-ff", "-q", "-m", "merge by hand", "aidev/"+merged.Ref)

	if got := listJSON(t, h)[merged.Ref]; got["apply_state"] != "landed" {
		t.Errorf("hand-merged task = %v, want landed", got["apply_state"])
	}
}

// An apply recorded by aidev keeps its own state: "applied" says more than
// "landed" (who, when, which merge commit), and it must not be overwritten.
func TestAnAppliedTaskStaysApplied(t *testing.T) {
	h := newHarness(t, nil)
	applied := succeededWriting(t, h, "applied.txt")
	if _, stderr, err := h.runCLI(t, "task", "apply", applied.Ref); err != nil {
		t.Fatalf("apply: %v\n%s", err, stderr)
	}
	if got := listJSON(t, h)[applied.Ref]; got["apply_state"] != "applied" {
		t.Errorf("applied task = %v, want applied", got["apply_state"])
	}
}

// MCP's needs_apply asks the same question as --unapplied.
func TestMCPNeedsApplySkipsLandedTasks(t *testing.T) {
	m := newMCPHarness(t)
	picked := succeededWriting(t, m.harness, "picked.txt")
	gitRun(t, m.repoPath, "cherry-pick", "aidev/"+picked.Ref)

	var listed struct {
		Tasks []map[string]any `json:"tasks"`
		Count int              `json:"count"`
	}
	m.call(t, "aidev_list_tasks", map[string]any{"needs_apply": true}, &listed)
	if listed.Count != 0 {
		t.Errorf("needs_apply = %v, want nothing: the only task is on main", listed.Tasks)
	}
	m.call(t, "aidev_list_tasks", map[string]any{}, &listed)
	if listed.Count != 1 || listed.Tasks[0]["apply_state"] != "landed" {
		t.Errorf("list = %v, want the task landed", listed.Tasks)
	}
}
