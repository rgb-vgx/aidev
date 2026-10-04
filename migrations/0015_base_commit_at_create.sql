-- The commit the base ref pointed at when the task was created (research
-- D2). The ref itself is resolved again when the task runs, which can be
-- hours later; if someone pushed to it in between, the agent starts from
-- code the task's author never looked at. Recording the commit lets the run
-- notice and say so. '' for tasks created before this column existed: there
-- is nothing to compare against, so they are never reported as moved.
ALTER TABLE tasks
	ADD COLUMN base_commit_at_create TEXT NOT NULL DEFAULT '';

-- The warning a run records when the ref moved. The CHECK is dropped and
-- re-added under its own name because editing an applied migration would
-- change its checksum and be rejected by every database that ran it.
ALTER TABLE events DROP CONSTRAINT events_type_valid;
ALTER TABLE events ADD CONSTRAINT events_type_valid CHECK (type IN (
    'task.created', 'task.ready', 'task.started', 'task.succeeded',
    'task.failed', 'task.cancelled',
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
