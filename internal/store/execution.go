package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aidev/internal/task"
)

const attemptColumns = `id, task_id, attempt_number, status, failure_kind, error, started_at, finished_at`

// NextAttemptNumber returns the number the next attempt for a task should use.
//
// Callers must run this inside the same transaction as the matching
// CreateAttempt; the UNIQUE (task_id, attempt_number) constraint is what
// ultimately prevents two concurrent callers from both claiming a number.
func (s *Store) NextAttemptNumber(ctx context.Context, taskID uuid.UUID) (int, error) {
	var next int
	err := s.db.QueryRow(ctx,
		`SELECT coalesce(max(attempt_number), 0) + 1 FROM task_attempts WHERE task_id = $1`,
		taskID).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("next attempt number for task %s: %w", taskID, classify(err))
	}
	return next, nil
}

// CreateAttempt persists a new, running attempt.
func (s *Store) CreateAttempt(ctx context.Context, a task.TaskAttempt) (task.TaskAttempt, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO task_attempts (id, task_id, attempt_number, status, failure_kind, error, started_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+attemptColumns,
		a.ID, a.TaskID, a.AttemptNumber, string(a.Status), string(a.FailureKind), a.Error, a.StartedAt)

	created, err := scanAttempt(row)
	if err != nil {
		return task.TaskAttempt{}, fmt.Errorf("create attempt %d for task %s: %w", a.AttemptNumber, a.TaskID, err)
	}
	return created, nil
}

// FinishAttempt records an attempt's outcome. It refuses to finish an attempt
// twice, so a double-completion bug surfaces as a conflict instead of quietly
// rewriting the first result.
func (s *Store) FinishAttempt(ctx context.Context, id uuid.UUID, status task.AttemptStatus, kind task.FailureKind, errMsg string) error {
	if status == task.AttemptRunning {
		return fmt.Errorf("finish attempt %s: RUNNING is not an outcome", id)
	}
	if !status.Valid() {
		return fmt.Errorf("finish attempt %s: %q is not a valid attempt status", id, status)
	}
	if !kind.Valid() {
		return fmt.Errorf("finish attempt %s: %q is not a valid failure kind", id, kind)
	}

	tag, err := s.db.Exec(ctx, `
		UPDATE task_attempts
		SET status = $2, failure_kind = $3, error = $4, finished_at = now()
		WHERE id = $1 AND status = 'RUNNING'`,
		id, string(status), string(kind), errMsg)
	if err != nil {
		return fmt.Errorf("finish attempt %s: %w", id, classify(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finish attempt %s: not found or already finished: %w", id, ErrConflict)
	}
	return nil
}

// GetAttempt returns one attempt.
func (s *Store) GetAttempt(ctx context.Context, id uuid.UUID) (task.TaskAttempt, error) {
	row := s.db.QueryRow(ctx, `SELECT `+attemptColumns+` FROM task_attempts WHERE id = $1`, id)
	a, err := scanAttempt(row)
	if err != nil {
		return task.TaskAttempt{}, fmt.Errorf("get attempt %s: %w", id, err)
	}
	return a, nil
}

// ListAttempts returns a task's attempts, most recent first.
func (s *Store) ListAttempts(ctx context.Context, taskID uuid.UUID) ([]task.TaskAttempt, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+attemptColumns+` FROM task_attempts WHERE task_id = $1 ORDER BY attempt_number DESC`,
		taskID)
	if err != nil {
		return nil, fmt.Errorf("list attempts for task %s: %w", taskID, classify(err))
	}
	defer rows.Close()

	var attempts []task.TaskAttempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("list attempts for task %s: %w", taskID, err)
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attempts for task %s: %w", taskID, classify(err))
	}
	return attempts, nil
}

// LatestAttempt returns a task's most recent attempt.
func (s *Store) LatestAttempt(ctx context.Context, taskID uuid.UUID) (task.TaskAttempt, error) {
	row := s.db.QueryRow(ctx,
		`SELECT `+attemptColumns+` FROM task_attempts WHERE task_id = $1
		 ORDER BY attempt_number DESC LIMIT 1`, taskID)
	a, err := scanAttempt(row)
	if err != nil {
		return task.TaskAttempt{}, fmt.Errorf("latest attempt for task %s: %w", taskID, err)
	}
	return a, nil
}

func scanAttempt(row scanner) (task.TaskAttempt, error) {
	var (
		a      task.TaskAttempt
		status string
		kind   string
	)
	err := row.Scan(&a.ID, &a.TaskID, &a.AttemptNumber, &status, &kind, &a.Error, &a.StartedAt, &a.FinishedAt)
	if err != nil {
		return task.TaskAttempt{}, classify(err)
	}
	a.Status = task.AttemptStatus(status)
	a.FailureKind = task.FailureKind(kind)
	if !a.Status.Valid() {
		return task.TaskAttempt{}, fmt.Errorf("attempt %s has unrecognised status %q", a.ID, status)
	}
	return a, nil
}

