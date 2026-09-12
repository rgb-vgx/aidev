package task

import "fmt"

// Status is a task's lifecycle state. It is a distinct type rather than a
// string so that the compiler rejects an arbitrary value, and every transition
// goes through Status.CanTransitionTo.
type Status string

const (
	// StatusPending means the task exists but is not yet eligible to run.
	// In the MVP nothing blocks a task, so it becomes ready as soon as a run
	// is requested; the state exists because a future dependency graph needs
	// somewhere to hold a task that is waiting on another one.
	StatusPending Status = "PENDING"

	// StatusReady means the task is eligible to be picked up.
	StatusReady Status = "READY"

	// StatusRunning means an agent is executing inside the task's worktree.
	StatusRunning Status = "RUNNING"

	// StatusVerifying means the agent finished and aidev is independently
	// running the task's verification commands.
	StatusVerifying Status = "VERIFYING"

	// StatusSucceeded means verification passed. It is the only success state
	// and it is reachable only from StatusVerifying, which is what makes an
	// agent's own claim of success irrelevant.
	StatusSucceeded Status = "SUCCEEDED"

	// StatusFailed means the attempt failed: the agent errored, timed out, or
	// verification did not pass.
	StatusFailed Status = "FAILED"

	// StatusCancelled means a human or a shutdown stopped the task.
	StatusCancelled Status = "CANCELLED"

	// StatusWaitingApproval means the task is gated by policy and will not
	// execute until an approval is granted.
	StatusWaitingApproval Status = "WAITING_APPROVAL"
)

// AllStatuses lists every valid status. Used by validation and by the
// migration's CHECK constraint, which must stay in agreement with this list;
// TestStatusesMatchMigration asserts that.
func AllStatuses() []Status {
	return []Status{
		StatusPending,
		StatusReady,
		StatusRunning,
		StatusVerifying,
		StatusSucceeded,
		StatusFailed,
		StatusCancelled,
		StatusWaitingApproval,
	}
}

// transitions is the authoritative state machine. A status absent from a
// source's set is not reachable from it.
//
// Deliberately absent in the MVP:
//   - FAILED -> READY (retry). Attempts are recorded so that retry can be
//     added later, but automatic retry is out of scope, so the edge does not
//     exist yet rather than existing and being unused.
//   - any edge out of a terminal state.
var transitions = map[Status]map[Status]bool{
	StatusPending: {
		StatusReady:           true,
		StatusWaitingApproval: true,
		StatusCancelled:       true,
	},
	StatusReady: {
		StatusRunning:         true,
		StatusWaitingApproval: true,
		StatusCancelled:       true,
	},
	StatusWaitingApproval: {
		StatusReady:     true, // approval granted
		StatusFailed:    true, // approval denied
		StatusCancelled: true,
	},
	StatusRunning: {
		StatusVerifying: true,
		StatusFailed:    true,
		StatusCancelled: true,
	},
	StatusVerifying: {
		StatusSucceeded: true,
		StatusFailed:    true,
		StatusCancelled: true,
	},
	StatusSucceeded: {},
	StatusFailed:    {},
	StatusCancelled: {},
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal reports whether no transition out of s exists.
func (s Status) Terminal() bool {
	return len(transitions[s]) == 0
}

// Active reports whether the task is currently occupying execution resources
// (a worktree, a subprocess). Used to decide what a cancellation must clean up.
func (s Status) Active() bool {
	return s == StatusRunning || s == StatusVerifying
}

// CanTransitionTo reports whether s -> next is a legal transition.
func (s Status) CanTransitionTo(next Status) bool {
	return transitions[s][next]
}

// String makes Status printable.
func (s Status) String() string { return string(s) }

// ParseStatus converts a persisted or user-supplied string into a Status.
func ParseStatus(raw string) (Status, error) {
	s := Status(raw)
	if !s.Valid() {
		return "", fmt.Errorf("%q is not a valid task status", raw)
	}
	return s, nil
}

// TransitionError reports a rejected state change. It is a distinct type so
// callers (and the MCP layer) can turn an illegal transition into a precise
// message instead of a generic failure.
type TransitionError struct {
	From Status
	To   Status
}

func (e *TransitionError) Error() string {
	if !e.From.Valid() {
		return fmt.Sprintf("cannot transition from unknown status %q", e.From)
	}
	if !e.To.Valid() {
		return fmt.Sprintf("cannot transition %s -> unknown status %q", e.From, e.To)
	}
	if e.From.Terminal() {
		return fmt.Sprintf("cannot transition %s -> %s: %s is terminal", e.From, e.To, e.From)
	}
	return fmt.Sprintf("cannot transition %s -> %s", e.From, e.To)
}
