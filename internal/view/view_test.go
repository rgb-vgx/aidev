package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aidev/internal/event"
	"aidev/internal/task"
)

// These shapes are an interface that scripts, the CLI's --json output and the MCP
// tools all depend on, so the field names are asserted rather than left to derive
// from the domain structs.
func TestTaskJSONShape(t *testing.T) {
	tk := task.Task{
		ID:               uuid.Must(uuid.NewV7()),
		Ref:              "TASK-000007",
		ProjectID:        uuid.Must(uuid.NewV7()),
		Title:            "Add a Greet function",
		Status:           task.StatusSucceeded,
		Agent:            "build",
		Priority:         10,
		RequiresApproval: true,
		Timeout:          90 * time.Second,
		Verification: []task.VerificationStep{
			{Command: "go", Args: []string{"test", "./..."}},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	encoded, err := json.Marshal(NewTask(tk))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		"ref", "id", "status", "title", "agent", "priority",
		"verification", "requires_approval", "project_id", "created_at", "updated_at",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("JSON is missing %q: %s", key, encoded)
		}
	}
	if decoded["status"] != "SUCCEEDED" {
		t.Errorf("status = %v", decoded["status"])
	}
	if got := decoded["verification"].([]any); len(got) != 1 || got[0] != "go test ./..." {
		t.Errorf("verification = %v, want the rendered command", got)
	}
	if decoded["timeout_seconds"].(float64) != 90 {
		t.Errorf("timeout_seconds = %v, want 90", decoded["timeout_seconds"])
	}
}

// A failing step's output is included even in the compact view: it is the first
// thing anyone wants after a failure. A passing step's is not, because it would
// dominate a result nobody needs to read.
func TestVerificationIncludesFailureOutputOnly(t *testing.T) {
	exit := 1
	runs := []task.VerificationRun{
		{StepIndex: 0, Command: "go test ./...", Status: task.VerificationPassed, Stdout: "ok"},
		{StepIndex: 1, Command: "go vet ./...", Status: task.VerificationFailed, ExitCode: &exit, Stderr: "vet: something is wrong"},
		{StepIndex: 2, Command: "true", Status: task.VerificationSkipped},
	}

	compact := NewVerifications(runs, false)
	if compact[0].Stdout != "" {
		t.Errorf("a passing step's output should be omitted, got %q", compact[0].Stdout)
	}
	if !strings.Contains(compact[1].Stderr, "something is wrong") {
		t.Errorf("the failing step's stderr was omitted: %+v", compact[1])
	}
	if compact[2].Status != "SKIPPED" || compact[2].ExitCode != nil {
		t.Errorf("skipped step = %+v, want SKIPPED with no exit code", compact[2])
	}

	full := NewVerifications(runs, true)
	if full[0].Stdout != "ok" {
		t.Errorf("the full view should include a passing step's output, got %q", full[0].Stdout)
	}
}

// Absent sections are omitted rather than present and empty, so a reader can tell
// "has not run" from "ran and produced nothing".
func TestResultOmitsAbsentSections(t *testing.T) {
	result := NewResult(
		task.Task{Ref: "TASK-000001", Status: task.StatusPending},
		nil, nil, nil, nil, nil, "not run yet", false,
	)

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &top); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"attempt", "worker", "verification", "worktree", "approval"} {
		if _, present := top[absent]; present {
			t.Errorf("result has a %q section for a task that has not run: %s", absent, encoded)
		}
	}
}

// A zero-valued attempt is not an attempt. Emitting one would tell a caller a task
// had run when it had not.
func TestResultIgnoresAZeroAttempt(t *testing.T) {
	result := NewResult(task.Task{Ref: "TASK-000001"}, &task.TaskAttempt{}, nil, nil, nil, nil, "", false)
	if result.Attempt != nil {
		t.Errorf("Attempt = %+v, want nil for a zero-valued attempt", result.Attempt)
	}
}

func TestResultIncludesEverythingItIsGiven(t *testing.T) {
	exit := 0
	finished := time.Now()
	attempt := task.TaskAttempt{
		ID: uuid.Must(uuid.NewV7()), AttemptNumber: 1,
		Status: task.AttemptSucceeded, StartedAt: time.Now().Add(-time.Minute), FinishedAt: &finished,
	}
	result := NewResult(
		task.Task{Ref: "TASK-000001", Status: task.StatusSucceeded},
		&attempt,
		&task.WorkerRun{Backend: "opencode", Status: task.WorkerSucceeded, ExitCode: &exit, Summary: "did the thing", ChangedFiles: 2},
		[]task.VerificationRun{{StepIndex: 0, Command: "go test ./...", Status: task.VerificationPassed}},
		&task.Worktree{Path: "/tmp/wt", Branch: "aidev/TASK-000001", Status: task.WorktreeRemoved},
		&task.Approval{ID: uuid.Must(uuid.NewV7()), Status: task.ApprovalGranted, DecidedBy: "someone"},
		"succeeded",
		false,
	)

	if result.Attempt == nil || result.Attempt.Number != 1 {
		t.Errorf("attempt = %+v", result.Attempt)
	}
	if result.Worker == nil || result.Worker.ChangedFiles != 2 {
		t.Errorf("worker = %+v", result.Worker)
	}
	if len(result.Verification) != 1 {
		t.Errorf("verification = %+v", result.Verification)
	}
	if result.Worktree == nil || result.Worktree.Status != "REMOVED" {
		t.Errorf("worktree = %+v", result.Worktree)
	}
	if result.Approval == nil || result.Approval.DecidedBy != "someone" {
		t.Errorf("approval = %+v", result.Approval)
	}
}

// The agent's summary must be carried through verbatim: it is evidence about the
// agent, and paraphrasing it would destroy its diagnostic value.
func TestWorkerSummaryIsVerbatim(t *testing.T) {
	claim := "All done! Tests pass."
	w := NewWorker(task.WorkerRun{Backend: "opencode", Status: task.WorkerSucceeded, Summary: claim})
	if w.Summary != claim {
		t.Errorf("summary = %q, want it unchanged", w.Summary)
	}
}

func TestEventPayloadIsPassedThroughUnchanged(t *testing.T) {
	attemptID := uuid.Must(uuid.NewV7())
	e := event.Event{
		Seq:       7,
		Type:      event.TypeVerificationCompleted,
		AttemptID: &attemptID,
		Payload:   []byte(`{"passed":true,"summary":"1/1 steps"}`),
		CreatedAt: time.Now(),
	}

	withPayload, err := json.Marshal(NewEvent(e, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withPayload), `"passed":true`) {
		t.Errorf("payload was not passed through: %s", withPayload)
	}
	if !strings.Contains(string(withPayload), attemptID.String()) {
		t.Errorf("attempt id missing: %s", withPayload)
	}

	without, err := json.Marshal(NewEvent(e, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "passed") {
		t.Errorf("payload was included when not requested: %s", without)
	}
}

func TestRawJSONHandlesEmpty(t *testing.T) {
	encoded, err := RawJSON(nil).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "null" {
		t.Errorf("empty RawJSON = %s, want null", encoded)
	}
}

// Tail keeps the end, which is where a failure's explanation usually is.
func TestTail(t *testing.T) {
	if got := Tail("0123456789", 4); got != "…6789" {
		t.Errorf("Tail = %q", got)
	}
	if got := Tail("abc", 10); got != "abc" {
		t.Errorf("Tail should leave short input alone, got %q", got)
	}
}
