package task

import (
	"errors"
	"strings"
	"testing"
)

func TestAllStatusesAreValid(t *testing.T) {
	for _, s := range AllStatuses() {
		if !s.Valid() {
			t.Errorf("status %s is listed by AllStatuses but reports itself invalid", s)
		}
	}
	if Status("NOPE").Valid() {
		t.Error("an unknown status reported itself valid")
	}
}

func TestTerminalStates(t *testing.T) {
	terminal := map[Status]bool{
		StatusSucceeded: true,
		StatusFailed:    true,
		StatusCancelled: true,
	}
	for _, s := range AllStatuses() {
		if got, want := s.Terminal(), terminal[s]; got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}
}

// TestHappyPathIsTheOnlyRouteToSucceeded is the product's central rule as a
// test: success is reachable only by passing through verification.
func TestHappyPathIsTheOnlyRouteToSucceeded(t *testing.T) {
	for _, from := range AllStatuses() {
		canSucceed := from.CanTransitionTo(StatusSucceeded)
		if from == StatusVerifying {
			if !canSucceed {
				t.Fatal("VERIFYING must be able to reach SUCCEEDED")
			}
			continue
		}
		if canSucceed {
			t.Errorf("%s must not reach SUCCEEDED directly: only independent verification may declare success", from)
		}
	}
}

func TestRunningCannotSkipVerification(t *testing.T) {
	if StatusRunning.CanTransitionTo(StatusSucceeded) {
		t.Error("RUNNING -> SUCCEEDED would let an agent's own claim decide the outcome")
	}
	if !StatusRunning.CanTransitionTo(StatusVerifying) {
		t.Error("RUNNING -> VERIFYING must be allowed")
	}
}

func TestTerminalStatesAreDeadEnds(t *testing.T) {
	for _, from := range []Status{StatusSucceeded, StatusFailed, StatusCancelled} {
		for _, to := range AllStatuses() {
			if from.CanTransitionTo(to) {
				t.Errorf("%s -> %s must not be allowed: %s is terminal", from, to, from)
			}
		}
	}
}

// The MVP does not retry. The edge is absent rather than present-and-unused, so
// that adding retry later is a deliberate change with a test to update.
func TestFailedDoesNotRetryYet(t *testing.T) {
	if StatusFailed.CanTransitionTo(StatusReady) {
		t.Error("FAILED -> READY exists, but automatic retry is out of scope for the MVP")
	}
}

func TestCancellationReachableFromEveryLiveState(t *testing.T) {
	for _, from := range AllStatuses() {
		if from.Terminal() {
			continue
		}
		if !from.CanTransitionTo(StatusCancelled) {
			t.Errorf("%s cannot be cancelled, so a stuck task would have no way out", from)
		}
	}
}

func TestApprovalGate(t *testing.T) {
	for _, from := range []Status{StatusPending, StatusReady} {
		if !from.CanTransitionTo(StatusWaitingApproval) {
			t.Errorf("%s must be able to enter WAITING_APPROVAL", from)
		}
	}
	if !StatusWaitingApproval.CanTransitionTo(StatusReady) {
		t.Error("a granted approval must be able to release the task")
	}
	if !StatusWaitingApproval.CanTransitionTo(StatusFailed) {
		t.Error("a denied approval must be able to fail the task")
	}
	if StatusWaitingApproval.CanTransitionTo(StatusRunning) {
		t.Error("WAITING_APPROVAL -> RUNNING would bypass the approval gate")
	}
}

func TestActiveStates(t *testing.T) {
	for _, s := range AllStatuses() {
		want := s == StatusRunning || s == StatusVerifying
		if s.Active() != want {
			t.Errorf("%s.Active() = %v, want %v", s, s.Active(), want)
		}
	}
}

func TestTransitionToChecksTheStateMachine(t *testing.T) {
	tk := Task{Status: StatusPending}

	if err := tk.TransitionTo(StatusRunning); err == nil {
		t.Fatal("PENDING -> RUNNING should have been rejected")
	} else {
		var te *TransitionError
		if !errors.As(err, &te) {
			t.Fatalf("want *TransitionError, got %T", err)
		}
		if te.From != StatusPending || te.To != StatusRunning {
			t.Errorf("TransitionError = %+v, want From=PENDING To=RUNNING", te)
		}
	}
	if tk.Status != StatusPending {
		t.Errorf("a rejected transition changed status to %s", tk.Status)
	}

	if err := tk.TransitionTo(StatusReady); err != nil {
		t.Fatalf("PENDING -> READY: %v", err)
	}
	if tk.Status != StatusReady {
		t.Errorf("status = %s, want READY", tk.Status)
	}
	if tk.UpdatedAt.IsZero() {
		t.Error("a successful transition did not stamp UpdatedAt")
	}
}

func TestTransitionErrorMessages(t *testing.T) {
	cases := []struct {
		name string
		err  *TransitionError
		want string
	}{
		{"terminal source", &TransitionError{From: StatusSucceeded, To: StatusReady}, "SUCCEEDED is terminal"},
		{"unknown source", &TransitionError{From: Status("X"), To: StatusReady}, "unknown status"},
		{"unknown target", &TransitionError{From: StatusReady, To: Status("X")}, "unknown status"},
		{"plain", &TransitionError{From: StatusPending, To: StatusRunning}, "cannot transition PENDING -> RUNNING"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("Error() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	if got, err := ParseStatus("RUNNING"); err != nil || got != StatusRunning {
		t.Errorf("ParseStatus(RUNNING) = %v, %v", got, err)
	}
	if _, err := ParseStatus("running"); err == nil {
		t.Error("lowercase status should be rejected: persisted values are uppercase")
	}
	if _, err := ParseStatus(""); err == nil {
		t.Error("empty status should be rejected")
	}
}
