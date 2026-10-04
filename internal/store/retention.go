package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aidev/internal/task"
)

// Retention (research C6). Captured output is the bulk of the database —
// up to 1 MiB of stdout, 1 MiB of stderr and 4 MiB of diff per attempt — and
// nothing cleared it. These are the two ways to reclaim it: blank old output
// and keep the record, or delete a finished task outright.

// terminalStatuses is every status a task cannot leave, as SQL parameters.
// Built from the Go enum so a new terminal status cannot be forgotten here.
func terminalStatuses() []string {
	var out []string
	for _, s := range task.AllStatuses() {
		if s.Terminal() {
			out = append(out, string(s))
		}
	}
	return out
}

// PruneReport says what PruneLogs cleared, or would clear on a dry run.
type PruneReport struct {
	WorkerRuns       int   `json:"worker_runs"`
	VerificationRuns int   `json:"verification_runs"`
	Bytes            int64 `json:"bytes"`
	DryRun           bool  `json:"dry_run"`
}

// pruneWhere selects the rows PruneLogs may clear: runs that finished before
// the cutoff, of tasks that have finished, that still hold some output. A
// run of a task that is still going is never touched — its output may be
// what someone is about to read — and a row with nothing left to clear is
// not flagged, so logs_pruned always means something was removed.
const pruneWhere = `
	r.finished_at IS NOT NULL AND r.finished_at < $1
	AND NOT r.logs_pruned
	AND a.id = r.attempt_id AND t.id = a.task_id
	AND t.status = ANY($2)`

// PruneLogs blanks the captured stdout, stderr and diff of runs that
// finished before cutoff, on tasks that have finished, and marks them
// logs_pruned. Everything else about the run stays: status, exit code,
// timings, the agent's summary, and every event. With dryRun nothing is
// written and the report says what would have been cleared.
func (s *Store) PruneLogs(ctx context.Context, cutoff time.Time, dryRun bool) (PruneReport, error) {
	report := PruneReport{DryRun: dryRun}
	terminal := terminalStatuses()

	err := s.InTx(ctx, func(tx *Store) error {
		var workerBytes, verifyBytes int64
		if err := tx.db.QueryRow(ctx, `
			SELECT count(*), COALESCE(sum(octet_length(r.stdout) + octet_length(r.stderr) + octet_length(r.diff)), 0)
			FROM worker_runs r, task_attempts a, tasks t
			WHERE `+pruneWhere+` AND (r.stdout <> '' OR r.stderr <> '' OR r.diff <> '')`,
			cutoff, terminal).Scan(&report.WorkerRuns, &workerBytes); err != nil {
			return fmt.Errorf("measure prunable agent output: %w", classify(err))
		}
		if err := tx.db.QueryRow(ctx, `
			SELECT count(*), COALESCE(sum(octet_length(r.stdout) + octet_length(r.stderr)), 0)
			FROM verification_runs r, task_attempts a, tasks t
			WHERE `+pruneWhere+` AND (r.stdout <> '' OR r.stderr <> '')`,
			cutoff, terminal).Scan(&report.VerificationRuns, &verifyBytes); err != nil {
			return fmt.Errorf("measure prunable verification output: %w", classify(err))
		}
		report.Bytes = workerBytes + verifyBytes
		if dryRun {
			return nil
		}

		if _, err := tx.db.Exec(ctx, `
			UPDATE worker_runs r SET stdout = '', stderr = '', diff = '', logs_pruned = true
			FROM task_attempts a, tasks t
			WHERE `+pruneWhere+` AND (r.stdout <> '' OR r.stderr <> '' OR r.diff <> '')`,
			cutoff, terminal); err != nil {
			return fmt.Errorf("prune agent output: %w", classify(err))
		}
		if _, err := tx.db.Exec(ctx, `
			UPDATE verification_runs r SET stdout = '', stderr = '', logs_pruned = true
			FROM task_attempts a, tasks t
			WHERE `+pruneWhere+` AND (r.stdout <> '' OR r.stderr <> '')`,
			cutoff, terminal); err != nil {
			return fmt.Errorf("prune verification output: %w", classify(err))
		}
		return nil
	})
	if err != nil {
		return PruneReport{}, err
	}
	return report, nil
}

var (
	// ErrTaskNotFinished means a task cannot be deleted because it can still
	// change: deleting it would pull the record out from under a run.
	ErrTaskNotFinished = errors.New("task has not finished")

	// ErrWorktreeOnDisk means a task cannot be deleted because one of its
	// worktrees is still on disk. Deleting the record would leave that
	// directory with nothing pointing at it.
	ErrWorktreeOnDisk = errors.New("a worktree of the task is still on disk")
)

// DeleteTask removes a finished task together with its attempts, runs,
// worktree records, approvals and events — the ON DELETE CASCADE the schema
// was written for (migration 0001: deleting a task with its history is a
// retention operation; changing history is what the events trigger
// forbids). It refuses a task that has not finished, and one whose worktree
// is still on disk. The task's branch in the repository is not aidev's to
// delete and is left alone.
//
// The checks and the delete run in one transaction with the task row
// locked, so a run cannot start between the check and the delete.
func (s *Store) DeleteTask(ctx context.Context, id uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		var status string
		err := tx.db.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1 FOR UPDATE`, id).Scan(&status)
		if err != nil {
			return fmt.Errorf("delete task %s: %w", id, classify(err))
		}
		if !task.Status(status).Terminal() {
			return fmt.Errorf("delete task %s: it is %s: %w", id, status, ErrTaskNotFinished)
		}

		var path string
		err = tx.db.QueryRow(ctx, `
			SELECT w.path FROM worktrees w JOIN task_attempts a ON a.id = w.attempt_id
			WHERE a.task_id = $1 AND w.status <> $2
			ORDER BY w.created_at DESC LIMIT 1`, id, string(task.WorktreeRemoved)).Scan(&path)
		switch {
		case err == nil:
			return fmt.Errorf("delete task %s: %s: %w", id, path, ErrWorktreeOnDisk)
		case !errors.Is(classify(err), ErrNotFound):
			return fmt.Errorf("delete task %s: %w", id, classify(err))
		}

		tag, err := tx.db.Exec(ctx, `DELETE FROM tasks WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("delete task %s: %w", id, classify(err))
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("delete task %s: %w", id, ErrNotFound)
		}
		return nil
	})
}
