package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"aidev/internal/event"
	"aidev/internal/store"
	"aidev/internal/task"
)

// Two questions an operator asks and aidev could not answer:
//
//  1. which repository is this task for — the list showed ref, status, agent and
//     title but never the project;
//  2. has this task's verified result been taken — the fact is in the event log
//     (task.applied / task.apply_undone) and nothing read it back out, so a
//     SUCCEEDED task waiting to be applied looked like every other one.
//
// The state is derived from the events, not stored twice: the newest of the two
// event types decides, and a task with neither is "never applied". Three states,
// not a boolean: "never applied" and "applied and then undone" are different
// things to an operator.

// seedApply records an apply or an undo the way the commands do, so the tests
// read the same events production writes.
func seedApply(t *testing.T, h *harness, tk task.Task, into, commit string) {
	t.Helper()
	e, err := event.New(tk.ID, nil, event.TypeTaskApplied, map[string]any{
		"into": into, "branch": "aidev/" + tk.Ref, "commit": commit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AppendEvent(h.ctx, e); err != nil {
		t.Fatal(err)
	}
}

func seedUndo(t *testing.T, h *harness, tk task.Task, into, commit, undid string) {
	t.Helper()
	e, err := event.New(tk.ID, nil, event.TypeTaskApplyUndone, map[string]any{
		"into": into, "commit": commit, "undid": undid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AppendEvent(h.ctx, e); err != nil {
		t.Fatal(err)
	}
}

func itemFor(t *testing.T, items []store.TaskListItem, ref string) store.TaskListItem {
	t.Helper()
	for _, it := range items {
		if it.Task.Ref == ref {
			return it
		}
	}
	t.Fatalf("%s is not in the list (%d items)", ref, len(items))
	return store.TaskListItem{}
}

// The list carries the repository and the apply state of every task.
func TestListTasksCarriesTheRepositoryAndTheApplyState(t *testing.T) {
	h := newHarness(t, nil)
	never := h.createTask(nil)
	applied := h.createTask(nil)
	undone := h.createTask(nil)
	seedApply(t, h, applied, "main", "aaaa1111")
	seedApply(t, h, undone, "main", "bbbb2222")
	seedUndo(t, h, undone, "main", "cccc3333", "bbbb2222")

	items, err := h.store.ListTasks(h.ctx, store.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("%d items, want 3", len(items))
	}
	for _, it := range items {
		if it.RepoPath != h.repoPath {
			t.Errorf("%s: repo_path = %q, want %q", it.Task.Ref, it.RepoPath, h.repoPath)
		}
	}

	if got := itemFor(t, items, never.Ref).Apply; got.State != task.ApplyNever {
		t.Errorf("a task with no apply event = %+v, want state %q", got, task.ApplyNever)
	}
	if got := itemFor(t, items, applied.Ref).Apply; got.State != task.ApplyApplied || got.Into != "main" || got.Commit != "aaaa1111" {
		t.Errorf("an applied task = %+v, want applied into main at aaaa1111", got)
	}
	if got := itemFor(t, items, undone.Ref).Apply; got.State != task.ApplyUndone || got.Commit != "cccc3333" {
		t.Errorf("an undone task = %+v, want undone with the revert commit cccc3333", got)
	}
}

// The newest of the two events decides, in both orders: applying again after an
// undo makes the task applied once more.
func TestApplyStateFollowsTheNewestEvent(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.createTask(nil)

	if got, err := h.store.ApplyState(h.ctx, tk.ID); err != nil {
		t.Fatalf("ApplyState: %v", err)
	} else if got.State != task.ApplyNever {
		t.Errorf("state = %q, want %q before anything was applied", got.State, task.ApplyNever)
	}

	seedApply(t, h, tk, "main", "first")
	if got, _ := h.store.ApplyState(h.ctx, tk.ID); got.State != task.ApplyApplied || got.Commit != "first" {
		t.Errorf("after an apply = %+v, want applied at first", got)
	}
	seedUndo(t, h, tk, "main", "revert", "first")
	if got, _ := h.store.ApplyState(h.ctx, tk.ID); got.State != task.ApplyUndone || got.Into != "main" {
		t.Errorf("after an undo = %+v, want undone into main", got)
	}
	seedApply(t, h, tk, "main", "second")
	got, _ := h.store.ApplyState(h.ctx, tk.ID)
	if got.State != task.ApplyApplied || got.Commit != "second" {
		t.Errorf("after re-applying = %+v, want applied at second", got)
	}
	if got.At.IsZero() {
		t.Error("the state carries no time; an operator cannot tell when it happened")
	}
}

// The filter an operator actually wants: succeeded, and not in their branch.
func TestListTasksUnapplied(t *testing.T) {
	h := newHarness(t, nil)
	never := h.createTask(nil)
	applied := h.createTask(nil)
	undone := h.createTask(nil)
	failed := h.createTask(nil)
	// The filter is about SUCCEEDED tasks, so run the three that must stay
	// out of a terminal state to SUCCEEDED first; failed goes to FAILED below.
	h.backend.Work = doTheWork
	for _, ref := range []string{never.Ref, applied.Ref, undone.Ref} {
		if out, err := h.orchestrator.RunTask(h.ctx, ref); err != nil || out.Task.Status != task.StatusSucceeded {
			t.Fatalf("RunTask: %v, %v", out.Task.Status, err)
		}
	}
	seedApply(t, h, applied, "main", "aaaa")
	seedApply(t, h, undone, "main", "bbbb")
	seedUndo(t, h, undone, "main", "cccc", "bbbb")
	finish(t, h.ctx, h.store, failed)

	items, err := h.store.ListTasks(h.ctx, store.TaskFilter{Unapplied: true})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	refs := make([]string, 0, len(items))
	for _, it := range items {
		refs = append(refs, it.Task.Ref)
		if it.Task.Status != task.StatusSucceeded {
			t.Errorf("%s is %s in an unapplied listing, want only SUCCEEDED tasks", it.Task.Ref, it.Task.Status)
		}
	}
	joined := strings.Join(refs, ",")
	if !strings.Contains(joined, never.Ref) {
		t.Errorf("unapplied = %v, want the never-applied task %s", refs, never.Ref)
	}
	if !strings.Contains(joined, undone.Ref) {
		t.Errorf("unapplied = %v, want the undone task %s: its work is not in the branch", refs, undone.Ref)
	}
	for _, unwanted := range []string{applied.Ref, failed.Ref} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("unapplied = %v, want neither the applied task %s nor the failed one %s", refs, applied.Ref, failed.Ref)
		}
	}
}

// The CLI list shows the repository and the apply state, and filters by it.
func TestTaskListShowsRepositoryAndApplyState(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	applied := h.createTask(nil)
	seedApply(t, h, applied, "main", "aaaa1111bbbb2222")
	if out, err := h.orchestrator.RunTask(h.ctx, applied.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask: %v, %v", out.Task.Status, err)
	}
	waiting := h.createTask(nil)
	if out, err := h.orchestrator.RunTask(h.ctx, waiting.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask: %v, %v", out.Task.Status, err)
	}

	stdout, stderr, err := h.runCLI(t, "task", "list", "--repo", h.repoPath)
	if err != nil {
		t.Fatalf("task list: %v\n%s", err, stderr)
	}
	repoName := filepathBase(h.repoPath)
	if !strings.Contains(stdout, repoName) {
		t.Errorf("list output does not name the repository:\n%s", stdout)
	}
	for _, want := range []string{applied.Ref, "applied", "main"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list output lacks %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, err = h.runCLI(t, "task", "list", "--unapplied")
	if err != nil {
		t.Fatalf("task list --unapplied: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, waiting.Ref) {
		t.Errorf("--unapplied output lacks %s:\n%s", waiting.Ref, stdout)
	}
	if strings.Contains(stdout, applied.Ref) {
		t.Errorf("--unapplied output lists the applied task %s:\n%s", applied.Ref, stdout)
	}

	// --unapplied already fixes the status, so combining them is a mistake.
	if _, _, err := h.runCLI(t, "task", "list", "--unapplied", "--status", "FAILED"); err == nil {
		t.Error("--unapplied with --status was accepted; they contradict each other")
	}
}

// The JSON shapes are what scripts read.
func TestTaskListJSONCarriesTheApplyState(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	created := h.createTask(nil)
	if out, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask: %v, %v", out.Task.Status, err)
	}
	seedApply(t, h, created, "main", "aaaa1111bbbb2222")

	stdout, stderr, err := h.runCLI(t, "task", "list", "--json")
	if err != nil {
		t.Fatalf("task list --json: %v\n%s", err, stderr)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if len(items) != 1 {
		t.Fatalf("%d items, want 1", len(items))
	}
	item := items[0]
	if item["repo_path"] != h.repoPath {
		t.Errorf("repo_path = %v, want %q", item["repo_path"], h.repoPath)
	}
	if item["apply_state"] != "applied" {
		t.Errorf("apply_state = %v, want applied", item["apply_state"])
	}
	if item["apply_into"] != "main" || item["apply_commit"] != "aaaa1111bbbb2222" {
		t.Errorf("apply_into/apply_commit = %v/%v, want main/aaaa1111bbbb2222", item["apply_into"], item["apply_commit"])
	}

	// A task that was never applied carries no apply keys at all.
	other := h.createTask(nil)
	stdout, _, err = h.runCLI(t, "task", "list", "--json", "--status", "PENDING")
	if err != nil {
		t.Fatalf("task list --json --status PENDING: %v", err)
	}
	items = nil
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["apply_state"] != nil {
		t.Errorf("a task that was never applied = %v (want no apply_state); task %s", items, other.Ref)
	}
}

// A single task says the same thing, on every surface.
func TestTaskGetAndResultShowTheApplyState(t *testing.T) {
	h := newHarness(t, nil)
	h.backend.Work = doTheWork
	created := h.createTask(nil)
	if out, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil || out.Task.Status != task.StatusSucceeded {
		t.Fatalf("RunTask: %v, %v", out.Task.Status, err)
	}

	// Before: nothing claims it.
	getJSON, _, err := h.runCLI(t, "task", "get", created.Ref, "--json")
	if err != nil {
		t.Fatalf("task get --json: %v", err)
	}
	if strings.Contains(getJSON, "apply_state") {
		t.Errorf("an unapplied task reports an apply state:\n%s", getJSON)
	}
	if resultOut, _, err := h.runCLI(t, "task", "result", created.Ref); err != nil {
		t.Fatalf("task result: %v", err)
	} else if strings.Contains(resultOut, "applied") {
		t.Errorf("an unapplied task's result says applied:\n%s", resultOut)
	}

	if _, stderr, err := h.runCLI(t, "task", "apply", created.Ref); err != nil {
		t.Fatalf("task apply: %v\n%s", err, stderr)
	}

	getJSON, _, err = h.runCLI(t, "task", "get", created.Ref, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(getJSON), &got); err != nil {
		t.Fatal(err)
	}
	if got["apply_state"] != "applied" || got["apply_into"] != "main" {
		t.Errorf("task get after apply = %v/%v, want applied/main", got["apply_state"], got["apply_into"])
	}

	resultText, _, err := h.runCLI(t, "task", "result", created.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText, "applied to main") {
		t.Errorf("task result does not say where the work went:\n%s", resultText)
	}

	resultJSON, _, err := h.runCLI(t, "task", "result", created.Ref, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Apply *struct {
			State  string `json:"state"`
			Into   string `json:"into"`
			Commit string `json:"commit"`
		} `json:"apply"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result.Apply == nil || result.Apply.State != "applied" || result.Apply.Into != "main" || result.Apply.Commit == "" {
		t.Errorf("result.apply = %+v, want the applied record", result.Apply)
	}

	// Undo it: the state says undone, and the result keeps what it undid.
	if _, stderr, err := h.runCLI(t, "task", "undo", created.Ref); err != nil {
		t.Fatalf("task undo: %v\n%s", err, stderr)
	}
	resultJSON, _, err = h.runCLI(t, "task", "result", created.Ref, "--json")
	if err != nil {
		t.Fatal(err)
	}
	result.Apply = nil
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result.Apply == nil || result.Apply.State != "undone" {
		t.Fatalf("result.apply after undo = %+v, want state undone", result.Apply)
	}
	if !strings.Contains(resultJSON, `"undid"`) {
		t.Errorf("the undone record does not say what it reverted:\n%s", resultJSON)
	}
}

// MCP: the same list and result, with a filter that says what it means.
func TestMCPListAndResultCarryTheApplyState(t *testing.T) {
	m := newMCPHarness(t)
	created := succeededTask(t, m.harness)
	seedApply(t, m.harness, created, "main", "aaaa1111bbbb2222")

	var listed struct {
		Tasks []map[string]any `json:"tasks"`
		Count int              `json:"count"`
	}
	m.call(t, "aidev_list_tasks", map[string]any{"repo_path": m.repoPath}, &listed)
	if listed.Count != 1 {
		t.Fatalf("count = %d, want 1", listed.Count)
	}
	if listed.Tasks[0]["repo_path"] != m.repoPath || listed.Tasks[0]["apply_state"] != "applied" {
		t.Errorf("list item = %v, want repo_path and apply_state applied", listed.Tasks[0])
	}

	var waiting struct {
		Count int `json:"count"`
	}
	m.call(t, "aidev_list_tasks", map[string]any{"needs_apply": true}, &waiting)
	if waiting.Count != 0 {
		t.Errorf("needs_apply = %d tasks, want 0: the only succeeded task is applied", waiting.Count)
	}
	other := succeededTask(t, m.harness)
	// created is already applied (seeded above), so a real `task apply` of it
	// would be refused as "already applied"; other's untouched result is what
	// needs applying.
	m.call(t, "aidev_list_tasks", map[string]any{"needs_apply": true}, &waiting)
	if waiting.Count != 1 {
		t.Errorf("needs_apply = %d tasks, want 1 (%s)", waiting.Count, other.Ref)
	}

	var result struct {
		Result struct {
			Apply *struct {
				State string `json:"state"`
				Into  string `json:"into"`
			} `json:"apply"`
		} `json:"result"`
	}
	m.call(t, "aidev_get_task_result", map[string]any{"task": created.Ref}, &result)
	if result.Result.Apply == nil || result.Result.Apply.State != "applied" || result.Result.Apply.Into != "main" {
		t.Errorf("result.apply = %+v, want the applied record", result.Result.Apply)
	}
}

// filepathBase is the repository's name as a person writes it in a listing.
func filepathBase(path string) string {
	path = strings.TrimSuffix(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
