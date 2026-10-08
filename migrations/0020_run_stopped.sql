-- A cancel waits for the process running the task to stop, and records
-- whether it did (task.run_stopped). A cancelled task whose runner — or the
-- applications its agent's commands started — kept running was otherwise
-- indistinguishable from one that stopped (TASK-000092). The CHECK is dropped
-- and re-added under its own name, as in 0018, because editing an applied
-- migration would change its checksum.
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
    'task.run_stopped'));
