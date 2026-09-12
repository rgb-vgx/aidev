package event

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAllTypesAreValid(t *testing.T) {
	for _, ty := range AllTypes() {
		if !ty.Valid() {
			t.Errorf("%s is listed by AllTypes but reports itself invalid", ty)
		}
		if !strings.HasPrefix(string(ty), "task.") {
			t.Errorf("%s does not use the task. prefix, so prefix filtering would miss it", ty)
		}
	}
	if Type("task.invented").Valid() {
		t.Error("an unknown type reported itself valid")
	}
}

// The brief's lifecycle vocabulary must all exist; a missing one would mean a
// stage of the pipeline leaves no trace in history.
func TestLifecycleVocabularyIsComplete(t *testing.T) {
	required := []Type{
		TypeTaskCreated, TypeTaskStarted, TypeWorkerStarted, TypeWorkerCompleted,
		TypeVerificationStarted, TypeVerificationCompleted,
		TypeTaskSucceeded, TypeTaskFailed, TypeTaskCancelled, TypeApprovalRequired,
	}
	have := map[Type]bool{}
	for _, ty := range AllTypes() {
		have[ty] = true
	}
	for _, ty := range required {
		if !have[ty] {
			t.Errorf("event type %s is missing from AllTypes", ty)
		}
	}
}

func TestNewEvent(t *testing.T) {
	taskID := uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())

	e, err := New(taskID, &attemptID, TypeWorkerStarted, map[string]any{"backend": "opencode"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.ID == uuid.Nil {
		t.Error("no id assigned")
	}
	if e.TaskID != taskID {
		t.Errorf("task id = %s, want %s", e.TaskID, taskID)
	}
	if e.AttemptID == nil || *e.AttemptID != attemptID {
		t.Errorf("attempt id = %v, want %s", e.AttemptID, attemptID)
	}
	if e.CreatedAt.IsZero() {
		t.Error("no timestamp")
	}

	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if payload["backend"] != "opencode" {
		t.Errorf("payload = %v, want backend=opencode", payload)
	}
}

func TestNilPayloadBecomesEmptyObject(t *testing.T) {
	e, err := New(uuid.Must(uuid.NewV7()), nil, TypeTaskCreated, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if string(e.Payload) != "{}" {
		t.Errorf("payload = %s, want {}", e.Payload)
	}
}

// Consumers index payloads by field name, so a payload that is not an object
// would break them. Rejecting it at construction keeps that impossible.
func TestPayloadMustBeAnObject(t *testing.T) {
	for _, payload := range []any{42, "a string", []int{1, 2}, true} {
		_, err := New(uuid.Must(uuid.NewV7()), nil, TypeTaskCreated, payload)
		if err == nil {
			t.Errorf("payload %#v was accepted; only JSON objects are allowed", payload)
		}
	}
}

func TestUnencodablePayloadIsReported(t *testing.T) {
	// A channel cannot be marshalled. Dropping the detail silently would leave a
	// gap in the audit log, so New reports it.
	_, err := New(uuid.Must(uuid.NewV7()), nil, TypeTaskCreated, map[string]any{"ch": make(chan int)})
	if err == nil {
		t.Fatal("an unencodable payload was accepted")
	}
	if !strings.Contains(err.Error(), "not JSON-encodable") {
		t.Errorf("error = %v, want it to explain the encoding failure", err)
	}
}

func TestNewRejectsMissingTaskAndUnknownType(t *testing.T) {
	if _, err := New(uuid.Nil, nil, TypeTaskCreated, nil); err == nil {
		t.Error("an event without a task id was accepted")
	}
	if _, err := New(uuid.Must(uuid.NewV7()), nil, Type("task.nope"), nil); err == nil {
		t.Error("an unknown event type was accepted")
	}
}

func TestParseType(t *testing.T) {
	if got, err := ParseType("task.succeeded"); err != nil || got != TypeTaskSucceeded {
		t.Errorf("ParseType = %v, %v", got, err)
	}
	if _, err := ParseType("TASK.SUCCEEDED"); err == nil {
		t.Error("types are lowercase; the uppercase form should not parse")
	}
}
