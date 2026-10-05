package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"aidev/internal/event"
	"aidev/internal/task"
)

const eventColumns = `id, seq, task_id, attempt_id, type, payload, created_at`

// DefaultEventLimit bounds a history read.
const DefaultEventLimit = 200

// MaxEventLimit caps an explicit limit.
const MaxEventLimit = 1000

// AppendEvent writes one event and returns it with its assigned sequence
// number. There is deliberately no update or delete counterpart: the database
// rejects an UPDATE on this table, and the way to correct the record is to
// append.
//
// BIGSERIAL assigns seq at INSERT time, not at COMMIT time, so without a lock
// two transactions appending for the same task can commit in the opposite
// order of their sequence numbers — a reader paging with seq > last_seen would
// then never see the event that committed last but sorted first. The advisory
// lock (keyed per task, so unrelated tasks never contend) is taken in the same
// statement as the INSERT so it also holds when this method runs in
// autocommit; when the caller already opened a transaction via InTx, it is
// held until that transaction ends, which is exactly the window in which a
// cursor reader could otherwise skip an event.
func (s *Store) AppendEvent(ctx context.Context, e event.Event) (event.Event, error) {
	if !e.Type.Valid() {
		return event.Event{}, fmt.Errorf("append event: %q is not a known event type", e.Type)
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}

	// $2 is typed text by hashtext, so the insert side casts it back to uuid:
	// PostgreSQL resolves a parameter to one type for the whole statement.
	row := s.db.QueryRow(ctx, `
		WITH lock AS (
			SELECT pg_advisory_xact_lock(hashtext($2::text)::bigint)
		)
		INSERT INTO events (id, task_id, attempt_id, type, payload, created_at)
		SELECT $1, $2::uuid, $3, $4, $5, $6
		FROM lock
		RETURNING `+eventColumns,
		e.ID, e.TaskID.String(), e.AttemptID, string(e.Type), []byte(payload), e.CreatedAt)

	written, err := scanEvent(row)
	if err != nil {
		return event.Event{}, fmt.Errorf("append event %s: %w", e.Type, err)
	}
	return written, nil
}

// EventFilter narrows a history read.
type EventFilter struct {
	TaskID uuid.UUID

	// AfterSeq returns only events with a sequence strictly greater than this,
	// which is how a caller resumes a read without re-reading what it has.
	AfterSeq int64

	// Types restricts the result to these event types. Empty means all.
	Types []event.Type

	Limit int
}

// ListEvents returns a task's history in order.
func (s *Store) ListEvents(ctx context.Context, filter EventFilter) ([]event.Event, error) {
	if filter.TaskID == uuid.Nil {
		return nil, fmt.Errorf("list events: a task id is required")
	}
	limit := filter.Limit
	switch {
	case limit <= 0:
		limit = DefaultEventLimit
	case limit > MaxEventLimit:
		limit = MaxEventLimit
	}

	types := make([]string, 0, len(filter.Types))
	for _, t := range filter.Types {
		if !t.Valid() {
			return nil, fmt.Errorf("list events: %q is not a known event type", t)
		}
		types = append(types, string(t))
	}

	rows, err := s.db.Query(ctx, `
		SELECT `+eventColumns+`
		FROM events
		WHERE task_id = $1
		  AND seq > $2
		  AND (cardinality($3::text[]) = 0 OR type = ANY($3))
		ORDER BY seq
		LIMIT $4`,
		filter.TaskID, filter.AfterSeq, types, limit)
	if err != nil {
		return nil, fmt.Errorf("list events for task %s: %w", filter.TaskID, classify(err))
	}
	defer rows.Close()

	var events []event.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list events for task %s: %w", filter.TaskID, err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events for task %s: %w", filter.TaskID, classify(err))
	}
	return events, nil
}

