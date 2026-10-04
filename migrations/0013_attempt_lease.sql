-- A lease per attempt, so a run that is alive proves it and one that died can
-- be found (research C1). lease_owner names the process heartbeating the
-- attempt (hostname:pid:uuid); lease_expires_at is when that claim runs out
-- unless renewed. The run's cancel poll renews both every round trip, so a
-- live run keeps its lease and a dead one stops within seconds.
--
-- A NULL lease_expires_at means nobody ever reported in: the row predates this
-- migration, or its process died before its first renewal. Either way
-- `aidev task recover` treats it as expired — there is no evidence anyone is
-- still working on it.
ALTER TABLE task_attempts
	ADD COLUMN lease_owner      TEXT        NOT NULL DEFAULT '',
	ADD COLUMN lease_expires_at TIMESTAMPTZ;
