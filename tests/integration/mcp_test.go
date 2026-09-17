package integration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"aidev/internal/config"
	"aidev/internal/logging"
	aidevmcp "aidev/internal/mcp"
	"aidev/internal/task"
)

// mcpHarness runs the real MCP server over an in-memory transport and talks to it
// with a real MCP client. Only the agent is faked, so the protocol, the schemas,
// the orchestrator, git and PostgreSQL are all exercised for real.
type mcpHarness struct {
	*harness
	session *sdk.ClientSession
}

func newMCPHarness(t *testing.T) *mcpHarness {
	t.Helper()
	return newMCPHarnessWith(t, nil)
}

// newMCPHarnessWith is newMCPHarness over a configuration the test adjusts.
func newMCPHarnessWith(t *testing.T, mutate func(*config.Config)) *mcpHarness {
	t.Helper()

	h := newHarness(t, mutate)
	server := aidevmcp.New(h.orchestrator, h.store, "test", logging.Discard())

	serverTransport, clientTransport := sdk.NewInMemoryTransports()

	serveCtx, cancelServe := context.WithCancel(h.ctx)
	served := make(chan error, 1)
	go func() { served <- server.ServeTransport(serveCtx, serverTransport) }()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(h.ctx, clientTransport, nil)
	if err != nil {
		cancelServe()
		t.Fatalf("connect to the mcp server: %v", err)
	}

	t.Cleanup(func() {
		_ = session.Close()
		cancelServe()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Error("the mcp server did not shut down")
		}
	})

	return &mcpHarness{harness: h, session: session}
}

// call invokes a tool and decodes its structured result.
func (m *mcpHarness) call(t *testing.T, name string, args any, out any) *sdk.CallToolResult {
	t.Helper()

	res, err := m.session.CallTool(m.ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned an error: %s", name, toolErrorText(res))
	}
	if out != nil {
		encoded, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("%s: re-encode structured content: %v", name, err)
		}
		if err := json.Unmarshal(encoded, out); err != nil {
			t.Fatalf("%s: decode structured content: %v\n%s", name, err, encoded)
		}
	}
	return res
}

// callExpectingError invokes a tool that should fail and returns the message.
func (m *mcpHarness) callExpectingError(t *testing.T, name string, args any) string {
	t.Helper()

	res, err := m.session.CallTool(m.ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// A protocol-level error is also an acceptable rejection.
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s succeeded but should have failed: %+v", name, res.StructuredContent)
	}
	return toolErrorText(res)
}

func toolErrorText(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, " ")
}

// Every tool the brief requires must be discoverable, with both an input and an
// output schema: a planner that cannot see the shape of a result cannot use it.
func TestMCPToolsAreDiscoverableWithSchemas(t *testing.T) {
	m := newMCPHarness(t)

	res, err := m.session.ListTools(m.ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	byName := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}

	required := []string{
		"aidev_create_task", "aidev_get_task", "aidev_list_tasks", "aidev_run_task",
		"aidev_cancel_task", "aidev_get_task_result", "aidev_get_task_events",
		"aidev_approve_task",
	}
	for _, name := range required {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("tool %s is not exposed", name)
			continue
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("tool %s has no description", name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("tool %s has no output schema", name)
		}
	}

	// The instructions are where a planner learns that verification decides the
	// outcome, so their absence would be a real defect rather than cosmetic.
	init := m.session.InitializeResult()
	if init == nil || strings.TrimSpace(init.Instructions) == "" {
		t.Fatal("the server sent no instructions")
	}
	for _, want := range []string{"verif", "worktree"} {
		if !strings.Contains(strings.ToLower(init.Instructions), want) {
			t.Errorf("instructions do not mention %q:\n%s", want, init.Instructions)
		}
	}
}

