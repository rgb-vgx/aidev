package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"aidev/internal/event"
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
func (s *Store) AppendEvent(ctx context.Context, e event.Event) (event.Event, error) {
	if !e.Type.Valid() {
		return event.Event{}, fmt.Errorf("append event: %q is not a known event type", e.Type)
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}

	row := s.db.QueryRow(ctx, `
		INSERT INTO events (id, task_id, attempt_id, type, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING `+eventColumns,
		e.ID, e.TaskID, e.AttemptID, string(e.Type), []byte(payload), e.CreatedAt)

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
