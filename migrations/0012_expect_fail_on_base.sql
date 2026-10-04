-- Red-before-green checking (docs/research.md B3), for tasks whose point is
-- to fix a bug: with expect_fail_on_base the run first executes the
-- verification commands on the base commit, before the agent touches
-- anything. Commands that already pass there cannot tell the before state
-- from the after state — such a task would report success while nothing
-- changed — so the attempt fails immediately with kind VERIFICATION instead
-- of calling the agent. BOOLEAN rather than a CHECK list: there is one flag
-- and its false value is the default every existing row already means.
--
-- The event vocabulary gains task.base_check_completed (the outcome of that
-- pre-agent pass) by dropping and re-adding the CHECK under its own name,
-- because editing an applied migration would change its checksum and be
-- rejected by every database that ran it.

ALTER TABLE tasks
    ADD COLUMN expect_fail_on_base BOOLEAN NOT NULL DEFAULT false;

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
    'task.approval_required', 'task.approval_granted', 'task.approval_denied',
    'task.containment_breach', 'task.shared_refs_changed'));
