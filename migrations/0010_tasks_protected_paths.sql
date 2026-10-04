-- Human-declared "do not touch" globs, one per task (research §7b tier 2). If
-- the attempt changes a path matching any of them, verification refuses to run
-- the checks at all and the task fails with VERIFICATION — the same refusal
-- path as interception, reusing task.verification_intercepted so a reviewer
-- reads one event for both reasons not to trust a run.
--
-- This is the enforcement half of the test-editing report: the classifier in
-- verification.TestPaths only reports because it is heuristic, while these
-- patterns are declared up front by whoever creates the task, so acting on
-- them cannot punish an honest fixture edit that nobody singled out.
--
-- An empty array is the default: most tasks protect nothing, and the check is
-- skipped outright then. JSONB rather than TEXT[] so future pattern options
-- (per-pattern severity, say) can be carried without another migration.
ALTER TABLE tasks
    ADD COLUMN protected_paths JSONB NOT NULL DEFAULT '[]'::jsonb
    CONSTRAINT tasks_protected_paths_is_array CHECK (jsonb_typeof(protected_paths) = 'array');
