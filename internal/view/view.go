// Package view holds aidev's external JSON shapes.
//
// The CLI's --json output and the MCP tools' structured results are the same
// contract, so they are defined once here rather than twice. They are separate
// types from the domain structs on purpose: this is an interface that scripts and
// a planner depend on, and deriving it from task.Task would make every internal
// rename a silent breaking change.
//
// The `jsonschema` tags are read by the MCP SDK to generate each tool's output
// schema, so a field's description is written for the reader of that schema.
package view

import (
	"time"

	"github.com/google/uuid"

	"aidev/internal/event"
	"aidev/internal/task"
)

// Task is a task as seen from outside.
type Task struct {
	Ref                string   `json:"ref" jsonschema:"human-facing task reference, for example TASK-000001"`
	ID                 string   `json:"id" jsonschema:"the task's UUID"`
	Status             string   `json:"status" jsonschema:"PENDING, READY, RUNNING, VERIFYING, SUCCEEDED, FAILED, CANCELLED or WAITING_APPROVAL"`
	Title              string   `json:"title" jsonschema:"short statement of the work"`
	Description        string   `json:"description,omitempty" jsonschema:"the full instruction given to the agent"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty" jsonschema:"what done looks like"`
	Agent              string   `json:"agent" jsonschema:"agent used to implement the task"`
	Model              string   `json:"model,omitempty" jsonschema:"model used to implement the task; empty means the configured default"`
	Hardness           string   `json:"hardness,omitempty" jsonschema:"how hard the task stated it was: TRIVIAL, STANDARD or HARD; empty means none was stated"`
	Priority           int      `json:"priority" jsonschema:"higher runs first"`
	Verification       []string `json:"verification" jsonschema:"the commands aidev runs itself to decide whether the task succeeded"`
	RequiresApproval   bool     `json:"requires_approval" jsonschema:"whether a human decision is required before the task may run"`
	MaxRetries         int      `json:"max_retries" jsonschema:"recorded for a future retry feature; aidev does not retry"`
	BaseRef            string   `json:"base_ref,omitempty" jsonschema:"git ref the task's branch starts from"`
	TimeoutSeconds     int      `json:"timeout_seconds,omitempty" jsonschema:"per-task agent timeout; 0 means the configured default"`
	ProjectID          string   `json:"project_id" jsonschema:"the repository this task belongs to"`
	CreatedAt          string   `json:"created_at" jsonschema:"RFC3339 timestamp"`
	UpdatedAt          string   `json:"updated_at" jsonschema:"RFC3339 timestamp"`
}

