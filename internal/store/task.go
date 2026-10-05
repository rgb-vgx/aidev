package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"aidev/internal/task"
)

const taskColumns = `id, ref, project_id, title, description, agent, model, hardness, priority, status,
	acceptance_criteria, verification, protected_paths, setup_steps, verification_mode,
	max_retries, requires_approval, expect_fail_on_base, base_ref, timeout_seconds, created_at, updated_at,
	base_commit_at_create`

// CreateTask persists a new task and returns it with the database-assigned
// reference filled in.
func (s *Store) CreateTask(ctx context.Context, t task.Task) (task.Task, error) {
	verification, err := json.Marshal(t.Verification)
	if err != nil {
		return task.Task{}, fmt.Errorf("encode verification steps: %w", err)
	}
	// A nil slice marshals to null, which the column's array CHECK rejects; a
	// task that protects nothing — or prepares nothing — stores an empty
	// array instead.
	protectedList := t.ProtectedPaths
	if protectedList == nil {
		protectedList = []string{}
	}
	protected, err := json.Marshal(protectedList)
	if err != nil {
		return task.Task{}, fmt.Errorf("encode protected paths: %w", err)
	}
	setupList := t.SetupSteps
	if setupList == nil {
		setupList = []task.VerificationStep{}
	}
	setup, err := json.Marshal(setupList)
	if err != nil {
		return task.Task{}, fmt.Errorf("encode setup steps: %w", err)
	}

	row := s.db.QueryRow(ctx, `
		INSERT INTO tasks (
			id, project_id, title, description, agent, model, hardness, priority, status,
			acceptance_criteria, verification, protected_paths, setup_steps, verification_mode,
			max_retries, requires_approval, expect_fail_on_base, base_ref, timeout_seconds,
			base_commit_at_create
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING `+taskColumns,
		t.ID, t.ProjectID, t.Title, t.Description, t.Agent, t.Model, string(t.Hardness), t.Priority, string(t.Status),
		t.AcceptanceCriteria, verification, protected, setup, string(t.VerificationMode),
		t.MaxRetries, t.RequiresApproval, t.ExpectFailOnBase,
		t.BaseRef, int(t.Timeout.Seconds()), t.BaseCommitAtCreate)

	created, err := scanTask(row)
	if err != nil {
		return task.Task{}, fmt.Errorf("create task: %w", err)
	}
	return created, nil
}

// GetTask returns a task by id.
func (s *Store) GetTask(ctx context.Context, id uuid.UUID) (task.Task, error) {
	row := s.db.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1`, id)
	t, err := scanTask(row)
	if err != nil {
		return task.Task{}, fmt.Errorf("get task %s: %w", id, err)
	}
	return t, nil
}

// GetTaskByRef returns a task by its human-facing reference, case-insensitively
// so that "task-000007" works as well as "TASK-000007".
func (s *Store) GetTaskByRef(ctx context.Context, ref string) (task.Task, error) {
	row := s.db.QueryRow(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE upper(ref) = upper($1)`, strings.TrimSpace(ref))
	t, err := scanTask(row)
	if err != nil {
		return task.Task{}, fmt.Errorf("get task %s: %w", ref, err)
	}
	return t, nil
}

// ResolveTask accepts either a UUID or a reference such as TASK-000007, so that
// the CLI and MCP layers do not each have to guess which one they were given.
func (s *Store) ResolveTask(ctx context.Context, idOrRef string) (task.Task, error) {
	trimmed := strings.TrimSpace(idOrRef)
	if trimmed == "" {
		return task.Task{}, fmt.Errorf("no task identifier given: %w", ErrNotFound)
	}
	if id, err := uuid.Parse(trimmed); err == nil {
		return s.GetTask(ctx, id)
	}
	return s.GetTaskByRef(ctx, trimmed)
}

// TaskFilter narrows a task listing.
type TaskFilter struct {
	ProjectID uuid.UUID     // zero means any project
	Statuses  []task.Status // empty means any status
	Limit     int           // zero means the default
	Offset    int
	// Unapplied means the operator's question — SUCCEEDED and not currently
	// applied — so the listing holds only SUCCEEDED tasks whose newest apply
	// event is not task.applied (never applied, or applied and then undone).
	Unapplied bool
}

// DefaultTaskListLimit bounds an unfiltered listing so that a caller cannot
// accidentally pull the whole table through MCP.
const DefaultTaskListLimit = 50

// MaxTaskListLimit caps an explicit limit.
const MaxTaskListLimit = 500

// TaskListItem is a task together with the repository it belongs to and its
// derived apply state, which is what an operator needs to make sense of a
// listing: which project the task is for, and whether its verified result has
// been taken into their branch.
type TaskListItem struct {
	Task     task.Task
	RepoPath string
	Apply    task.Apply
}

