-- Widen events_type_valid with the interception event, without editing 0001_init.sql.
ALTER TABLE events DROP CONSTRAINT events_type_valid;
ALTER TABLE events ADD CONSTRAINT events_type_valid CHECK (type IN (
    'task.created', 'task.ready', 'task.started', 'task.succeeded',
    'task.failed', 'task.cancelled',
    'task.worktree_created', 'task.worktree_removed', 'task.worktree_retained',
    'task.worker_started', 'task.worker_completed',
    'task.verification_started', 'task.verification_step_completed',
    'task.verification_completed', 'task.verification_intercepted',
    'task.approval_required', 'task.approval_granted', 'task.approval_denied'));
