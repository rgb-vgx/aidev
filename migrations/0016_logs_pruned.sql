-- Retention for captured output (research C6). Every attempt can keep up to
-- 1 MiB of stdout, 1 MiB of stderr and 4 MiB of diff, and until now nothing
-- ever cleared them. `aidev prune --logs-older-than` blanks those columns on
-- runs of finished tasks and sets logs_pruned, so an empty column reads as
-- "removed for retention" rather than "the command printed nothing".
--
-- Only these output columns are touched. The events table is not: it is
-- append-only (invariant 4), and the audit trail of what happened stays whole
-- even when the bulky output it summarised is gone.
ALTER TABLE worker_runs
	ADD COLUMN logs_pruned BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE verification_runs
	ADD COLUMN logs_pruned BOOLEAN NOT NULL DEFAULT false;
