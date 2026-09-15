package store

import (
	"context"
	"fmt"
)

// StatsRow aggregates finished attempts by the model that ran them and the
// hardness of their task, so routing can be checked against what happened.
type StatsRow struct {
	Model        string
	Hardness     string
	Runs         int
	Succeeded    int
	Failed       int
	FailureKinds map[string]int
}

// StatsByModelAndHardness groups finished attempts by model and hardness. The
// model is the one recorded on the worker run, which is what actually ran;
// an attempt with no worker run never reached the agent and is left out by
// the inner join. Grouping happens in SQL; Go only merges the per-status
// breakdowns into rows sorted by model then hardness.
func (s *Store) StatsByModelAndHardness(ctx context.Context) ([]StatsRow, error) {
	rows, err := s.db.Query(ctx, `
		SELECT w.model, t.hardness, a.status, a.failure_kind, COUNT(*)
		FROM task_attempts a
		JOIN tasks t ON t.id = a.task_id
		JOIN worker_runs w ON w.attempt_id = a.id
		WHERE a.status <> 'RUNNING'
		GROUP BY w.model, t.hardness, a.status, a.failure_kind
		ORDER BY w.model, t.hardness`)
	if err != nil {
		return nil, fmt.Errorf("stats by model and hardness: %w", classify(err))
	}
	defer rows.Close()

	var out []StatsRow
	index := map[string]int{}
	for rows.Next() {
		var model, hardness, status, failureKind string
		var n int64
		if err := rows.Scan(&model, &hardness, &status, &failureKind, &n); err != nil {
			return nil, fmt.Errorf("stats by model and hardness: %w", classify(err))
		}
		key := model + "\x00" + hardness
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, StatsRow{
				Model:        model,
				Hardness:     hardness,
				FailureKinds: map[string]int{},
			})
		}
		out[i].Runs += int(n)
		if status == "SUCCEEDED" {
			out[i].Succeeded += int(n)
		} else {
			out[i].Failed += int(n)
		}
		if failureKind != "" {
			out[i].FailureKinds[failureKind] += int(n)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats by model and hardness: %w", classify(err))
	}
	return out, nil
}
