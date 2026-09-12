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

const taskColumns = `id, ref, project_id, title, description, agent, priority, status,
	acceptance_criteria, verification, max_retries, requires_approval, base_ref,
	timeout_seconds, created_at, updated_at`

// CreateTask persists a new task and returns it with the database-assigned
// reference filled in.
func (s *Store) CreateTask(ctx context.Context, t task.Task) (task.Task, error) {
	verification, err := json.Marshal(t.Verification)
	if err != nil {
		return task.Task{}, fmt.Errorf("encode verification steps: %w", err)
	}

	row := s.db.QueryRow(ctx, `
		INSERT INTO tasks (
			id, project_id, title, description, agent, priority, status,
			acceptance_criteria, verification, max_retries, requires_approval,
			base_ref, timeout_seconds
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+taskColumns,
		t.ID, t.ProjectID, t.Title, t.Description, t.Agent, t.Priority, string(t.Status),
		t.AcceptanceCriteria, verification, t.MaxRetries, t.RequiresApproval,
		t.BaseRef, int(t.Timeout.Seconds()))

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
}

// DefaultTaskListLimit bounds an unfiltered listing so that a caller cannot
// accidentally pull the whole table through MCP.
const DefaultTaskListLimit = 50

// MaxTaskListLimit caps an explicit limit.
const MaxTaskListLimit = 500

// ListTasks returns tasks newest first.
func (s *Store) ListTasks(ctx context.Context, filter TaskFilter) ([]task.Task, error) {
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
	rows, err := s.db.Query(ctx, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE ($1::uuid IS NULL OR project_id = $1)
		  AND (cardinality($2::text[]) = 0 OR status = ANY($2))
		ORDER BY created_at DESC, ref DESC
		LIMIT $3 OFFSET $4`,
		nullUUID(filter.ProjectID), statuses, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", classify(err))
	}
	defer rows.Close()

	var tasks []task.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks: %w", classify(err))
	}
	return tasks, nil
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
		verification   []byte
		timeoutSeconds int
	)
	err := row.Scan(
		&t.ID, &t.Ref, &t.ProjectID, &t.Title, &t.Description, &t.Agent, &t.Priority,
		&status, &t.AcceptanceCriteria, &verification, &t.MaxRetries, &t.RequiresApproval,
		&t.BaseRef, &timeoutSeconds, &t.CreatedAt, &t.UpdatedAt)
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

	if len(verification) > 0 {
		if err := json.Unmarshal(verification, &t.Verification); err != nil {
			return task.Task{}, fmt.Errorf("task %s has unreadable verification steps: %w", t.ID, err)
		}
	}
	t.Timeout = time.Duration(timeoutSeconds) * time.Second
	return t, nil
}

// nullUUID maps the zero UUID onto SQL NULL so that "any project" can be
// expressed as a parameter rather than as a different query.
func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
