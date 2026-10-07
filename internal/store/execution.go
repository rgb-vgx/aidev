package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aidev/internal/task"
)

const attemptColumns = `id, task_id, attempt_number, status, failure_kind, error, started_at, finished_at,
	lease_owner, lease_expires_at`

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
		INSERT INTO task_attempts (id, task_id, attempt_number, status, failure_kind, error, started_at,
			lease_owner, lease_expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING `+attemptColumns,
		a.ID, a.TaskID, a.AttemptNumber, string(a.Status), string(a.FailureKind), a.Error, a.StartedAt,
		a.LeaseOwner, a.LeaseExpiresAt)

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
	err := row.Scan(&a.ID, &a.TaskID, &a.AttemptNumber, &status, &kind, &a.Error, &a.StartedAt, &a.FinishedAt,
		&a.LeaseOwner, &a.LeaseExpiresAt)
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

// ErrLeaseLost means the attempt a run is writing for is no longer RUNNING
// under that run's process: someone else — a Cancel, `aidev task recover` —
// ended it. The run must stop publishing (see HoldLease).
var ErrLeaseLost = errors.New("the attempt is no longer held by this process")

// HoldLease is the fence every write of a run passes through while its
// attempt is open (production review, 2026-10-04). Inside the caller's
// transaction it locks the task row, then the attempt row, and requires the
// attempt to be RUNNING and leased to owner; otherwise it returns
// ErrLeaseLost and the caller's transaction writes nothing.
//
// A lease alone only lets aidev notice a dead run. A run that was paused, or
// that never heard of a Cancel, could otherwise go on writing — events after
// the ending, a commit on the branch — once someone else had finished its
// attempt. Holding both row locks until the caller commits also makes the
// fence and the write one step: a Cancel waits on the same rows, in the same
// order (task first, then attempts), so it cannot slip in between.
//
// What it guards is state other parties own: the task's status, its events,
// and — because the callers hold it across their git work — the branch and
// the worktree. Rows that only record what an attempt itself did (its worker
// run, its verification results) are written without it: they describe work
// that really happened, and an attempt ended elsewhere is when they matter
// most.
//
// It must run inside a transaction — a row lock taken on the pool is released
// as soon as the statement ends — and refuses otherwise.
func (s *Store) HoldLease(ctx context.Context, taskID, attemptID uuid.UUID, owner string) error {
	if s.pool != nil {
		return fmt.Errorf("hold lease on attempt %s: HoldLease must run inside a transaction", attemptID)
	}
	var one int
	if err := s.db.QueryRow(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&one); err != nil {
		return fmt.Errorf("hold lease on attempt %s: lock task: %w", attemptID, classify(err))
	}
	err := s.db.QueryRow(ctx, `
		SELECT 1 FROM task_attempts
		WHERE id = $1 AND task_id = $2 AND status = 'RUNNING' AND lease_owner = $3
		FOR UPDATE`, attemptID, taskID, owner).Scan(&one)
	switch {
	case err == nil:
		return nil
	case errors.Is(classify(err), ErrNotFound):
		return fmt.Errorf("hold lease on attempt %s: %w", attemptID, ErrLeaseLost)
	default:
		return fmt.Errorf("hold lease on attempt %s: %w", attemptID, classify(err))
	}
}

// RenewLease extends an attempt's lease and returns the task's current status.
//
// One round trip does both, and that pairing is the point: the run's claim
// stays alive only while the run also checks for a cancel, so a watcher that
// renews is a watcher that polls and neither half can be dropped while the
// other is observed. The data-modifying CTE runs exactly once even though the
// main query does not read it, and the status comes from the task row whether
// or not anything was renewed — a finished or foreign attempt reports its
// task's status all the same, which is how the watcher learns the run ended
// even after it stopped holding the lease itself.
func (s *Store) RenewLease(ctx context.Context, attemptID uuid.UUID, owner string, ttl time.Duration) (task.Status, error) {
	var status string
	err := s.db.QueryRow(ctx, `
		WITH renewed AS (
			UPDATE task_attempts
			SET lease_expires_at = now() + make_interval(secs => $3::double precision)
			WHERE id = $1 AND lease_owner = $2 AND status = 'RUNNING'
			RETURNING id
		)
		SELECT t.status
		FROM tasks t
		JOIN task_attempts a ON a.task_id = t.id
		WHERE a.id = $1`,
		attemptID, owner, ttl.Seconds()).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("renew lease on attempt %s: %w", attemptID, classify(err))
	}
	parsed, err := task.ParseStatus(status)
	if err != nil {
		return "", fmt.Errorf("renew lease on attempt %s: task has unrecognised status: %w", attemptID, err)
	}
	return parsed, nil
}

// StuckAttempt is a task still RUNNING or VERIFYING whose latest attempt has
// no live lease: the process that owned it is gone, or never claimed it.
// LeaseExpiresAt is nil when the lease was never set and LeaseOwner empty when
// nobody ever reported in.
type StuckAttempt struct {
	TaskID         uuid.UUID
	TaskRef        string
	Status         task.Status
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	StartedAt      time.Time
}

// StuckLeases lists tasks whose latest attempt is no longer being heartbeated.
//
// An attempt whose lease is still in the future belongs to a process checking
// in right now and is not reported. A NULL lease counts as expired: nobody is
// renewing it, so nobody is vouching for it either. A task with no attempts at
// all — which a crash between the status change and the attempt insert could
// leave — has nothing to heartbeat and is reported for the same reason.
func (s *Store) StuckLeases(ctx context.Context) ([]StuckAttempt, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.id, t.ref, t.status, a.lease_owner, a.lease_expires_at, a.started_at
		FROM tasks t
		LEFT JOIN LATERAL (
			SELECT lease_owner, lease_expires_at, started_at
			FROM task_attempts
			WHERE task_id = t.id
			ORDER BY attempt_number DESC
			LIMIT 1
		) a ON true
		WHERE t.status IN ('RUNNING', 'VERIFYING')
		  AND (a.lease_expires_at IS NULL OR a.lease_expires_at < now())
		ORDER BY t.ref`)
	if err != nil {
		return nil, fmt.Errorf("list tasks with an expired lease: %w", classify(err))
	}
	defer rows.Close()

	var out []StuckAttempt
	for rows.Next() {
		var (
			item   StuckAttempt
			status string
		)
		if err := rows.Scan(&item.TaskID, &item.TaskRef, &status,
			&item.LeaseOwner, &item.LeaseExpiresAt, &item.StartedAt); err != nil {
			return nil, fmt.Errorf("list tasks with an expired lease: %w", classify(err))
		}
		parsed, err := task.ParseStatus(status)
		if err != nil {
			return nil, fmt.Errorf("task %s has unrecognised status: %w", item.TaskRef, err)
		}
		item.Status = parsed
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks with an expired lease: %w", classify(err))
	}
	return out, nil
}