// The schema must mark verification required, because a task aidev cannot check is
// one it will refuse — and a planner should learn that from the schema rather than
// from a rejection.
func TestCreateTaskSchemaRequiresVerification(t *testing.T) {
	m := newMCPHarness(t)

	res, err := m.session.ListTools(m.ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema []byte
	for _, tool := range res.Tools {
		if tool.Name == "aidev_create_task" {
			schema, err = json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if schema == nil {
		t.Fatal("aidev_create_task not found")
	}

	var decoded struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatalf("decode schema: %v\n%s", err, schema)
	}

	required := strings.Join(decoded.Required, ",")
	for _, want := range []string{"repo_path", "title", "verification"} {
		if !strings.Contains(required, want) {
			t.Errorf("%q is not required by the schema; required = %v", want, decoded.Required)
		}
	}
	for _, optional := range []string{"description", "requires_approval", "base_ref"} {
		if _, ok := decoded.Properties[optional]; !ok {
			t.Errorf("schema has no %q property", optional)
		}
		if strings.Contains(required, optional) {
			t.Errorf("%q should be optional", optional)
		}
	}
}

func TestMCPHappyPath(t *testing.T) {
	m := newMCPHarness(t)
	m.backend.Work = doTheWork
	m.backend.Summary = "Created marker.txt"

	var created struct {
		Task     map[string]any `json:"task"`
		NextStep string         `json:"next_step"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Create the marker file",
		"description":  "Create marker.txt containing the word done",
		"verification": []string{"test -f marker.txt"},
	}, &created)

	ref, _ := created.Task["ref"].(string)
	if !strings.HasPrefix(ref, "TASK-") {
		t.Fatalf("created task ref = %q", ref)
	}
	if created.Task["status"] != "PENDING" {
		t.Errorf("status = %v, want PENDING: creating a task must not run it", created.Task["status"])
	}
	if !strings.Contains(created.NextStep, "aidev_run_task") {
		t.Errorf("next_step = %q, want it to name the run tool", created.NextStep)
	}

	var run struct {
		Result       map[string]any `json:"result"`
		StillRunning bool           `json:"still_running"`
		Succeeded    bool           `json:"succeeded"`
		NextStep     string         `json:"next_step"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": ref, "wait_seconds": 60}, &run)

	if run.StillRunning {
		t.Fatal("the fake agent's run did not finish within 60s")
	}
	if !run.Succeeded {
		t.Fatalf("succeeded = false: %v", run.Result["message"])
	}
	resultTask := run.Result["task"].(map[string]any)
	if resultTask["status"] != "SUCCEEDED" {
		t.Errorf("status = %v, want SUCCEEDED", resultTask["status"])
	}

	// The verification section is the evidence, and it must be present.
	verification, ok := run.Result["verification"].([]any)
	if !ok || len(verification) != 1 {
		t.Fatalf("verification = %v, want one entry", run.Result["verification"])
	}
	step := verification[0].(map[string]any)
	if step["status"] != "PASSED" {
		t.Errorf("verification status = %v", step["status"])
	}
	if !strings.Contains(run.NextStep, "branch") {
		t.Errorf("next_step = %q, want it to point at the branch", run.NextStep)
	}

	// The result is readable back through a separate tool, which is what a
	// planner does after a long run.
	var result struct {
		Result       map[string]any `json:"result"`
		StillRunning bool           `json:"still_running"`
		Diff         string         `json:"diff"`
	}
	m.call(t, "aidev_get_task_result", map[string]any{"task": ref}, &result)
	if result.StillRunning {
		t.Error("still_running is true for a finished task")
	}
	if result.Diff != "" {
		t.Error("the diff was included without include_logs being set")
	}

	m.call(t, "aidev_get_task_result", map[string]any{"task": ref, "include_logs": true}, &result)
	if !strings.Contains(result.Diff, "marker.txt") {
		t.Errorf("include_logs did not return the diff: %q", result.Diff)
	}

	var events struct {
		Events  []map[string]any `json:"events"`
		Count   int              `json:"count"`
		LastSeq int64            `json:"last_seq"`
	}
	m.call(t, "aidev_get_task_events", map[string]any{"task": ref}, &events)
	if events.Count == 0 || events.LastSeq == 0 {
		t.Fatalf("events = %+v, want a history", events)
	}
	var names []string
	for _, e := range events.Events {
		names = append(names, e["type"].(string))
	}
	for _, want := range []string{"task.created", "task.started", "task.verification_completed", "task.succeeded"} {
		if !contains(names, want) {
			t.Errorf("history is missing %s: %v", want, names)
		}
	}

	// Resuming from a sequence returns only what is new, which is how a planner
	// follows a long run without re-reading.
	var tail struct {
		Count int `json:"count"`
	}
	m.call(t, "aidev_get_task_events", map[string]any{"task": ref, "after_seq": events.LastSeq}, &tail)
	if tail.Count != 0 {
		t.Errorf("after_seq returned %d events, want none after the last one", tail.Count)
	}
}

// The product's central claim, through the protocol a planner actually uses.
func TestMCPReportsFailureWhenTheAgentOnlyClaimsSuccess(t *testing.T) {
	m := newMCPHarness(t)
	m.backend.Work = nil
	m.backend.Status = task.WorkerSucceeded
	m.backend.Summary = "All done! Tests pass."

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Create the marker file",
		"verification": []string{"test -f marker.txt"},
	}, &created)
	ref := created.Task["ref"].(string)

	var run struct {
		Result    map[string]any `json:"result"`
		Succeeded bool           `json:"succeeded"`
		NextStep  string         `json:"next_step"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": ref, "wait_seconds": 60}, &run)

	if run.Succeeded {
		t.Fatal("succeeded = true for an agent that did nothing")
	}
	resultTask := run.Result["task"].(map[string]any)
	if resultTask["status"] != "FAILED" {
		t.Errorf("status = %v, want FAILED", resultTask["status"])
	}

	// The claim is reported, clearly separated from the evidence.
	workerSection := run.Result["worker"].(map[string]any)
	if workerSection["summary"] != "All done! Tests pass." {
		t.Errorf("the agent's claim was not passed through: %v", workerSection["summary"])
	}
	if workerSection["status"] != "SUCCEEDED" {
		t.Errorf("worker status = %v, want the agent's own run recorded as SUCCEEDED", workerSection["status"])
	}
	verification := run.Result["verification"].([]any)
	step := verification[0].(map[string]any)
	if step["status"] != "FAILED" {
		t.Errorf("verification status = %v, want FAILED", step["status"])
	}
	if step["stderr"] == nil && step["stdout"] == nil {
		t.Log("note: the failing command produced no output, which is expected for `test -f`")
	}
	if !strings.Contains(run.NextStep, "include_logs") {
		t.Errorf("next_step = %q, want it to suggest how to diagnose", run.NextStep)
	}
}

// A long run must not be reported as a failure just because the tool call cannot
// wait for it.
func TestMCPRunReturnsWhileStillRunning(t *testing.T) {
	m := newMCPHarness(t)
	m.backend.Delay = 30 * time.Second
	m.backend.Work = doTheWork

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Slow task",
		"verification": []string{"test -f marker.txt"},
	}, &created)
	ref := created.Task["ref"].(string)

	var run struct {
		Result       map[string]any `json:"result"`
		StillRunning bool           `json:"still_running"`
		Succeeded    bool           `json:"succeeded"`
		NextStep     string         `json:"next_step"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": ref, "wait_seconds": 1}, &run)

	if !run.StillRunning {
		t.Fatal("still_running = false, but the agent takes 30s and the wait was 1s")
	}
	if run.Succeeded {
		t.Error("succeeded = true for a task that has not finished")
	}
	if !strings.Contains(run.NextStep, "aidev_get_task_result") {
		t.Errorf("next_step = %q, want it to say how to find the outcome", run.NextStep)
	}
	resultTask := run.Result["task"].(map[string]any)
	if status := resultTask["status"]; status != "RUNNING" && status != "VERIFYING" {
		t.Errorf("status = %v, want the task still executing", status)
	}

	// A second call joins the run in flight rather than failing or starting another.
	var second struct {
		StillRunning bool `json:"still_running"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": ref, "wait_seconds": 0}, &second)
	if !second.StillRunning {
		t.Error("a repeated run call did not report the run in flight")
	}
	if len(m.backend.Calls()) != 1 {
		t.Errorf("the agent was started %d times, want 1", len(m.backend.Calls()))
	}

	// Cancelling is how a planner stops it, and the cancellation is recorded.
	var cancelled struct {
		Task    map[string]any `json:"task"`
		Message string         `json:"message"`
	}
	m.call(t, "aidev_cancel_task", map[string]any{"task": ref, "reason": "taking too long"}, &cancelled)
	if cancelled.Task["status"] != "CANCELLED" {
		t.Errorf("status = %v, want CANCELLED", cancelled.Task["status"])
	}
}

func TestMCPApprovalGate(t *testing.T) {
	m := newMCPHarness(t)
	m.backend.Work = doTheWork

	var created struct {
		Task     map[string]any `json:"task"`
		NextStep string         `json:"next_step"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":         m.repoPath,
		"title":             "Needs a human",
		"verification":      []string{"test -f marker.txt"},
		"requires_approval": true,
	}, &created)
	ref := created.Task["ref"].(string)
	if !strings.Contains(created.NextStep, "aidev_approve_task") {
		t.Errorf("next_step = %q, want it to mention approval", created.NextStep)
	}

	// Running a gated task is not an error: the planner is told a human is needed.
	var run struct {
		Result    map[string]any `json:"result"`
		Succeeded bool           `json:"succeeded"`
		NextStep  string         `json:"next_step"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": ref, "wait_seconds": 30}, &run)
	if run.Succeeded {
		t.Error("a gated task reported success")
	}
	resultTask := run.Result["task"].(map[string]any)
	if resultTask["status"] != "WAITING_APPROVAL" {
		t.Errorf("status = %v, want WAITING_APPROVAL", resultTask["status"])
	}
	if !strings.Contains(run.NextStep, "human") {
		t.Errorf("next_step = %q, want it to say a human must decide", run.NextStep)
	}
	if len(m.backend.Calls()) != 0 {
		t.Error("the agent ran despite the approval gate")
	}

	var approved struct {
		Task     map[string]any `json:"task"`
		Approval map[string]any `json:"approval"`
		Message  string         `json:"message"`
	}
	m.call(t, "aidev_approve_task", map[string]any{
		"task": ref, "approve": true, "decided_by": "thuyetmt", "reason": "reviewed",
	}, &approved)
	if approved.Task["status"] != "READY" {
		t.Errorf("status after approval = %v, want READY", approved.Task["status"])
	}
	if approved.Approval["status"] != "GRANTED" {
		t.Errorf("approval = %v, want GRANTED", approved.Approval)
	}

	m.call(t, "aidev_run_task", map[string]any{"task": ref, "wait_seconds": 60}, &run)
	if !run.Succeeded {
		t.Errorf("the approved task did not succeed: %v", run.Result["message"])
	}
}

func TestMCPListAndGet(t *testing.T) {
	m := newMCPHarness(t)

	var first, second struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path": m.repoPath, "title": "One", "verification": []string{"true"},
	}, &first)
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path": m.repoPath, "title": "Two", "verification": []string{"true"},
	}, &second)

	var list struct {
		Tasks []map[string]any `json:"tasks"`
		Count int              `json:"count"`
	}
	m.call(t, "aidev_list_tasks", map[string]any{"repo_path": m.repoPath}, &list)
	if list.Count != 2 {
		t.Errorf("count = %d, want 2", list.Count)
	}

	m.call(t, "aidev_list_tasks", map[string]any{"statuses": []string{"PENDING"}}, &list)
	if list.Count != 2 {
		t.Errorf("filtered count = %d, want 2", list.Count)
	}

	m.call(t, "aidev_list_tasks", map[string]any{"statuses": []string{"SUCCEEDED"}}, &list)
	if list.Count != 0 {
		t.Errorf("SUCCEEDED count = %d, want 0", list.Count)
	}

	// An unknown repository is an empty result, not an error: nothing is wrong
	// with asking about a repository that has no tasks.
	m.call(t, "aidev_list_tasks", map[string]any{"repo_path": newTestRepo(t)}, &list)
	if list.Count != 0 {
		t.Errorf("count = %d for a repository with no tasks, want 0", list.Count)
	}

	ref := first.Task["ref"].(string)
	var got struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_get_task", map[string]any{"task": ref}, &got)
	if got.Task["ref"] != ref {
		t.Errorf("get returned %v, want %s", got.Task["ref"], ref)
	}
	// The UUID must work too, since that is what a planner may have stored.
	m.call(t, "aidev_get_task", map[string]any{"task": first.Task["id"]}, &got)
	if got.Task["ref"] != ref {
		t.Errorf("lookup by uuid returned %v", got.Task["ref"])
	}
}

// Errors must be actionable: they say what was wrong and, where useful, how to
// find the right value.
func TestMCPErrorsAreActionable(t *testing.T) {
	m := newMCPHarness(t)

	msg := m.callExpectingError(t, "aidev_get_task", map[string]any{"task": "TASK-999999"})
	if !strings.Contains(msg, "TASK-999999") || !strings.Contains(msg, "aidev_list_tasks") {
		t.Errorf("error = %q, want the identifier and how to list tasks", msg)
	}

	msg = m.callExpectingError(t, "aidev_create_task", map[string]any{
		"repo_path": m.repoPath, "title": "No verification", "verification": []string{},
	})
	if !strings.Contains(msg, "verification") {
		t.Errorf("error = %q, want it to explain that verification is required", msg)
	}

	msg = m.callExpectingError(t, "aidev_create_task", map[string]any{
		"repo_path": m.repoPath, "title": "Shell", "verification": []string{"go test ./... | tee log"},
	})
	if !strings.Contains(msg, "without a shell") {
		t.Errorf("error = %q, want the no-shell explanation", msg)
	}

	msg = m.callExpectingError(t, "aidev_create_task", map[string]any{
		"repo_path": t.TempDir(), "title": "Not a repo", "verification": []string{"true"},
	})
	if !strings.Contains(strings.ToLower(msg), "repositor") {
		t.Errorf("error = %q, want it to say the path is not a repository", msg)
	}

	msg = m.callExpectingError(t, "aidev_list_tasks", map[string]any{"statuses": []string{"NONSENSE"}})
	if !strings.Contains(msg, "NONSENSE") {
		t.Errorf("error = %q, want the rejected value named", msg)
	}

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path": m.repoPath, "title": "Negative wait", "verification": []string{"true"},
	}, &created)
	msg = m.callExpectingError(t, "aidev_run_task", map[string]any{
		"task": created.Task["ref"], "wait_seconds": -5,
	})
	if !strings.Contains(msg, "wait_seconds") {
		t.Errorf("error = %q, want it to name the bad field", msg)
	}
}

// A task that has never run must report that plainly rather than inventing an
// attempt.
func TestMCPResultForATaskThatHasNotRun(t *testing.T) {
	m := newMCPHarness(t)

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path": m.repoPath, "title": "Never run", "verification": []string{"true"},
	}, &created)

	var result struct {
		Result       map[string]any `json:"result"`
		StillRunning bool           `json:"still_running"`
	}
	m.call(t, "aidev_get_task_result", map[string]any{"task": created.Task["ref"]}, &result)

	if result.StillRunning {
		t.Error("still_running = true for a task that has never run")
	}
	if _, present := result.Result["attempt"]; present {
		t.Error("an attempt was reported for a task that has never run")
	}
	if _, present := result.Result["verification"]; present {
		t.Error("verification results were reported for a task that has never run")
	}
	if result.Result["task"].(map[string]any)["status"] != "PENDING" {
		t.Errorf("status = %v, want PENDING", result.Result["task"])
	}
}

// The verification commands are what the agent is judged by, so the prompt must
// state them — checked here because it is the MCP path that a planner uses and it
// would be easy for the two to diverge.
func TestMCPPromptTellsTheAgentHowItWillBeJudged(t *testing.T) {
	m := newMCPHarness(t)
	m.backend.Work = doTheWork

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Create the marker file",
		"verification": []string{"test -f marker.txt", "true"},
	}, &created)

	var run struct {
		Succeeded bool `json:"succeeded"`
	}
	m.call(t, "aidev_run_task", map[string]any{"task": created.Task["ref"], "wait_seconds": 60}, &run)
	if !run.Succeeded {
		t.Fatal("the run did not succeed")
	}

	call, ok := m.backend.LastCall()
	if !ok {
		t.Fatal("the agent was never called")
	}
	if !strings.Contains(call.Prompt, "test -f marker.txt") {
		t.Errorf("the prompt does not state the verification command:\n%s", call.Prompt)
	}
	if !strings.HasPrefix(call.WorkingDir, m.workspace) {
		t.Errorf("the agent's working dir %q is not under the workspace root", call.WorkingDir)
	}
}

// An MCP server on stdio must never write to stdout: that is the JSON-RPC channel.
// The logger is the thing most likely to break this rule, so it is checked
// directly.
func TestLoggerNeverWritesToStdout(t *testing.T) {
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()

	original := os.Stdout
	os.Stdout = stdout
	defer func() { os.Stdout = original }()

	logging.New(0).Info("this must not reach stdout", "task_ref", "TASK-000001")

	if err := stdout.Sync(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 0 {
		t.Errorf("the logger wrote %d bytes to stdout, which would corrupt the MCP protocol:\n%s",
			len(content), content)
	}
}
