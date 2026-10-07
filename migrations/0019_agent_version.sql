-- The version of the agent that ran each worker run, as the agent's own
-- `--version` prints it. An agent upgraded between two runs of one task is
-- otherwise invisible, and it is the first thing to rule out when the same
-- task passes one day and fails the next. NOT NULL with an empty default,
-- like model and agent in 0003: empty means the version could not be read,
-- and rows from before this migration read the same way.
ALTER TABLE worker_runs ADD COLUMN agent_version TEXT NOT NULL DEFAULT '';