const worktreeColumns = `id, attempt_id, path, branch, base_commit, head_commit, status, created_at, removed_at, agent_tree`

// CreateWorktree records an isolated workspace.
func (s *Store) CreateWorktree(ctx context.Context, w task.Worktree) (task.Worktree, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO worktrees (id, attempt_id, path, branch, base_commit, head_commit, status, agent_tree)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING `+worktreeColumns,
		w.ID, w.AttemptID, w.Path, w.Branch, w.BaseCommit, w.HeadCommit, string(w.Status), w.AgentTree)

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

// SetWorktreeAgentTree records the tree the agent's work was snapshotted to
// (research A6): the tree the success commit carries, taken before
// verification ran. The in-memory copy is what the commit uses; this row is
// so a later reader can see what was committed without trusting the branch.
func (s *Store) SetWorktreeAgentTree(ctx context.Context, id uuid.UUID, tree string) error {
	tag, err := s.db.Exec(ctx, `UPDATE worktrees SET agent_tree = $2 WHERE id = $1`, id, tree)
	if err != nil {
		return fmt.Errorf("set worktree agent tree %s: %w", id, classify(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set worktree agent tree %s: %w", id, ErrNotFound)
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

// WorktreeWithTask is a worktree together with the task it belongs to, which is
// what an operator needs to make sense of a directory on disk.
type WorktreeWithTask struct {
	Worktree      task.Worktree
	TaskID        uuid.UUID
	TaskRef       string
	TaskTitle     string
	TaskStatus    task.Status
	AttemptNumber int
}

// ListWorktrees returns worktrees with their task, newest first, optionally
// restricted to certain statuses.
//
// It exists because a worktree on disk is meaningless on its own: an operator
// looking at workspace_root needs to know which task left it there and how that
// task ended.
func (s *Store) ListWorktrees(ctx context.Context, statuses []task.WorktreeStatus) ([]WorktreeWithTask, error) {
	wanted := make([]string, 0, len(statuses))
	for _, st := range statuses {
		wanted = append(wanted, string(st))
	}

	rows, err := s.db.Query(ctx, `
		SELECT w.id, w.attempt_id, w.path, w.branch, w.base_commit, w.head_commit,
		       w.status, w.created_at, w.removed_at, w.agent_tree,
		       t.id, t.ref, t.title, t.status, a.attempt_number
		FROM worktrees w
		JOIN task_attempts a ON a.id = w.attempt_id
		JOIN tasks t ON t.id = a.task_id
		WHERE cardinality($1::text[]) = 0 OR w.status = ANY($1)
		ORDER BY w.created_at DESC`, wanted)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", classify(err))
	}
	defer rows.Close()

	var out []WorktreeWithTask
	for rows.Next() {
		var (
			item       WorktreeWithTask
			wtStatus   string
			taskStatus string
		)
		err := rows.Scan(
			&item.Worktree.ID, &item.Worktree.AttemptID, &item.Worktree.Path,
			&item.Worktree.Branch, &item.Worktree.BaseCommit, &item.Worktree.HeadCommit,
			&wtStatus, &item.Worktree.CreatedAt, &item.Worktree.RemovedAt,
			&item.Worktree.AgentTree,
			&item.TaskID, &item.TaskRef, &item.TaskTitle, &taskStatus, &item.AttemptNumber)
		if err != nil {
			return nil, fmt.Errorf("list worktrees: %w", classify(err))
		}
		item.Worktree.Status = task.WorktreeStatus(wtStatus)
		item.TaskStatus = task.Status(taskStatus)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list worktrees: %w", classify(err))
	}
	return out, nil
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
		&w.HeadCommit, &status, &w.CreatedAt, &w.RemovedAt, &w.AgentTree)
	if err != nil {
		return task.Worktree{}, classify(err)
	}
	w.Status = task.WorktreeStatus(status)
	return w, nil
}

const workerRunColumns = `id, attempt_id, backend, model, agent, status, failure_kind, command, working_dir,
	exit_code, stdout, stdout_truncated, stderr, stderr_truncated, session_id, summary,
	finish_reason, tokens, cost, diff, diff_truncated, changed_files, started_at, finished_at, logs_pruned,
	agent_version`

// CreateWorkerRun records what an agent backend did.
func (s *Store) CreateWorkerRun(ctx context.Context, r task.WorkerRun) (task.WorkerRun, error) {
	var tokens any
	if len(r.Tokens) > 0 {
		tokens = []byte(r.Tokens)
	}
	row := s.db.QueryRow(ctx, `
		INSERT INTO worker_runs (
			id, attempt_id, backend, model, agent, status, failure_kind, command, working_dir,
			exit_code, stdout, stdout_truncated, stderr, stderr_truncated,
			session_id, summary, finish_reason, tokens, cost, diff, diff_truncated,
			changed_files, started_at, finished_at, agent_version
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		RETURNING `+workerRunColumns,
		r.ID, r.AttemptID, r.Backend, r.Model, r.Agent, string(r.Status), string(r.FailureKind), r.Command,
		r.WorkingDir, r.ExitCode, r.Stdout, r.StdoutTruncated, r.Stderr, r.StderrTruncated,
		r.SessionID, r.Summary, r.FinishReason, tokens, r.Cost, r.Diff, r.DiffTruncated,
		r.ChangedFiles, r.StartedAt, r.FinishedAt, r.AgentVersion)

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
	err := row.Scan(&r.ID, &r.AttemptID, &r.Backend, &r.Model, &r.Agent, &status, &kind, &r.Command, &r.WorkingDir,
		&r.ExitCode, &r.Stdout, &r.StdoutTruncated, &r.Stderr, &r.StderrTruncated,
		&r.SessionID, &r.Summary, &r.FinishReason, &tokens, &r.Cost, &r.Diff,
		&r.DiffTruncated, &r.ChangedFiles, &r.StartedAt, &r.FinishedAt, &r.LogsPruned,
		&r.AgentVersion)
	if err != nil {
		return task.WorkerRun{}, classify(err)
	}
	r.Status = task.WorkerRunStatus(status)
	r.FailureKind = task.FailureKind(kind)
	r.Tokens = tokens
	return r, nil
}

const verificationRunColumns = `id, attempt_id, step_index, phase, command, status, exit_code,
	stdout, stdout_truncated, stderr, stderr_truncated, duration_ms, started_at, finished_at, logs_pruned`

// CreateVerificationRun records the result of one verification step that aidev
// ran itself.
func (s *Store) CreateVerificationRun(ctx context.Context, r task.VerificationRun) (task.VerificationRun, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO verification_runs (
			id, attempt_id, step_index, phase, command, status, exit_code,
			stdout, stdout_truncated, stderr, stderr_truncated, duration_ms,
			started_at, finished_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING `+verificationRunColumns,
		r.ID, r.AttemptID, r.StepIndex, string(r.Phase), r.Command, string(r.Status), r.ExitCode,
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
		phase      string
		status     string
		durationMS int64
	)
	err := row.Scan(&r.ID, &r.AttemptID, &r.StepIndex, &phase, &r.Command, &status, &r.ExitCode,
		&r.Stdout, &r.StdoutTruncated, &r.Stderr, &r.StderrTruncated, &durationMS,
		&r.StartedAt, &r.FinishedAt, &r.LogsPruned)
	if err != nil {
		return task.VerificationRun{}, classify(err)
	}
	// The column defaults to 'verify', which is also what rows written
	// before the column existed mean; anything else not in the vocabulary
	// could only come from a diverged CHECK constraint.
	if !task.VerificationPhase(phase).Valid() {
		return task.VerificationRun{}, fmt.Errorf("verification run %s has unrecognised phase %q", r.ID, phase)
	}
	r.Phase = task.VerificationPhase(phase)
	r.Status = task.VerificationStatus(status)
	r.Duration = time.Duration(durationMS) * time.Millisecond
	return r, nil
}

// OverlappingBranches returns which of branches belong to another attempt
// whose run overlapped this one: not finished yet, or finished after since
// (this attempt's start). Tasks of one repository running side by side
// create and move such branches in the shared repository while each other's
// agents work; containment uses this to tell that from an agent touching
// refs it should not.
func (s *Store) OverlappingBranches(ctx context.Context, ownAttempt uuid.UUID, since time.Time, branches []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(branches) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT w.branch
		FROM worktrees w JOIN task_attempts a ON a.id = w.attempt_id
		WHERE w.branch = ANY($1) AND a.id <> $2
		  AND (a.finished_at IS NULL OR a.finished_at >= $3)`, branches, ownAttempt, since)
	if err != nil {
		return nil, fmt.Errorf("find overlapping branches: %w", classify(err))
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("find overlapping branches: %w", classify(err))
		}
		out[b] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find overlapping branches: %w", classify(err))
	}
	return out, nil
}
