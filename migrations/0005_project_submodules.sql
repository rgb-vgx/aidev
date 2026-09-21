-- Record, per project, whether a task worktree should be given the content of
-- the repository's git submodules. Without it, a worktree of a repository that
-- keeps its sources in submodules has empty submodule directories, so any
-- verification command that reads those sources cannot run — and a task aidev
-- cannot verify is a task aidev will not accept.
--
-- The column follows the style of the other TEXT enumerations: NOT NULL, with a
-- CHECK that mirrors the Go vocabulary exactly (see
-- internal/store/schema_parity_test.go). Unlike tasks.hardness there is no
-- blank member: every project has a mode, and the default is the one that
-- changes nothing for a repository without submodules.
ALTER TABLE projects ADD COLUMN submodules TEXT NOT NULL DEFAULT 'NONE';
ALTER TABLE projects ADD CONSTRAINT projects_submodules_valid CHECK (submodules IN (
    'NONE', 'READ_ONLY'));
