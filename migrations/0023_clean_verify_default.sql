-- New projects verify in a clean checkout: a file git ignores (a .venv the
-- agent built, a vendored dependency, a cached build output) cannot make the
-- checks pass there, while in the agent's worktree it can, silently. Only the
-- column default changes — every existing row keeps the mode it already holds,
-- and EnsureProject is insert-or-select, so re-registering a project never
-- rewrites its setting.
ALTER TABLE projects ALTER COLUMN verification_mode SET DEFAULT 'clean';
