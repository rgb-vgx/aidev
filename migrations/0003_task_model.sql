-- Remember which model each task asked for and which model and agent actually
-- ran it, without editing 0001_init.sql. The new columns follow the style of
-- the existing TEXT columns (see CONSTRAINT tasks_agent_not_blank): NOT NULL
-- with an empty-string default, and no CHECK constraint, because model and
-- agent names are backend-defined identifiers rather than a closed vocabulary.
ALTER TABLE tasks ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE worker_runs ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE worker_runs ADD COLUMN agent TEXT NOT NULL DEFAULT '';
