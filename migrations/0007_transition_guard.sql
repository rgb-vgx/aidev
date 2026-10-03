-- The task status state machine is implemented twice: in Go
-- (internal/task/status.go, checked before every write aidev makes) and here.
--
-- This trigger is load-bearing, not a decorative backstop. Agent backends
-- inherit the operator's environment (PG* credentials included) and the agent
-- can still read aidev's config file, so a containment hole could reach psql
-- directly and issue an arbitrary UPDATE; until the sandbox work (report item
-- F) closes that path, the trigger is what makes an illegal status impossible
-- to store regardless of who issues it. A parity test
-- (TestTransitionGuardMatchesGoStateMachine) keeps the two copies in agreement.

CREATE FUNCTION tasks_transition_guard() RETURNS trigger AS $$
BEGIN
    -- Writing the same status is not a transition: UPDATE OF status fires
    -- even when the value does not change, and a no-op rewrite must not be
    -- rejected as illegal.
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;

    IF (OLD.status = 'PENDING' AND NEW.status IN ('READY', 'WAITING_APPROVAL', 'CANCELLED'))
    OR (OLD.status = 'READY' AND NEW.status IN ('RUNNING', 'WAITING_APPROVAL', 'CANCELLED'))
    OR (OLD.status = 'WAITING_APPROVAL' AND NEW.status IN ('READY', 'FAILED', 'CANCELLED'))
    OR (OLD.status = 'RUNNING' AND NEW.status IN ('VERIFYING', 'FAILED', 'CANCELLED'))
    OR (OLD.status = 'VERIFYING' AND NEW.status IN ('SUCCEEDED', 'FAILED', 'CANCELLED'))
    THEN
        RETURN NEW;
    END IF;

    -- Terminal states (SUCCEEDED, FAILED, CANCELLED) have no clause above, so
    -- any change out of them lands here too.
    RAISE EXCEPTION 'illegal task transition: % -> %', OLD.status, NEW.status
        USING HINT = 'the state machine lives in internal/task/status.go; change it there and add a migration';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tasks_transition_guard
    BEFORE UPDATE OF status ON tasks
    FOR EACH ROW EXECUTE FUNCTION tasks_transition_guard();
