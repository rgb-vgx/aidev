-- A project can turn the coverage report on for all of its tasks
-- (`aidev project coverage on`): once verification passes, aidev measures
-- which of the Go lines a task changed its tests ran, and records the
-- finding as task.coverage_reported. A report only; the outcome never depends
-- on it. The column is a template like verification_mode — each task freezes
-- its own copy at creation, so changing it here never re-judges a task that
-- already exists — and it defaults off, so existing projects behave as before.
ALTER TABLE projects ADD COLUMN coverage BOOLEAN NOT NULL DEFAULT false;