const worktreeColumns = `id, attempt_id, path, branch, base_commit, head_commit, status, created_at, removed_at`

// CreateWorktree records an isolated workspace.
func (s *Store) CreateWorktree(ctx context.Context, w task.Worktree) (task.Worktree, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO worktrees (id, attempt_id, path, branch, base_commit, head_commit, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+worktreeColumns,
		w.ID, w.AttemptID, w.Path, w.Branch, w.BaseCommit, w.HeadCommit, string(w.Status))

	created, err := scanWorktree(row)
	if err != nil {
		return task.Worktree{}, fmt.Errorf("record worktree for attempt %s: %w", w.AttemptID, err)
	}
	return created, nil
}

// SetWorktreeHead records the commit a worktree ended on.
func (s *Store) SetWorktreeHead(ctx context.Context, id uuid.UUID, headCommit string) error {
	tag, err := s.db.Exec(ctx, `UPDATE worktrees SET head_commit = $2 WHERE id = $1`, id, headCommit)
	if err != nil {
		return fmt.Errorf("set worktree head %s: %w", id, classify(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set worktree head %s: %w", id, ErrNotFound)
	}
	return nil
}

// SetWorktreeStatus records a worktree as removed or retained.
//
// removed_at is set only for REMOVED, matching the database's consistency
// constraint: a retained worktree still exists on disk, so it has no removal
// time.
func (s *Store) SetWorktreeStatus(ctx context.Context, id uuid.UUID, status task.WorktreeStatus) error {
	var removedAt *time.Time
	if status == task.WorktreeRemoved {
		now := time.Now().UTC()
		removedAt = &now
	}
	tag, err := s.db.Exec(ctx,
		`UPDATE worktrees SET status = $2, removed_at = $3 WHERE id = $1`,
		id, string(status), removedAt)
	if err != nil {
		return fmt.Errorf("set worktree status %s: %w", id, classify(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set worktree status %s: %w", id, ErrNotFound)
	}
	return nil
}

// GetWorktreeByAttempt returns the worktree belonging to an attempt.
func (s *Store) GetWorktreeByAttempt(ctx context.Context, attemptID uuid.UUID) (task.Worktree, error) {
	row := s.db.QueryRow(ctx, `SELECT `+worktreeColumns+` FROM worktrees WHERE attempt_id = $1`, attemptID)
	w, err := scanWorktree(row)
	if err != nil {
		return task.Worktree{}, fmt.Errorf("get worktree for attempt %s: %w", attemptID, err)
	}
	return w, nil
}

// ListRetainedWorktrees returns worktrees deliberately kept after a failure, so
// an operator can find and review abandoned work.
func (s *Store) ListRetainedWorktrees(ctx context.Context) ([]task.Worktree, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+worktreeColumns+` FROM worktrees WHERE status = 'RETAINED' ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list retained worktrees: %w", classify(err))
	}
	defer rows.Close()

	var out []task.Worktree
	for rows.Next() {
		w, err := scanWorktree(rows)
		if err != nil {
			return nil, fmt.Errorf("list retained worktrees: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list retained worktrees: %w", classify(err))
	}
	return out, nil
}

func scanWorktree(row scanner) (task.Worktree, error) {
	var (
		w      task.Worktree
		status string
	)
	err := row.Scan(&w.ID, &w.AttemptID, &w.Path, &w.Branch, &w.BaseCommit,
		&w.HeadCommit, &status, &w.CreatedAt, &w.RemovedAt)
	if err != nil {
		return task.Worktree{}, classify(err)
	}
	w.Status = task.WorktreeStatus(status)
	return w, nil
}

const workerRunColumns = `id, attempt_id, backend, status, failure_kind, command, working_dir,
	exit_code, stdout, stdout_truncated, stderr, stderr_truncated, session_id, summary,
	finish_reason, tokens, cost, diff, diff_truncated, changed_files, started_at, finished_at`

// CreateWorkerRun records what an agent backend did.
func (s *Store) CreateWorkerRun(ctx context.Context, r task.WorkerRun) (task.WorkerRun, error) {
	var tokens any
	if len(r.Tokens) > 0 {
		tokens = []byte(r.Tokens)
	}
	row := s.db.QueryRow(ctx, `
		INSERT INTO worker_runs (
			id, attempt_id, backend, status, failure_kind, command, working_dir,
			exit_code, stdout, stdout_truncated, stderr, stderr_truncated,
			session_id, summary, finish_reason, tokens, cost, diff, diff_truncated,
			changed_files, started_at, finished_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		RETURNING `+workerRunColumns,
		r.ID, r.AttemptID, r.Backend, string(r.Status), string(r.FailureKind), r.Command,
		r.WorkingDir, r.ExitCode, r.Stdout, r.StdoutTruncated, r.Stderr, r.StderrTruncated,
		r.SessionID, r.Summary, r.FinishReason, tokens, r.Cost, r.Diff, r.DiffTruncated,
		r.ChangedFiles, r.StartedAt, r.FinishedAt)

	created, err := scanWorkerRun(row)
	if err != nil {
		return task.WorkerRun{}, fmt.Errorf("record worker run for attempt %s: %w", r.AttemptID, err)
	}
	return created, nil
}

// ListWorkerRuns returns an attempt's worker runs in order.
func (s *Store) ListWorkerRuns(ctx context.Context, attemptID uuid.UUID) ([]task.WorkerRun, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+workerRunColumns+` FROM worker_runs WHERE attempt_id = $1 ORDER BY started_at`,
		attemptID)
	if err != nil {
		return nil, fmt.Errorf("list worker runs for attempt %s: %w", attemptID, classify(err))
	}
	defer rows.Close()

	var out []task.WorkerRun
	for rows.Next() {
		r, err := scanWorkerRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list worker runs for attempt %s: %w", attemptID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list worker runs for attempt %s: %w", attemptID, classify(err))
	}
	return out, nil
}

func scanWorkerRun(row scanner) (task.WorkerRun, error) {
	var (
		r      task.WorkerRun
		status string
		kind   string
		tokens []byte
	)
	err := row.Scan(&r.ID, &r.AttemptID, &r.Backend, &status, &kind, &r.Command, &r.WorkingDir,
		&r.ExitCode, &r.Stdout, &r.StdoutTruncated, &r.Stderr, &r.StderrTruncated,
		&r.SessionID, &r.Summary, &r.FinishReason, &tokens, &r.Cost, &r.Diff,
		&r.DiffTruncated, &r.ChangedFiles, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return task.WorkerRun{}, classify(err)
	}
	r.Status = task.WorkerRunStatus(status)
	r.FailureKind = task.FailureKind(kind)
	r.Tokens = tokens
	return r, nil
}

const verificationRunColumns = `id, attempt_id, step_index, command, status, exit_code,
	stdout, stdout_truncated, stderr, stderr_truncated, duration_ms, started_at, finished_at`

// CreateVerificationRun records the result of one verification step that aidev
// ran itself.
func (s *Store) CreateVerificationRun(ctx context.Context, r task.VerificationRun) (task.VerificationRun, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO verification_runs (
			id, attempt_id, step_index, command, status, exit_code,
			stdout, stdout_truncated, stderr, stderr_truncated, duration_ms,
			started_at, finished_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+verificationRunColumns,
		r.ID, r.AttemptID, r.StepIndex, r.Command, string(r.Status), r.ExitCode,
		r.Stdout, r.StdoutTruncated, r.Stderr, r.StderrTruncated,
		r.Duration.Milliseconds(), r.StartedAt, r.FinishedAt)

	created, err := scanVerificationRun(row)
	if err != nil {
		return task.VerificationRun{}, fmt.Errorf("record verification step %d for attempt %s: %w",
			r.StepIndex, r.AttemptID, err)
	}
	return created, nil
}

// ListVerificationRuns returns an attempt's verification results in step order.
func (s *Store) ListVerificationRuns(ctx context.Context, attemptID uuid.UUID) ([]task.VerificationRun, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+verificationRunColumns+` FROM verification_runs WHERE attempt_id = $1 ORDER BY step_index`,
		attemptID)
	if err != nil {
		return nil, fmt.Errorf("list verification runs for attempt %s: %w", attemptID, classify(err))
	}
	defer rows.Close()

	var out []task.VerificationRun
	for rows.Next() {
		r, err := scanVerificationRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list verification runs for attempt %s: %w", attemptID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list verification runs for attempt %s: %w", attemptID, classify(err))
	}
	return out, nil
}

func scanVerificationRun(row scanner) (task.VerificationRun, error) {
	var (
		r          task.VerificationRun
		status     string
		durationMS int64
	)
	err := row.Scan(&r.ID, &r.AttemptID, &r.StepIndex, &r.Command, &status, &r.ExitCode,
		&r.Stdout, &r.StdoutTruncated, &r.Stderr, &r.StderrTruncated, &durationMS,
		&r.StartedAt, &r.FinishedAt)
	if err != nil {
		return task.VerificationRun{}, classify(err)
	}
	r.Status = task.VerificationStatus(status)
	r.Duration = time.Duration(durationMS) * time.Millisecond
	return r, nil
}
