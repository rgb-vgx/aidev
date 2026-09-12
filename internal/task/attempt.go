package task

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// AttemptStatus is the state of one execution attempt of a task.
type AttemptStatus string

const (
	AttemptRunning   AttemptStatus = "RUNNING"
	AttemptSucceeded AttemptStatus = "SUCCEEDED"
	AttemptFailed    AttemptStatus = "FAILED"
	AttemptCancelled AttemptStatus = "CANCELLED"
)

// AllAttemptStatuses lists every valid attempt status.
func AllAttemptStatuses() []AttemptStatus {
	return []AttemptStatus{AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptCancelled}
}

// Valid reports whether s is a known attempt status.
func (s AttemptStatus) Valid() bool {
	for _, v := range AllAttemptStatuses() {
		if s == v {
			return true
		}
	}
	return false
}

func (s AttemptStatus) String() string { return string(s) }

// FailureKind classifies why something failed, so that a caller can react
// without parsing prose. The values come from failure modes actually observed
// in Phase 0 (docs/research.md §2.5) rather than from guesswork; an unmapped
// failure is recorded as FailureUnknown instead of being forced into a bucket.
type FailureKind string

const (
	// FailureNone means no failure.
	FailureNone FailureKind = ""

	// FailureStartup means the agent process could not be started or rejected
	// its arguments before doing any work. Observed: an invalid working
	// directory exits 1 with a message on stderr and nothing on stdout.
	FailureStartup FailureKind = "STARTUP"

	// FailureAgentError means the agent reported an error in its own event
	// stream. Observed: an invalid model produces an "error" NDJSON event.
	FailureAgentError FailureKind = "AGENT_ERROR"

	// FailureAgentExit means the agent exited non-zero without an error event.
	FailureAgentExit FailureKind = "AGENT_EXIT"

	// FailureTimeout means aidev's own deadline elapsed.
	FailureTimeout FailureKind = "TIMEOUT"

	// FailureCancelled means a human or a shutdown stopped the work.
	FailureCancelled FailureKind = "CANCELLED"

	// FailureVerification means the agent finished but aidev's verification
	// did not pass. This is the expected failure mode, not an anomaly.
	FailureVerification FailureKind = "VERIFICATION"

	// FailureWorktree means the isolated workspace could not be prepared or
	// inspected.
	FailureWorktree FailureKind = "WORKTREE"

	// FailureApprovalDenied means policy refused the task.
	FailureApprovalDenied FailureKind = "APPROVAL_DENIED"

	// FailureInternal means aidev itself failed, for example a database error.
	FailureInternal FailureKind = "INTERNAL"

	// FailureUnknown means the failure could not be classified. Kept as an
	// honest bucket rather than silently mapping to something plausible.
	FailureUnknown FailureKind = "UNKNOWN"
)

// AllFailureKinds lists every classification except FailureNone.
func AllFailureKinds() []FailureKind {
	return []FailureKind{
		FailureStartup, FailureAgentError, FailureAgentExit, FailureTimeout,
		FailureCancelled, FailureVerification, FailureWorktree,
		FailureApprovalDenied, FailureInternal, FailureUnknown,
	}
}

// Valid reports whether k is a known classification. FailureNone is valid.
func (k FailureKind) Valid() bool {
	if k == FailureNone {
		return true
	}
	for _, v := range AllFailureKinds() {
		if k == v {
			return true
		}
	}
	return false
}

func (k FailureKind) String() string { return string(k) }

// TaskAttempt records one execution of a task. Attempts are never overwritten:
// a future retry appends a new attempt, which is what makes task history
// reconstructable.
type TaskAttempt struct {
	ID            uuid.UUID
	TaskID        uuid.UUID
	AttemptNumber int
	Status        AttemptStatus
	FailureKind   FailureKind
	Error         string
	StartedAt     time.Time
	FinishedAt    *time.Time
}

// NewAttempt builds a running attempt.
func NewAttempt(taskID uuid.UUID, number int) TaskAttempt {
	return TaskAttempt{
		ID:            uuid.Must(uuid.NewV7()),
		TaskID:        taskID,
		AttemptNumber: number,
		Status:        AttemptRunning,
		StartedAt:     time.Now().UTC(),
	}
}

// Duration returns how long the attempt took, or the elapsed time so far.
func (a TaskAttempt) Duration() time.Duration {
	if a.FinishedAt == nil {
		return time.Since(a.StartedAt)
	}
	return a.FinishedAt.Sub(a.StartedAt)
}

// WorktreeStatus is the lifecycle of an isolated workspace.
type WorktreeStatus string

const (
	// WorktreeActive means the worktree exists and is in use.
	WorktreeActive WorktreeStatus = "ACTIVE"

	// WorktreeRemoved means aidev deleted it after a clean success.
	WorktreeRemoved WorktreeStatus = "REMOVED"

	// WorktreeRetained means aidev deliberately kept it because the attempt
	// failed or was cancelled and the work may still be useful. Retained
	// worktrees are never deleted implicitly.
	WorktreeRetained WorktreeStatus = "RETAINED"
)

// AllWorktreeStatuses lists every valid worktree status.
func AllWorktreeStatuses() []WorktreeStatus {
	return []WorktreeStatus{WorktreeActive, WorktreeRemoved, WorktreeRetained}
}

func (s WorktreeStatus) String() string { return string(s) }

