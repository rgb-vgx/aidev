-- `aidev task create --coverage`: once verification passes, aidev measures
-- which of the Go lines the task changed its tests ran, and records the
-- finding as task.coverage_reported. A report only; the outcome never depends
-- on it. The column is frozen into the task like expect_fail_on_base; the
-- event CHECK is dropped and re-added under its own name, as in 0018 and 0020.
ALTER TABLE tasks ADD COLUMN coverage BOOLEAN NOT NULL DEFAULT false;
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
    'task.applied', 'task.apply_undone',
    'task.run_stopped', 'task.coverage_reported'));
