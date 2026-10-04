-- Automatic retry (research F; design settled with the user on 2026-09-17).
-- A task with max_retries > 0 whose attempt fails in a way another try can
-- fix — the checks ran and failed, or the agent stopped early — goes back to
-- READY instead of FAILED, and the same run starts the next attempt in the
-- same worktree directory, continuing the agent's session (an OpenCode
-- session can only be continued in the directory it was created in,
-- docs/research.md §2.12). The task never passes through FAILED on the way,
-- so FAILED stays terminal.

-- The two new edges: RUNNING -> READY when the agent stopped early, and
-- VERIFYING -> READY when the checks failed. The guard is replaced whole
-- under the same name; the earlier migration is not edited, because that
-- would change its checksum and be rejected by every database that ran it.
CREATE OR REPLACE FUNCTION tasks_transition_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;

    IF (OLD.status = 'PENDING' AND NEW.status IN ('READY', 'WAITING_APPROVAL', 'CANCELLED'))
    OR (OLD.status = 'READY' AND NEW.status IN ('RUNNING', 'WAITING_APPROVAL', 'CANCELLED'))
    OR (OLD.status = 'WAITING_APPROVAL' AND NEW.status IN ('READY', 'FAILED', 'CANCELLED'))
    OR (OLD.status = 'RUNNING' AND NEW.status IN ('VERIFYING', 'READY', 'FAILED', 'CANCELLED'))
    OR (OLD.status = 'VERIFYING' AND NEW.status IN ('SUCCEEDED', 'READY', 'FAILED', 'CANCELLED'))
    THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'illegal task transition: % -> %', OLD.status, NEW.status
        USING HINT = 'the state machine lives in internal/task/status.go; change it there and add a migration';
END;
$$ LANGUAGE plpgsql;

-- REUSED: the attempt's worktree directory was handed to the next attempt,
-- which continues in it on its own branch. The row keeps this attempt's
-- branch and head commit; the directory now belongs to the later row, so a
-- REUSED row is not something on disk that needs removing.
ALTER TABLE worktrees DROP CONSTRAINT worktrees_status_valid;
ALTER TABLE worktrees ADD CONSTRAINT worktrees_status_valid CHECK (status IN (
    'ACTIVE', 'REMOVED', 'RETAINED', 'REUSED'));

ALTER TABLE events DROP CONSTRAINT events_type_valid;
ALTER TABLE events ADD CONSTRAINT events_type_valid CHECK (type IN (
    'task.created', 'task.ready', 'task.started', 'task.succeeded',
    'task.failed', 'task.cancelled', 'task.retry_scheduled',
    'task.worktree_created', 'task.worktree_removed', 'task.worktree_retained',
    'task.base_moved',
    'task.worker_started', 'task.worker_completed',
    'task.base_check_completed',
    'task.verification_started', 'task.verification_step_completed',
    'task.verification_completed', 'task.verification_intercepted',
    'task.verification_tests_modified',
    'task.verification_worktree_modified',
    'task.approval_required', 'task.approval_granted', 'task.approval_denied',
    'task.containment_breach', 'task.shared_refs_changed'));