// NewTask converts a domain task.
func NewTask(t task.Task) Task {
	commands := make([]string, 0, len(t.Verification))
	for _, step := range t.Verification {
		commands = append(commands, step.String())
	}
	return Task{
		Ref:                t.Ref,
		ID:                 t.ID.String(),
		Status:             t.Status.String(),
		Title:              t.Title,
		Description:        t.Description,
		AcceptanceCriteria: t.AcceptanceCriteria,
		Agent:              t.Agent,
		Model:              t.Model,
		Hardness:           t.Hardness.String(),
		Priority:           t.Priority,
		Verification:       commands,
		RequiresApproval:   t.RequiresApproval,
		MaxRetries:         t.MaxRetries,
		BaseRef:            t.BaseRef,
		TimeoutSeconds:     int(t.Timeout.Seconds()),
		ProjectID:          t.ProjectID.String(),
		CreatedAt:          t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:          t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// Attempt is one execution of a task.
type Attempt struct {
	ID          string `json:"id" jsonschema:"the attempt's UUID"`
	Number      int    `json:"number" jsonschema:"1 for the first attempt"`
	Status      string `json:"status" jsonschema:"RUNNING, SUCCEEDED, FAILED or CANCELLED"`
	FailureKind string `json:"failure_kind,omitempty" jsonschema:"why it failed: STARTUP, AGENT_ERROR, AGENT_EXIT, TIMEOUT, CANCELLED, VERIFICATION, WORKTREE, APPROVAL_DENIED, INTERNAL or UNKNOWN"`
	Error       string `json:"error,omitempty" jsonschema:"failure detail"`
	StartedAt   string `json:"started_at" jsonschema:"RFC3339 timestamp"`
	FinishedAt  string `json:"finished_at,omitempty" jsonschema:"RFC3339 timestamp, absent while running"`
	DurationMS  int64  `json:"duration_ms" jsonschema:"elapsed milliseconds"`
}

// NewAttempt converts a domain attempt.
func NewAttempt(a task.TaskAttempt) *Attempt {
	v := &Attempt{
		ID:          a.ID.String(),
		Number:      a.AttemptNumber,
		Status:      a.Status.String(),
		FailureKind: a.FailureKind.String(),
		Error:       a.Error,
		StartedAt:   a.StartedAt.UTC().Format(time.RFC3339),
		DurationMS:  a.Duration().Milliseconds(),
	}
	if a.FinishedAt != nil {
		v.FinishedAt = a.FinishedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// Worker is what the agent did. It is the agent's own account plus facts aidev
// observed; Summary in particular is a claim, not evidence.
type Worker struct {
	Backend      string `json:"backend" jsonschema:"agent backend that ran, for example opencode"`
	Status       string `json:"status" jsonschema:"SUCCEEDED, FAILED, TIMED_OUT or CANCELLED - describes the agent's run, not whether the work is correct"`
	FailureKind  string `json:"failure_kind,omitempty" jsonschema:"classification when the agent did not succeed"`
	ExitCode     *int   `json:"exit_code,omitempty" jsonschema:"process exit status, absent if the agent never started"`
	SessionID    string `json:"session_id,omitempty" jsonschema:"the agent's own session id"`
	Summary      string `json:"summary,omitempty" jsonschema:"what the agent said it did - a claim recorded for diagnosis, never treated as evidence"`
	FinishReason string `json:"finish_reason,omitempty" jsonschema:"the backend's own completion reason"`
	ChangedFiles int    `json:"changed_files" jsonschema:"files changed, counted by aidev from git rather than reported by the agent"`
	DurationMS   int64  `json:"duration_ms" jsonschema:"how long the agent ran"`

	StdoutTruncated bool `json:"stdout_truncated,omitempty" jsonschema:"true if captured output hit the size limit"`
	StderrTruncated bool `json:"stderr_truncated,omitempty" jsonschema:"true if captured error output hit the size limit"`
}

// NewWorker converts a domain worker run.
func NewWorker(r task.WorkerRun) *Worker {
	return &Worker{
		Backend:         r.Backend,
		Status:          r.Status.String(),
		FailureKind:     r.FailureKind.String(),
		ExitCode:        r.ExitCode,
		SessionID:       r.SessionID,
		Summary:         r.Summary,
		FinishReason:    r.FinishReason,
		ChangedFiles:    r.ChangedFiles,
		DurationMS:      r.Duration().Milliseconds(),
		StdoutTruncated: r.StdoutTruncated,
		StderrTruncated: r.StderrTruncated,
	}
}

// Verification is one verification step that aidev ran.
type Verification struct {
	Step       int    `json:"step" jsonschema:"position in the task's verification list"`
	Command    string `json:"command" jsonschema:"the command as aidev ran it"`
	Status     string `json:"status" jsonschema:"PASSED, FAILED, TIMED_OUT, CANCELLED or SKIPPED"`
	ExitCode   *int   `json:"exit_code,omitempty" jsonschema:"process exit status; absent for a skipped step"`
	DurationMS int64  `json:"duration_ms" jsonschema:"how long the command took"`
	Stdout     string `json:"stdout,omitempty" jsonschema:"captured output; included for failures, or in full on request"`
	Stderr     string `json:"stderr,omitempty" jsonschema:"captured error output"`
}

// OutputLimit bounds how much of a failing step's output is included when the
// caller has not asked for everything. A planner needs enough to diagnose and not
// so much that the result is unreadable.
const OutputLimit = 2000

// NewVerifications converts domain verification runs. When includeOutput is
// false, only a failing step's output is included, trimmed to OutputLimit.
func NewVerifications(runs []task.VerificationRun, includeOutput bool) []Verification {
	views := make([]Verification, 0, len(runs))
	for _, r := range runs {
		v := Verification{
			Step:       r.StepIndex,
			Command:    r.Command,
			Status:     r.Status.String(),
			ExitCode:   r.ExitCode,
			DurationMS: r.Duration.Milliseconds(),
		}
		switch {
		case includeOutput:
			v.Stdout = r.Stdout
			v.Stderr = r.Stderr
		case r.Status != task.VerificationPassed && r.Status != task.VerificationSkipped:
			v.Stdout = Tail(r.Stdout, OutputLimit)
			v.Stderr = Tail(r.Stderr, OutputLimit)
		}
		views = append(views, v)
	}
	return views
}

// Worktree is the isolated checkout an attempt used.
type Worktree struct {
	Path       string `json:"path" jsonschema:"absolute path; present on disk unless the status is REMOVED"`
	Branch     string `json:"branch" jsonschema:"branch the work is on"`
	Status     string `json:"status" jsonschema:"ACTIVE, REMOVED, or RETAINED when kept after a failure for inspection"`
	BaseCommit string `json:"base_commit,omitempty" jsonschema:"commit the branch started from"`
	HeadCommit string `json:"head_commit,omitempty" jsonschema:"commit holding the work, when it was committed"`
}

// NewWorktree converts a domain worktree record.
func NewWorktree(w task.Worktree) *Worktree {
	return &Worktree{
		Path:       w.Path,
		Branch:     w.Branch,
		Status:     w.Status.String(),
		BaseCommit: w.BaseCommit,
		HeadCommit: w.HeadCommit,
	}
}

// Approval is a human decision gating a task.
type Approval struct {
	ID        string `json:"id" jsonschema:"the approval's UUID"`
	Status    string `json:"status" jsonschema:"PENDING, GRANTED or DENIED"`
	Reason    string `json:"reason,omitempty" jsonschema:"why it was requested or decided"`
	DecidedBy string `json:"decided_by,omitempty" jsonschema:"who decided"`
}

// NewApproval converts a domain approval.
func NewApproval(a task.Approval) *Approval {
	return &Approval{
		ID:        a.ID.String(),
		Status:    a.Status.String(),
		Reason:    a.Reason,
		DecidedBy: a.DecidedBy,
	}
}

// Result is everything known about a task's latest attempt.
type Result struct {
	Task         Task           `json:"task" jsonschema:"the task itself"`
	Attempt      *Attempt       `json:"attempt,omitempty" jsonschema:"the latest attempt, absent if the task has never run"`
	Worker       *Worker        `json:"worker,omitempty" jsonschema:"what the agent did"`
	Verification []Verification `json:"verification,omitempty" jsonschema:"the verification aidev ran; this alone decides success"`
	Worktree     *Worktree      `json:"worktree,omitempty" jsonschema:"the isolated checkout used"`
	Approval     *Approval      `json:"approval,omitempty" jsonschema:"the most recent approval record"`
	Message      string         `json:"message,omitempty" jsonschema:"one-line human summary of the outcome"`
}

// Event is one entry of a task's history.
type Event struct {
	Seq       int64  `json:"seq" jsonschema:"total ordering; pass the highest seen as after_seq to resume"`
	Type      string `json:"type" jsonschema:"event type, for example task.verification_completed"`
	CreatedAt string `json:"created_at" jsonschema:"RFC3339 timestamp"`
	AttemptID string `json:"attempt_id,omitempty" jsonschema:"the attempt this event belongs to, when it belongs to one"`
	Payload   any    `json:"payload,omitempty" jsonschema:"event-specific detail"`
}

// NewEvent converts a domain event.
func NewEvent(e event.Event, includePayload bool) Event {
	v := Event{
		Seq:       e.Seq,
		Type:      e.Type.String(),
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if e.AttemptID != nil {
		v.AttemptID = e.AttemptID.String()
	}
	if includePayload && len(e.Payload) > 0 {
		v.Payload = RawJSON(e.Payload)
	}
	return v
}

// RawJSON passes stored JSONB through without re-encoding it.
type RawJSON []byte

// MarshalJSON implements json.Marshaler.
func (r RawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// Tail keeps the end of a long output, which is where a failure's explanation
// usually is.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// NewResult assembles a Result from whatever is known. Absent sections are
// omitted rather than present and empty, so a reader can tell "has not run" from
// "ran and produced nothing".
func NewResult(
	t task.Task,
	attempt *task.TaskAttempt,
	workerRun *task.WorkerRun,
	runs []task.VerificationRun,
	worktree *task.Worktree,
	approval *task.Approval,
	message string,
	includeOutput bool,
) Result {
	result := Result{Task: NewTask(t), Message: message}
	if attempt != nil && attempt.ID != uuid.Nil {
		result.Attempt = NewAttempt(*attempt)
	}
	if workerRun != nil {
		result.Worker = NewWorker(*workerRun)
	}
	if len(runs) > 0 {
		result.Verification = NewVerifications(runs, includeOutput)
	}
	if worktree != nil {
		result.Worktree = NewWorktree(*worktree)
	}
	if approval != nil {
		result.Approval = NewApproval(*approval)
	}
	return result
}
