-- Shared-state containment (docs/research.md §7i): an attempt can now fail as
-- CONTAINMENT when the agent edits the config, hooks, attributes or HEAD that
-- the worktree shares with the main repository, and the check emits two new
-- event types. The vocabularies are widened here, by dropping and re-adding
-- each constraint under its own name, because editing an applied migration
-- would change its checksum and be rejected by every database that ran it.

ALTER TABLE task_attempts DROP CONSTRAINT task_attempts_failure_valid;
ALTER TABLE task_attempts ADD CONSTRAINT task_attempts_failure_valid CHECK (failure_kind IN (
    '', 'STARTUP', 'AGENT_ERROR', 'AGENT_EXIT', 'TIMEOUT', 'CANCELLED',
    'VERIFICATION', 'WORKTREE', 'APPROVAL_DENIED', 'CONTAINMENT',
    'INTERNAL', 'UNKNOWN'));

ALTER TABLE worker_runs DROP CONSTRAINT worker_runs_failure_valid;
ALTER TABLE worker_runs ADD CONSTRAINT worker_runs_failure_valid CHECK (failure_kind IN (
    '', 'STARTUP', 'AGENT_ERROR', 'AGENT_EXIT', 'TIMEOUT', 'CANCELLED',
    'VERIFICATION', 'WORKTREE', 'APPROVAL_DENIED', 'CONTAINMENT',
    'INTERNAL', 'UNKNOWN'));

ALTER TABLE events DROP CONSTRAINT events_type_valid;
ALTER TABLE events ADD CONSTRAINT events_type_valid CHECK (type IN (
    'task.created', 'task.ready', 'task.started', 'task.succeeded',
    'task.failed', 'task.cancelled',
    'task.worktree_created', 'task.worktree_removed', 'task.worktree_retained',
    'task.worker_started', 'task.worker_completed',
    'task.verification_started', 'task.verification_step_completed',
    'task.verification_completed', 'task.verification_intercepted',
    'task.approval_required', 'task.approval_granted', 'task.approval_denied',
    'task.containment_breach', 'task.shared_refs_changed'));