// ListTasks returns tasks newest first, each with its repository path and its
// apply state derived from the newest task.applied / task.apply_undone event.
func (s *Store) ListTasks(ctx context.Context, filter TaskFilter) ([]TaskListItem, error) {
	limit := filter.Limit
	switch {
	case limit <= 0:
		limit = DefaultTaskListLimit
	case limit > MaxTaskListLimit:
		limit = MaxTaskListLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	statuses := make([]string, 0, len(filter.Statuses))
	for _, st := range filter.Statuses {
		if !st.Valid() {
			return nil, fmt.Errorf("list tasks: %q is not a valid status", st)
		}
		statuses = append(statuses, string(st))
	}

	// A single query with "filter is empty" predicates keeps the SQL static,
	// which is easier to read and leaves no room for accidental injection.
	// The repository comes from a join with projects, and the apply state
	// from the newest task.applied / task.apply_undone event per task; the
	// events_task_seq_idx (task_id, seq) index serves that lookup.
	rows, err := s.db.Query(ctx, `
		SELECT t.id, t.ref, t.project_id, t.title, t.description, t.agent, t.model, t.hardness, t.priority, t.status,
			t.acceptance_criteria, t.verification, t.protected_paths, t.setup_steps, t.verification_mode,
			t.max_retries, t.requires_approval, t.expect_fail_on_base, t.base_ref, t.timeout_seconds, t.created_at, t.updated_at,
			t.base_commit_at_create,
			p.repo_path, e.type, e.payload, e.created_at
		FROM tasks t
		JOIN projects p ON p.id = t.project_id
		LEFT JOIN LATERAL (
			SELECT type, payload, created_at
			FROM events
			WHERE task_id = t.id AND type IN ('task.applied', 'task.apply_undone')
			ORDER BY seq DESC
			LIMIT 1
		) e ON true
		WHERE ($1::uuid IS NULL OR t.project_id = $1)
		  AND (cardinality($2::text[]) = 0 OR t.status = ANY($2))
		  AND (NOT $5 OR t.status = 'SUCCEEDED')
		  AND (NOT $5 OR e.type IS DISTINCT FROM 'task.applied')
		ORDER BY t.created_at DESC, t.ref DESC
		LIMIT $3 OFFSET $4`,
		nullUUID(filter.ProjectID), statuses, limit, offset, filter.Unapplied)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", classify(err))
	}
	defer rows.Close()

	var items []TaskListItem
	for rows.Next() {
		item, err := scanTaskListItem(rows)
		if err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks: %w", classify(err))
	}
	return items, nil
}

// TransitionTask moves a task from one status to another.
//
// The transition is checked against the domain state machine first, then
// applied with a compare-and-set on the current status. If another process
// changed the task in between, the update matches no row and ErrConflict is
// returned rather than overwriting that change.
func (s *Store) TransitionTask(ctx context.Context, id uuid.UUID, from, to task.Status) error {
	if !from.CanTransitionTo(to) {
		return &task.TransitionError{From: from, To: to}
	}

	tag, err := s.db.Exec(ctx,
		`UPDATE tasks SET status = $3 WHERE id = $1 AND status = $2`,
		id, string(from), string(to))
	if err != nil {
		return fmt.Errorf("transition task %s %s -> %s: %w", id, from, to, classify(err))
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "no such task" from "status moved underneath us": the
		// first is a caller error, the second is normal concurrency.
		current, getErr := s.GetTask(ctx, id)
		if errors.Is(getErr, ErrNotFound) {
			return fmt.Errorf("transition task %s: %w", id, ErrNotFound)
		}
		if getErr != nil {
			return fmt.Errorf("transition task %s %s -> %s: %w", id, from, to, getErr)
		}
		return fmt.Errorf("transition task %s %s -> %s: expected status %s but found %s: %w",
			current.Identifier(), from, to, from, current.Status, ErrConflict)
	}
	return nil
}

