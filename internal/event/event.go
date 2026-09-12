// Package event defines aidev's append-only task event log.
//
// Events exist for auditability, debugging and task history. They are append
// only: nothing in aidev updates or deletes an event, and the database enforces
// that with a trigger rather than trusting every future caller to remember.
package event

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Type names a kind of event. Values are lowercase dotted strings so they read
// well in logs and can be filtered by prefix.
type Type string

const (
	// Lifecycle.
	TypeTaskCreated   Type = "task.created"
	TypeTaskReady     Type = "task.ready"
	TypeTaskStarted   Type = "task.started"
	TypeTaskSucceeded Type = "task.succeeded"
	TypeTaskFailed    Type = "task.failed"
	TypeTaskCancelled Type = "task.cancelled"

	// Worktree isolation.
	TypeWorktreeCreated  Type = "task.worktree_created"
	TypeWorktreeRemoved  Type = "task.worktree_removed"
	TypeWorktreeRetained Type = "task.worktree_retained"

	// Agent execution.
	TypeWorkerStarted   Type = "task.worker_started"
	TypeWorkerCompleted Type = "task.worker_completed"

	// Independent verification.
	TypeVerificationStarted   Type = "task.verification_started"
	TypeVerificationStepRan   Type = "task.verification_step_completed"
	TypeVerificationCompleted Type = "task.verification_completed"

	// Approval policy.
	TypeApprovalRequired Type = "task.approval_required"
	TypeApprovalGranted  Type = "task.approval_granted"
	TypeApprovalDenied   Type = "task.approval_denied"
)

// AllTypes lists every event type aidev emits. The migration's CHECK constraint
// is generated from the same vocabulary; TestEventTypesMatchMigration keeps the
// two in agreement.
func AllTypes() []Type {
	return []Type{
		TypeTaskCreated, TypeTaskReady, TypeTaskStarted, TypeTaskSucceeded,
		TypeTaskFailed, TypeTaskCancelled,
		TypeWorktreeCreated, TypeWorktreeRemoved, TypeWorktreeRetained,
		TypeWorkerStarted, TypeWorkerCompleted,
		TypeVerificationStarted, TypeVerificationStepRan, TypeVerificationCompleted,
		TypeApprovalRequired, TypeApprovalGranted, TypeApprovalDenied,
	}
}

// Valid reports whether t is a known event type.
func (t Type) Valid() bool {
	for _, v := range AllTypes() {
		if t == v {
			return true
		}
	}
	return false
}

func (t Type) String() string { return string(t) }

// ParseType converts a string into a Type.
func ParseType(raw string) (Type, error) {
	t := Type(raw)
	if !t.Valid() {
		return "", fmt.Errorf("%q is not a known event type", raw)
	}
	return t, nil
}

// Event is one immutable record of something that happened to a task.
type Event struct {
	ID uuid.UUID

	// Seq is a database-assigned total ordering. Two events created in the same
	// millisecond are still strictly ordered, which timestamps alone cannot
	// guarantee.
	Seq int64

	TaskID uuid.UUID

	// AttemptID is set for events that belong to a specific execution attempt.
	AttemptID *uuid.UUID

	Type Type

	// Payload is event-specific detail stored as JSONB. It is always a JSON
	// object, never a bare value, so that fields can be added later without
	// changing the shape consumers read.
	Payload json.RawMessage

	CreatedAt time.Time
}

// New builds an event, marshalling payload to JSON. A nil payload becomes an
// empty object. The returned error reports an unmarshallable payload rather
// than silently dropping detail from the audit log.
func New(taskID uuid.UUID, attemptID *uuid.UUID, t Type, payload any) (Event, error) {
	if taskID == uuid.Nil {
		return Event{}, fmt.Errorf("event %s requires a task id", t)
	}
	if !t.Valid() {
		return Event{}, fmt.Errorf("%q is not a known event type", t)
	}

	raw := json.RawMessage("{}")
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("event %s payload is not JSON-encodable: %w", t, err)
		}
		// Reject non-object payloads so consumers can always index by field.
		if len(encoded) == 0 || encoded[0] != '{' {
			return Event{}, fmt.Errorf("event %s payload must encode to a JSON object, got %s", t, truncate(string(encoded), 40))
		}
		raw = encoded
	}

	return Event{
		ID:        uuid.Must(uuid.NewV7()),
		TaskID:    taskID,
		AttemptID: attemptID,
		Type:      t,
		Payload:   raw,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