// TestsModifiedPaths returns the changed paths that looked like the tests
// judging an attempt, as recorded by task.verification_tests_modified during
// verification. It reads that attempt's newest such event, so a future retry
// replaces the earlier report rather than appending to it. ErrNotFound when
// the attempt never recorded one — an attempt that left the tests alone.
//
// This is how a process that did not run the task answers the same question
// the in-process Outcome carries, so every surface reports the same thing.
func (s *Store) TestsModifiedPaths(ctx context.Context, taskID, attemptID uuid.UUID) ([]string, error) {
	if taskID == uuid.Nil {
		return nil, fmt.Errorf("tests modified: a task id is required")
	}
	if attemptID == uuid.Nil {
		return nil, fmt.Errorf("tests modified: an attempt id is required")
	}

	var payload []byte
	err := s.db.QueryRow(ctx, `
		SELECT payload
		FROM events
		WHERE task_id = $1 AND attempt_id = $2 AND type = $3
		ORDER BY seq DESC
		LIMIT 1`,
		taskID, attemptID, event.TypeVerificationTestsModified).Scan(&payload)
	if err != nil {
		return nil, fmt.Errorf("tests modified for attempt %s: %w", attemptID, classify(err))
	}

	var decoded struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("tests modified for attempt %s: payload is not decodable: %w", attemptID, err)
	}
	return decoded.Paths, nil
}

// ApplyState returns whether a task's verified result has been taken into the
// operator's branch, derived from the newest task.applied / task.apply_undone
// event. A task with neither is ApplyNever, which is not an error — nothing
// has happened yet, rather than something having gone wrong.
func (s *Store) ApplyState(ctx context.Context, taskID uuid.UUID) (task.Apply, error) {
	if taskID == uuid.Nil {
		return task.Apply{}, fmt.Errorf("apply state: a task id is required")
	}

	var (
		eventType string
		payload   []byte
		at        time.Time
	)
	err := s.db.QueryRow(ctx, `
		SELECT type, payload, created_at
		FROM events
		WHERE task_id = $1 AND type IN ('task.applied', 'task.apply_undone')
		ORDER BY seq DESC
		LIMIT 1`,
		taskID).Scan(&eventType, &payload, &at)
	if err != nil {
		if errors.Is(classify(err), ErrNotFound) {
			return task.Apply{}, nil
		}
		return task.Apply{}, fmt.Errorf("apply state for task %s: %w", taskID, classify(err))
	}
	return decodeApplyPayload(taskID, eventType, payload, at)
}

// decodeApply builds the derived state from one apply event's type, payload
// and timestamp. A nil type means no such event exists, so the task was never
// applied.
func decodeApply(taskID uuid.UUID, eventType *string, payload []byte, at *time.Time) (task.Apply, error) {
	if eventType == nil {
		return task.Apply{}, nil
	}
	var timestamp time.Time
	if at != nil {
		timestamp = *at
	}
	return decodeApplyPayload(taskID, *eventType, payload, timestamp)
}

// decodeApplyPayload decodes the newest apply event. The applied payload
// records into, branch, commit and whether it reapplied after an undo; the
// undone payload records into, the revert commit and what it undid.
func decodeApplyPayload(taskID uuid.UUID, eventType string, payload []byte, at time.Time) (task.Apply, error) {
	switch eventType {
	case string(event.TypeTaskApplied):
		var decoded struct {
			Into   string `json:"into"`
			Commit string `json:"commit"`
		}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return task.Apply{}, fmt.Errorf("apply state for task %s: payload is not decodable: %w", taskID, err)
		}
		return task.Apply{State: task.ApplyApplied, Into: decoded.Into, Commit: decoded.Commit, At: at}, nil
	case string(event.TypeTaskApplyUndone):
		var decoded struct {
			Into   string `json:"into"`
			Commit string `json:"commit"`
			Undid  string `json:"undid"`
		}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return task.Apply{}, fmt.Errorf("apply state for task %s: payload is not decodable: %w", taskID, err)
		}
		return task.Apply{State: task.ApplyUndone, Into: decoded.Into, Commit: decoded.Commit, Undid: decoded.Undid, At: at}, nil
	default:
		return task.Apply{}, fmt.Errorf("apply state for task %s: unexpected event type %q", taskID, eventType)
	}
}

func scanEvent(row scanner) (event.Event, error) {
	var (
		e       event.Event
		evType  string
		payload []byte
	)
	err := row.Scan(&e.ID, &e.Seq, &e.TaskID, &e.AttemptID, &evType, &payload, &e.CreatedAt)
	if err != nil {
		return event.Event{}, classify(err)
	}
	parsed, err := event.ParseType(evType)
	if err != nil {
		return event.Event{}, fmt.Errorf("event %s: %w", e.ID, err)
	}
	e.Type = parsed
	e.Payload = payload
	return e, nil
}