func scanTask(row scanner) (task.Task, error) {
	var (
		t              task.Task
		status         string
		hardness       string
		verification   []byte
		protectedPaths []byte
		setupSteps     []byte
		mode           string
		timeoutSeconds int
	)
	err := row.Scan(
		&t.ID, &t.Ref, &t.ProjectID, &t.Title, &t.Description, &t.Agent, &t.Model, &hardness, &t.Priority,
		&status, &t.AcceptanceCriteria, &verification, &protectedPaths, &setupSteps, &mode,
		&t.MaxRetries, &t.RequiresApproval, &t.ExpectFailOnBase,
		&t.BaseRef, &timeoutSeconds, &t.CreatedAt, &t.UpdatedAt, &t.BaseCommitAtCreate)
	if err != nil {
		return task.Task{}, classify(err)
	}

	parsed, err := task.ParseStatus(status)
	if err != nil {
		// The CHECK constraint makes this unreachable unless the enum and the
		// migration have diverged, which is worth reporting loudly.
		return task.Task{}, fmt.Errorf("task %s has unrecognised status: %w", t.ID, err)
	}
	t.Status = parsed

	if hardness != "" {
		h, err := task.ParseHardness(hardness)
		if err != nil {
			// The CHECK constraint makes this unreachable unless the enum and
			// the migration have diverged, which is worth reporting loudly.
			return task.Task{}, fmt.Errorf("task %s has unrecognised hardness: %w", t.ID, err)
		}
		t.Hardness = h
	}

	if len(verification) > 0 {
		if err := json.Unmarshal(verification, &t.Verification); err != nil {
			return task.Task{}, fmt.Errorf("task %s has unreadable verification steps: %w", t.ID, err)
		}
	}
	// The column defaults to '[]', so this is empty rather than absent for
	// every task that protects nothing; only an unreadable array is an error.
	if len(protectedPaths) > 0 {
		if err := json.Unmarshal(protectedPaths, &t.ProtectedPaths); err != nil {
			return task.Task{}, fmt.Errorf("task %s has unreadable protected paths: %w", t.ID, err)
		}
	}
	// Same for setup steps: the default '[]' means "no preparation", which
	// is a valid task, not a missing one.
	if len(setupSteps) > 0 {
		if err := json.Unmarshal(setupSteps, &t.SetupSteps); err != nil {
			return task.Task{}, fmt.Errorf("task %s has unreadable setup steps: %w", t.ID, err)
		}
	}
	// The CHECK constraint makes an unknown mode unreachable unless the enum
	// and the migration have diverged, which is worth reporting loudly.
	parsedMode, err := task.ParseVerificationMode(mode)
	if err != nil {
		return task.Task{}, fmt.Errorf("task %s has unrecognised verification mode: %w", t.ID, err)
	}
	t.VerificationMode = parsedMode
	t.Timeout = time.Duration(timeoutSeconds) * time.Second
	return t, nil
}

// scanTaskListItem reads one row of the ListTasks join: the task itself, the
// repository path from projects, and the newest apply event for the derived
// state. A task with no apply event carries ApplyNever.
func scanTaskListItem(row scanner) (TaskListItem, error) {
	var (
		item           TaskListItem
		status         string
		hardness       string
		verification   []byte
		protectedPaths []byte
		setupSteps     []byte
		mode           string
		timeoutSeconds int
		eventType      *string
		eventPayload   []byte
		eventAt        *time.Time
	)
	t := &item.Task
	err := row.Scan(
		&t.ID, &t.Ref, &t.ProjectID, &t.Title, &t.Description, &t.Agent, &t.Model, &hardness, &t.Priority,
		&status, &t.AcceptanceCriteria, &verification, &protectedPaths, &setupSteps, &mode,
		&t.MaxRetries, &t.RequiresApproval, &t.ExpectFailOnBase,
		&t.BaseRef, &timeoutSeconds, &t.CreatedAt, &t.UpdatedAt, &t.BaseCommitAtCreate,
		&item.RepoPath, &eventType, &eventPayload, &eventAt)
	if err != nil {
		return TaskListItem{}, classify(err)
	}

	parsed, err := task.ParseStatus(status)
	if err != nil {
		return TaskListItem{}, fmt.Errorf("task %s has unrecognised status: %w", t.ID, err)
	}
	t.Status = parsed

	if hardness != "" {
		h, err := task.ParseHardness(hardness)
		if err != nil {
			return TaskListItem{}, fmt.Errorf("task %s has unrecognised hardness: %w", t.ID, err)
		}
		t.Hardness = h
	}

	if len(verification) > 0 {
		if err := json.Unmarshal(verification, &t.Verification); err != nil {
			return TaskListItem{}, fmt.Errorf("task %s has unreadable verification steps: %w", t.ID, err)
		}
	}
	if len(protectedPaths) > 0 {
		if err := json.Unmarshal(protectedPaths, &t.ProtectedPaths); err != nil {
			return TaskListItem{}, fmt.Errorf("task %s has unreadable protected paths: %w", t.ID, err)
		}
	}
	if len(setupSteps) > 0 {
		if err := json.Unmarshal(setupSteps, &t.SetupSteps); err != nil {
			return TaskListItem{}, fmt.Errorf("task %s has unreadable setup steps: %w", t.ID, err)
		}
	}
	parsedMode, err := task.ParseVerificationMode(mode)
	if err != nil {
		return TaskListItem{}, fmt.Errorf("task %s has unrecognised verification mode: %w", t.ID, err)
	}
	t.VerificationMode = parsedMode
	t.Timeout = time.Duration(timeoutSeconds) * time.Second

	apply, err := decodeApply(t.ID, eventType, eventPayload, eventAt)
	if err != nil {
		return TaskListItem{}, err
	}
	item.Apply = apply
	return item, nil
}

// nullUUID maps the zero UUID onto SQL NULL so that "any project" can be
// expressed as a parameter rather than as a different query.
func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// LockTask takes the task's row lock until the caller's transaction ends, so
// two operations on the same task — two `aidev task apply` at once — run one
// after the other. It must run inside a transaction.
func (s *Store) LockTask(ctx context.Context, id uuid.UUID) error {
	if s.pool != nil {
		return fmt.Errorf("lock task %s: LockTask must run inside a transaction", id)
	}
	var one int
	if err := s.db.QueryRow(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, id).Scan(&one); err != nil {
		return fmt.Errorf("lock task %s: %w", id, classify(err))
	}
	return nil
}
