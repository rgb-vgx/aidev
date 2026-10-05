package task

import (
	"fmt"
	"time"
)

// ApplyState says whether a SUCCEEDED task's verified result has been taken
// into the operator's branch. It is derived from the event log
// (task.applied / task.apply_undone), never stored: the newest of the two
// event types decides, and a task with neither was never applied. Three
// states, not a boolean: "never applied" and "applied and then undone" are
// different things to an operator.
type ApplyState string

const (
	// ApplyNever means no apply event was ever recorded.
	ApplyNever ApplyState = ""
	// ApplyApplied means the newest event is task.applied.
	ApplyApplied ApplyState = "applied"
	// ApplyUndone means the newest event is task.apply_undone.
	ApplyUndone ApplyState = "undone"
)

// AllApplyStates lists every apply state.
func AllApplyStates() []ApplyState {
	return []ApplyState{
		ApplyNever,
		ApplyApplied,
		ApplyUndone,
	}
}

// Valid reports whether s is a known apply state.
func (s ApplyState) Valid() bool {
	switch s {
	case ApplyNever, ApplyApplied, ApplyUndone:
		return true
	}
	return false
}

// String makes ApplyState printable.
func (s ApplyState) String() string { return string(s) }

// ParseApplyState converts a string into an ApplyState.
func ParseApplyState(raw string) (ApplyState, error) {
	s := ApplyState(raw)
	if !s.Valid() {
		return "", fmt.Errorf("%q is not a valid apply state", raw)
	}
	return s, nil
}

// Apply is a task's derived apply state: where its verified result went, or
// came back from.
type Apply struct {
	State  ApplyState
	Into   string
	Commit string
	Undid  string
	// At is when the newest of the two events was recorded.
	At time.Time
}