// Worktree records the isolated workspace used by an attempt.
type Worktree struct {
	ID         uuid.UUID
	AttemptID  uuid.UUID
	Path       string
	Branch     string
	BaseCommit string
	HeadCommit string
	Status     WorktreeStatus
	CreatedAt  time.Time
	RemovedAt  *time.Time
}

// WorkerRunStatus is the outcome of one agent invocation.
type WorkerRunStatus string

const (
	WorkerSucceeded WorkerRunStatus = "SUCCEEDED"
	WorkerFailed    WorkerRunStatus = "FAILED"
	WorkerTimedOut  WorkerRunStatus = "TIMED_OUT"
	WorkerCancelled WorkerRunStatus = "CANCELLED"
)

// AllWorkerRunStatuses lists every valid worker run status.
func AllWorkerRunStatuses() []WorkerRunStatus {
	return []WorkerRunStatus{WorkerSucceeded, WorkerFailed, WorkerTimedOut, WorkerCancelled}
}

func (s WorkerRunStatus) String() string { return string(s) }

// WorkerRun records what an agent backend did: the command, the captured
// output, the exit status, and the structured result where the backend provides
// one. It is the audit record for "what did the agent actually do".
type WorkerRun struct {
	ID        uuid.UUID
	AttemptID uuid.UUID

	// Backend names the implementation, for example "opencode" or "fake".
	Backend string

	Status      WorkerRunStatus
	FailureKind FailureKind

	// Command is the argv rendered for display. Arguments are recorded because
	// they are task-defined and contain no secrets; environment is not.
	Command    string
	WorkingDir string

	// ExitCode is nil when the process never produced one, for example when it
	// could not be started.
	ExitCode *int

	Stdout          string
	StdoutTruncated bool
	Stderr          string
	StderrTruncated bool

	// SessionID is the agent's own session identifier when it exposes one.
	// Recorded now, unused by the MVP: Phase 0 confirmed OpenCode can resume a
	// session by id, which is what a future retry-with-context needs.
	SessionID string

	// Summary is the agent's closing message, when it produced one.
	Summary string

	// FinishReason is the backend's own completion reason, passed through
	// without interpretation.
	FinishReason string

	// Tokens and Cost hold usage reported by the backend, as raw JSON so that
	// aidev does not have to model every backend's accounting.
	Tokens []byte
	Cost   *float64

	// Diff and ChangedFiles describe what the agent changed in the worktree,
	// collected by aidev from git rather than reported by the agent.
	Diff          string
	DiffTruncated bool
	ChangedFiles  int

	StartedAt  time.Time
	FinishedAt *time.Time
}

// Duration returns how long the worker ran.
func (r WorkerRun) Duration() time.Duration {
	if r.FinishedAt == nil {
		return time.Since(r.StartedAt)
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// VerificationStatus is the outcome of one verification step.
type VerificationStatus string

const (
	VerificationPassed    VerificationStatus = "PASSED"
	VerificationFailed    VerificationStatus = "FAILED"
	VerificationTimedOut  VerificationStatus = "TIMED_OUT"
	VerificationCancelled VerificationStatus = "CANCELLED"

	// VerificationSkipped means an earlier step failed, so this one was not
	// run. Recorded explicitly so that a result never looks like a pass.
	VerificationSkipped VerificationStatus = "SKIPPED"
)

// AllVerificationStatuses lists every valid verification status.
func AllVerificationStatuses() []VerificationStatus {
	return []VerificationStatus{
		VerificationPassed, VerificationFailed, VerificationTimedOut,
		VerificationCancelled, VerificationSkipped,
	}
}

func (s VerificationStatus) String() string { return string(s) }

// VerificationRun records aidev running one verification step itself.
type VerificationRun struct {
	ID        uuid.UUID
	AttemptID uuid.UUID

	// StepIndex is the position in the task's verification list, so results can
	// be matched back to the step that produced them.
	StepIndex int

	Command string
	Status  VerificationStatus

	ExitCode        *int
	Stdout          string
	StdoutTruncated bool
	Stderr          string
	StderrTruncated bool

	StartedAt  time.Time
	FinishedAt *time.Time
	Duration   time.Duration
}

// Passed reports whether the step succeeded.
func (r VerificationRun) Passed() bool { return r.Status == VerificationPassed }

// ApprovalStatus is the state of a human approval request.
type ApprovalStatus string

const (
	ApprovalPending ApprovalStatus = "PENDING"
	ApprovalGranted ApprovalStatus = "GRANTED"
	ApprovalDenied  ApprovalStatus = "DENIED"
)

// AllApprovalStatuses lists every valid approval status.
func AllApprovalStatuses() []ApprovalStatus {
	return []ApprovalStatus{ApprovalPending, ApprovalGranted, ApprovalDenied}
}

func (s ApprovalStatus) String() string { return string(s) }

// ParseApprovalStatus converts a string into an ApprovalStatus.
func ParseApprovalStatus(raw string) (ApprovalStatus, error) {
	s := ApprovalStatus(raw)
	for _, v := range AllApprovalStatuses() {
		if s == v {
			return s, nil
		}
	}
	return "", fmt.Errorf("%q is not a valid approval status", raw)
}

// Approval records a human decision gating a task.
type Approval struct {
	ID          uuid.UUID
	TaskID      uuid.UUID
	Status      ApprovalStatus
	Reason      string
	RequestedAt time.Time
	DecidedAt   *time.Time
	DecidedBy   string
}
