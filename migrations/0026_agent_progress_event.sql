-- task.agent_progress records what a running agent has done so far: counts,
-- the latest tool and the path it aimed at (TASK-000143 had 20 minutes with
-- nothing between worker_started and worker_completed). A report only; the
-- outcome never depends on it. The event CHECK is dropped and re-added under
-- its own name, as in 0020, 0022 and 0025.
ALTER TABLE events DROP CONSTRAINT events_type_valid;
ALTER TABLE events ADD CONSTRAINT events_type_valid CHECK (type IN (
    'task.created', 'task.ready', 'task.started', 'task.succeeded',
    'task.failed', 'task.cancelled', 'task.retry_scheduled',
    'task.worktree_created', 'task.worktree_removed', 'task.worktree_retained',
    'task.base_moved',
    'task.worker_started', 'task.worker_completed',
    'task.agent_progress',
    'task.base_check_completed',
    'task.verification_started', 'task.verification_step_completed',
    'task.verification_completed', 'task.verification_intercepted',
    'task.verification_tests_modified',
    'task.verification_worktree_modified',
    'task.approval_required', 'task.approval_granted', 'task.approval_denied',
    'task.containment_breach', 'task.shared_refs_changed',
    'task.applied', 'task.apply_undone',
    'task.run_stopped', 'task.coverage_reported', 'task.files_added'));
