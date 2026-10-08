-- Directories outside the repository that the agent of this project's tasks
-- may read (`aidev project read-dirs`): an installed program's files, say. Set
-- only from the CLI, like requires_approval and verification_mode, because the
-- party creating tasks must not widen what its own agent can reach. Empty, the
-- default, means the agent reaches nothing outside its worktree.
ALTER TABLE projects ADD COLUMN read_dirs TEXT[] NOT NULL DEFAULT '{}';
