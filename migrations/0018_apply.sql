-- Applying a task's result (`aidev task apply`, `aidev task undo`): the
-- merge into the person's branch, and its revert, are recorded in the
-- task's history. Neither changes the task's status — SUCCEEDED says what
-- verification found, not whether anyone has taken the work — so they are
-- events, not states. The CHECK is dropped and re-added under its own name
-- because editing an applied migration would change its checksum and be
-- rejected by every database that ran it.
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
    'task.containment_breach', 'task.shared_refs_changed',
    'task.applied', 'task.apply_undone'));
