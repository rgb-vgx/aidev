-- The agent's delivery, as a tree object (research A6). Verification runs
-- after the agent finishes, so whatever the checks write — coverage output,
-- build artefacts, a regenerated snapshot — must not reach the branch: the
-- success commit is built from this tree instead of from the working
-- directory as verification left it. '' means no snapshot has been taken
-- yet: a row written before this migration, or an attempt that failed
-- before the snapshot ran.
ALTER TABLE worktrees
	ADD COLUMN agent_tree TEXT NOT NULL DEFAULT '';

-- Verification editing a tracked file after that snapshot is a warning, not
-- a failure: the commit still carries the agent's tree, so what the checks
-- ran against is not what the branch receives. The event is the reviewer's
-- signal that the two differ (research A6). The CHECK is dropped and
-- re-added under its own name because editing an applied migration would
-- change its checksum and be rejected by every database that ran it.
ALTER TABLE events DROP CONSTRAINT events_type_valid;
ALTER TABLE events ADD CONSTRAINT events_type_valid CHECK (type IN (
    'task.created', 'task.ready', 'task.started', 'task.succeeded',
    'task.failed', 'task.cancelled',
    'task.worktree_created', 'task.worktree_removed', 'task.worktree_retained',
    'task.worker_started', 'task.worker_completed',
    'task.base_check_completed',
    'task.verification_started', 'task.verification_step_completed',
    'task.verification_completed', 'task.verification_intercepted',
    'task.verification_tests_modified',
    'task.verification_worktree_modified',
    'task.approval_required', 'task.approval_granted', 'task.approval_denied',
    'task.containment_breach', 'task.shared_refs_changed'));
