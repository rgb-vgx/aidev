package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aidev/internal/task"
)

const approvalColumns = `id, task_id, status, reason, requested_at, decided_at, decided_by`

// RequestApproval opens a pending approval for a task.
//
// A partial unique index allows only one pending request per task, so a second
// concurrent request fails with ErrAlreadyExists rather than creating a
// duplicate that a human would have to reconcile.
func (s *Store) RequestApproval(ctx context.Context, taskID uuid.UUID, reason string) (task.Approval, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO approvals (id, task_id, status, reason)
		VALUES ($1, $2, 'PENDING', $3)
		RETURNING `+approvalColumns,
		uuid.Must(uuid.NewV7()), taskID, reason)

	a, err := scanApproval(row)
	if err != nil {
		return task.Approval{}, fmt.Errorf("request approval for task %s: %w", taskID, err)
	}
	return a, nil
}

// DecideApproval records a human decision on the task's pending request.
func (s *Store) DecideApproval(ctx context.Context, taskID uuid.UUID, status task.ApprovalStatus, decidedBy, reason string) (task.Approval, error) {
	if status != task.ApprovalGranted && status != task.ApprovalDenied {
		return task.Approval{}, fmt.Errorf("decide approval for task %s: %q is not a decision", taskID, status)
	}

	row := s.db.QueryRow(ctx, `
		UPDATE approvals
		SET status = $2, decided_at = now(), decided_by = $3,
		    reason = CASE WHEN $4 = '' THEN reason ELSE $4 END
		WHERE task_id = $1 AND status = 'PENDING'
		RETURNING `+approvalColumns,
		taskID, string(status), decidedBy, reason)

	a, err := scanApproval(row)
	if err != nil {
		return task.Approval{}, fmt.Errorf("decide approval for task %s: no pending request: %w", taskID, err)
	}
	return a, nil
}

// LatestApproval returns a task's most recent approval record.
func (s *Store) LatestApproval(ctx context.Context, taskID uuid.UUID) (task.Approval, error) {
	row := s.db.QueryRow(ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE task_id = $1
		 ORDER BY requested_at DESC LIMIT 1`, taskID)
	a, err := scanApproval(row)
	if err != nil {
		return task.Approval{}, fmt.Errorf("latest approval for task %s: %w", taskID, err)
	}
	return a, nil
}

// ListApprovals returns a task's approval history, newest first.
func (s *Store) ListApprovals(ctx context.Context, taskID uuid.UUID) ([]task.Approval, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE task_id = $1 ORDER BY requested_at DESC`,
		taskID)
	if err != nil {
		return nil, fmt.Errorf("list approvals for task %s: %w", taskID, classify(err))
	}
	defer rows.Close()

	var out []task.Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("list approvals for task %s: %w", taskID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list approvals for task %s: %w", taskID, classify(err))
	}
	return out, nil
}

func scanApproval(row scanner) (task.Approval, error) {
	var (
		a         task.Approval
		status    string
		decidedAt *time.Time
	)
	err := row.Scan(&a.ID, &a.TaskID, &status, &a.Reason, &a.RequestedAt, &decidedAt, &a.DecidedBy)
	if err != nil {
		return task.Approval{}, classify(err)
	}
	parsed, err := task.ParseApprovalStatus(status)
	if err != nil {
		return task.Approval{}, fmt.Errorf("approval %s: %w", a.ID, err)
	}
	a.Status = parsed
	a.DecidedAt = decidedAt
	return a, nil
}
