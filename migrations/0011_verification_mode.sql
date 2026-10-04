-- Where a task's verification commands run (research B2), and what runs
-- before them.
--
-- verification_mode: 'in_place' checks the worktree the agent worked in, as
-- everything did before this column existed; 'clean' re-checks out the
-- post-agent tree into a temporary worktree and checks there, so files git
-- ignores cannot make the checks pass and files the agent never added cannot
-- pass unnoticed. Both columns exist because the mode must survive a project
-- default changing later: a task's verification stays a pure function of the
-- task, frozen at creation.
--
-- setup_steps: commands that prepare the checkout (npm ci and friends) before
-- the verification commands. JSONB rather than TEXT[] for the same reason as
-- protected_paths — argv objects with per-step options need a document.
--
-- phase on verification_runs: setup and verify steps share one global
-- step_index (the UNIQUE constraint keys on it), so the step_index alone
-- cannot say which half a row belongs to. A column, not an overloaded index —
-- readers that page on step_index keep working unchanged.
ALTER TABLE projects
    ADD COLUMN verification_mode TEXT NOT NULL DEFAULT 'in_place'
    CONSTRAINT projects_verification_mode_valid CHECK (verification_mode IN ('in_place', 'clean'));

ALTER TABLE tasks
    ADD COLUMN verification_mode TEXT NOT NULL DEFAULT 'in_place'
    CONSTRAINT tasks_verification_mode_valid CHECK (verification_mode IN ('in_place', 'clean'));

ALTER TABLE tasks
    ADD COLUMN setup_steps JSONB NOT NULL DEFAULT '[]'::jsonb
    CONSTRAINT tasks_setup_steps_is_array CHECK (jsonb_typeof(setup_steps) = 'array');

ALTER TABLE verification_runs
    ADD COLUMN phase TEXT NOT NULL DEFAULT 'verify'
    CONSTRAINT verification_runs_phase_valid CHECK (phase IN ('setup', 'verify'));
