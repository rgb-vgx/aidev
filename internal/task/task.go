// Package task holds aidev's core domain: the task, its lifecycle state
// machine, its attempts, and the vocabulary used to describe why something
// failed. It has no dependencies outside the standard library and the UUID
// type, so every other package may depend on it.
package task

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Limits on user-supplied text. They exist so that a mistake or a runaway
// caller cannot push unbounded data into the database.
const (
	MaxTitleLength              = 200
	MaxDescriptionLength        = 32 * 1024
	MaxAcceptanceCriteriaLength = 32 * 1024
	MaxVerificationSteps        = 20
	MaxPriority                 = 1000
	MinPriority                 = -1000
	MaxRetriesLimit             = 10
)

// Project is a git repository aidev can run tasks against.
type Project struct {
	ID            uuid.UUID
	Name          string
	RepoPath      string // absolute path to the repository's main working tree
	DefaultBranch string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Task is a unit of work delegated to an agent and verified by aidev.
type Task struct {
	ID uuid.UUID

	// Ref is the human-facing identifier (for example TASK-000007). It is
	// assigned by the database on insert and is what a person types on the CLI
	// or passes through MCP.
	Ref string

	ProjectID          uuid.UUID
	Title              string
	Description        string
	Agent              string // agent name passed to the backend, e.g. "build"
	Priority           int    // higher runs first
	Status             Status
	AcceptanceCriteria string

	// Verification is the set of commands aidev runs itself. At least one is
	// required: aidev refuses to accept a task whose success it could not
	// independently establish.
	Verification []VerificationStep

	// MaxRetries is recorded for the future retry feature. The MVP never
	// retries automatically.
	MaxRetries int

	RequiresApproval bool

	// BaseRef is the git ref the task's worktree branches from. Empty means the
	// project's default branch.
	BaseRef string

	// Timeout bounds the agent run. Zero means "use the configured default".
	Timeout time.Duration

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewTaskInput is the validated input for creating a task. It is separate from
// Task so that server-assigned fields (ID, Ref, Status, timestamps) cannot be
// supplied by a caller.
type NewTaskInput struct {
	ProjectID          uuid.UUID
	Title              string
	Description        string
	Agent              string
	Priority           int
	AcceptanceCriteria string
	Verification       []VerificationStep
	MaxRetries         int
	RequiresApproval   bool
	BaseRef            string
	Timeout            time.Duration
}

// ValidationError reports one or more rejected fields.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid task: " + e.Problems[0]
	}
	return "invalid task:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// ErrVerificationRequired is the product's central rule as an error: a task aidev
// cannot verify is a task aidev cannot honestly report on, so it is not accepted at
// all. It is exported because the MCP server refuses such a request before it
// reaches the database, and both refusals must say the same thing.
var ErrVerificationRequired = errors.New("at least one verification command is required, because only aidev's own " +
	"verification can mark a task succeeded (for example: go test ./...)")

// New validates input and returns a task in StatusPending. Ref is left empty
// for the store to assign.
func New(input NewTaskInput, defaultAgent string) (Task, error) {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if input.ProjectID == uuid.Nil {
		add("project is required")
	}

	title := strings.TrimSpace(input.Title)
	switch {
	case title == "":
		add("title is required")
	case len(title) > MaxTitleLength:
		add("title is %d characters, limit is %d", len(title), MaxTitleLength)
	}

	if len(input.Description) > MaxDescriptionLength {
		add("description is %d bytes, limit is %d", len(input.Description), MaxDescriptionLength)
	}
	if len(input.AcceptanceCriteria) > MaxAcceptanceCriteriaLength {
		add("acceptance criteria is %d bytes, limit is %d", len(input.AcceptanceCriteria), MaxAcceptanceCriteriaLength)
	}

	agent := strings.TrimSpace(input.Agent)
	if agent == "" {
		agent = strings.TrimSpace(defaultAgent)
	}
	switch {
	case agent == "":
		add("agent is required and no default is configured")
	case strings.ContainsAny(agent, " \t\n"):
		add("agent %q must not contain whitespace", agent)
	}

	// At least one verification command is mandatory. This is the product's
	// central rule expressed as a constraint: a task aidev cannot verify is a
	// task aidev cannot honestly report on, so it is not accepted at all.
	switch {
	case len(input.Verification) == 0:
		add("%s", ErrVerificationRequired)
	case len(input.Verification) > MaxVerificationSteps:
		add("%d verification commands exceed the limit of %d", len(input.Verification), MaxVerificationSteps)
	}
	for i, step := range input.Verification {
		if err := step.Validate(); err != nil {
			add("verification command %d: %v", i+1, err)
		}
	}

	if input.Priority < MinPriority || input.Priority > MaxPriority {
		add("priority %d is outside %d..%d", input.Priority, MinPriority, MaxPriority)
	}
	if input.MaxRetries < 0 || input.MaxRetries > MaxRetriesLimit {
		add("max retries %d is outside 0..%d", input.MaxRetries, MaxRetriesLimit)
	}
	if input.Timeout < 0 {
		add("timeout must not be negative")
	}
	if ref := strings.TrimSpace(input.BaseRef); ref != "" && strings.ContainsAny(ref, " \t\n") {
		add("base ref %q must not contain whitespace", ref)
	}

	if len(problems) > 0 {
		return Task{}, &ValidationError{Problems: problems}
	}

	now := time.Now().UTC()
	return Task{
		ID:                 uuid.Must(uuid.NewV7()),
		ProjectID:          input.ProjectID,
		Title:              title,
		Description:        input.Description,
		Agent:              agent,
		Priority:           input.Priority,
		Status:             StatusPending,
		AcceptanceCriteria: input.AcceptanceCriteria,
		Verification:       input.Verification,
		MaxRetries:         input.MaxRetries,
		RequiresApproval:   input.RequiresApproval,
		BaseRef:            strings.TrimSpace(input.BaseRef),
		Timeout:            input.Timeout,
		CreatedAt:          now,
		UpdatedAt:          now,
	}, nil
}

// TransitionTo applies a state change in memory after checking the state
// machine. Persisting it is the store's job; this method exists so that no code
// path can assign Status directly without the check.
func (t *Task) TransitionTo(next Status) error {
	if !t.Status.CanTransitionTo(next) {
		return &TransitionError{From: t.Status, To: next}
	}
	t.Status = next
	t.UpdatedAt = time.Now().UTC()
	return nil
}

// EffectiveTimeout returns the task's timeout, falling back to the default.
func (t Task) EffectiveTimeout(def time.Duration) time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return def
}

// Identifier returns the best human-facing label for the task.
func (t Task) Identifier() string {
	if t.Ref != "" {
		return t.Ref
	}
	return t.ID.String()
}
